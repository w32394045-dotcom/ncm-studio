package backfill

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ncm-studio/internal/ai"
	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/pipeline"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// AIOptions wires the translation processor to the settings.
type AIOptions struct {
	Store *store.Store
	// NewClient builds the provider client. A nil one calls the vendor; a test
	// supplies its own.
	NewClient func(ai.Options) (*ai.Client, error)
}

// AIProcessor returns the job processor for an AI translation batch.
//
// One file is one request. A translation is per-song work — the model has to
// see the whole lyric to keep a chorus consistent with its verses — so there is
// nothing to batch, and the pool's concurrency is what makes a folder finish in
// reasonable time.
func AIProcessor(opts AIOptions) job.Processor {
	build := opts.NewClient
	if build == nil {
		build = ai.New
	}

	return func(ctx context.Context, it *job.Item, report job.Reporter) (job.Result, error) {
		cfg := opts.Store.Config()

		mode := store.LyricsMode(cfg.Lyrics)
		if !pipeline.LyricsMode(mode).WantsLyrics() {
			return job.Result{State: job.Skipped, Reason: "lyrics are off in the settings, so there is nowhere to put a translation"}, nil
		}
		target := strings.TrimSpace(cfg.AITarget)
		if target == "" {
			return job.Result{State: job.Skipped, Reason: "no target language is set"}, nil
		}

		report("read", 0, 0)
		info, err := tag.Inspect(it.Path)
		if err != nil {
			return job.Result{}, fmt.Errorf("read tags: %w", err)
		}

		source, from := TranslationSource(info)
		if strings.TrimSpace(source) == "" {
			return job.Result{State: job.Skipped, Reason: "this file has no lyrics to translate"}, nil
		}
		if info.Lyrics.MachineTranslated() && !it.Force && !cfg.AITranslated.Redoes() {
			// Asking a model to translate a translation is how a file ends up
			// holding English rendered into Chinese and back. Re-running is
			// deliberate, so it is possible — but it has to be asked for, either
			// for one batch or as the standing setting.
			return job.Result{State: job.Skipped, Reason: "this file already carries a machine translation"}, nil
		}

		provider := cfg.AIProvider
		if strings.TrimSpace(provider) == "" {
			provider = "deepseek"
		}
		key := opts.Store.AIKey(provider)
		if key == "" {
			return job.Result{State: job.Skipped, Reason: fmt.Sprintf("no API key is stored for %s", provider)}, nil
		}

		client, err := build(ai.Options{
			Provider: provider,
			Model:    cfg.AIModel,
			Key:      key,
			Target:   target,
			// Only a provider that takes thinking reads this; the rest are sent
			// nothing, which is why one setting can govern all of them and
			// survive a provider change.
			Thinking: cfg.AIThinking,
		})
		if err != nil {
			return job.Result{}, err
		}

		// Reported before the request rather than during it: there is no
		// progress inside a single HTTP call, and a row that says "translating"
		// for a minute is more honest than a bar that does not move.
		report("ai", 0, 0)
		res, err := client.Translate(ctx, source)
		if err != nil {
			return job.Result{}, err
		}

		want := tag.Lyrics{
			Original: strings.TrimSpace(sourceOf(info, from)),
			// Who wrote it, and with what. Both are part of the write: a file
			// that carries a translation should say it is not a person's.
			Translation:       res.Translation,
			TranslationSource: res.Provider,
			TranslationModel:  res.Model,
		}
		want.Merged = lyric.Merge(want.Original, want.Translation)
		if strings.TrimSpace(want.Merged) == "" {
			want.Merged = want.Translation
		}

		report("write", 0, 0)
		out, err := ApplyTimelines(info, tag.Credit{
			MusicID: info.MusicID,
			Title:   info.Title,
			Artists: info.Artists,
			Album:   info.Album,
		}, want, mode, func(done, total int64) {
			report("write", done, total)
		})
		if err != nil {
			return job.Result{}, err
		}

		return job.Result{
			Lyrics:   out.Lyrics,
			LRC:      out.LRC,
			Warnings: out.Warnings,
			Reason:   "translated into " + target + " by " + res.Provider + " (" + res.Model + ")",
		}, nil
	}
}

// TranslationSource picks the timeline to translate: what the file carries, or
// the .lrc beside it when the tags hold no words.
//
// The original is preferred over the merged timeline because the merged one may
// already hold a translation — sending it would ask the model to translate its
// own earlier output.
//
// It is exported because the page shows the user what would be sent before it
// spends a request, and the two must agree about which text that is.
func TranslationSource(info *tag.AudioInfo) (doc, from string) {
	if s := strings.TrimSpace(info.Lyrics.Original); s != "" {
		return s, "tags"
	}
	if s := strings.TrimSpace(info.Lyrics.Merged); s != "" {
		return s, "tags-merged"
	}
	if data, err := os.ReadFile(SidecarPath(info.Path)); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s, "lrc"
		}
	}
	return "", ""
}

// sourceOf is what the file's own words are, once the .lrc has been read in.
//
// A translation that came from a sidecar is written back with the words it
// translated, so the file ends up carrying both halves rather than a translation
// of a text it does not have.
func sourceOf(info *tag.AudioInfo, from string) string {
	if from == "tags" {
		return info.Lyrics.Original
	}
	if from == "tags-merged" {
		return info.Lyrics.Merged
	}
	// The sidecar's words; read again rather than carried through the call.
	if data, err := os.ReadFile(SidecarPath(info.Path)); err == nil {
		return string(data)
	}
	return info.Lyrics.Original
}
