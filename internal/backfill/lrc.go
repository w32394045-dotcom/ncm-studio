package backfill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// The two directions the .lrc page works in.
const (
	// LRCExport takes the lyrics a file already carries and writes them out as
	// a .lrc beside it — or into the folder the settings name.
	LRCExport = "export"
	// LRCImport takes a .lrc and writes it into the audio file's tags. It is
	// the direction that rewrites the audio, so it is also the one the conflict
	// policy protects.
	LRCImport = "import"
)

// LRCOptions wires the .lrc processor to the settings.
type LRCOptions struct {
	Store *store.Store
}

// LRCProcessor returns the job processor for the .lrc page.
//
// Both directions share one pool and one list because they are two halves of
// the same job — a file's lyrics moving between its tags and a file beside it —
// and a user who has just exported a folder is very likely to import the
// corrected result back. Starting one cancelling the other is the behaviour the
// page shows on its own button, which is what a person expects from a single
// progress bar.
func LRCProcessor(opts LRCOptions) job.Processor {
	return func(ctx context.Context, it *job.Item, report job.Reporter) (job.Result, error) {
		cfg := opts.Store.Config()

		report("read", 0, 0)
		// Inspect refuses anything whose tags this program cannot rewrite, so a
		// file that reads back at all is one both directions can work on.
		info, err := tag.Inspect(it.Path)
		if err != nil {
			return job.Result{}, fmt.Errorf("read tags: %w", err)
		}

		if it.Op == LRCImport {
			return lrcImport(ctx, info, cfg, report)
		}
		return lrcExport(info, cfg)
	}
}

// lrcExport writes out what the file already says.
//
// Nothing is fetched and nothing is guessed: a file with no lyrics in its tags
// is reported as having none, because inventing a .lrc from the file name would
// produce a file that looks like lyrics and is not.
func lrcExport(info *tag.AudioInfo, cfg store.Config) (job.Result, error) {
	body := strings.TrimSpace(info.Lyrics.Merged)
	if body == "" {
		body = strings.TrimSpace(info.Lyrics.Original)
	}
	if body == "" {
		return job.Result{State: job.Skipped, Reason: "this file has no lyrics to write out"}, nil
	}

	target := LRCPath(info, cfg.LCROutput)
	if existing, err := os.ReadFile(target); err == nil {
		have := string(existing)
		switch {
		case have == body || lyric.Contains(have, body):
			// Already there, line for line. Writing it again would only change
			// the file's modification time.
			return job.Result{LRC: target, Lyrics: "ok"}, nil
		case cfg.LRCConflict == store.ConflictSkip:
			return job.Result{
				State:  job.Skipped,
				Reason: "a .lrc is already there and it was not to be replaced",
				LRC:    target,
			}, nil
		case cfg.LRCConflict == store.ConflictMerge:
			// Both are the user's: keep the lines that are only in the .lrc.
			body = strings.TrimSpace(lyric.Combine(have, body))
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return job.Result{}, err
	}
	if err := os.WriteFile(target, []byte(body+"\n"), 0o644); err != nil {
		return job.Result{}, fmt.Errorf("write .lrc: %w", err)
	}
	return job.Result{LRC: target, Lyrics: "ok"}, nil
}

// lrcImport writes a .lrc into the audio file's tags.
func lrcImport(ctx context.Context, info *tag.AudioInfo, cfg store.Config, report job.Reporter) (job.Result, error) {
	src, body, err := readLRC(info, cfg.LCROutput)
	if err != nil {
		return job.Result{State: job.Skipped, Reason: "no .lrc beside this file to read"}, nil
	}
	if strings.TrimSpace(body) == "" || lyric.IsEmpty(body) {
		return job.Result{State: job.Skipped, Reason: "the .lrc holds no timed lines", LRC: src}, nil
	}

	have := info.Lyrics
	want := have
	switch {
	case lyric.Contains(have.Merged, body):
		// The file already says everything the .lrc says. This is what makes a
		// second import over the same folder free rather than a second copy of
		// every file.
		return job.Result{LRC: src, Lyrics: "ok"}, nil
	case !info.HasLyrics():
		// Nothing to conflict with.
		want.Merged, want.Original = strings.TrimSpace(body), strings.TrimSpace(body)
	case cfg.LRCConflict == store.ConflictSkip:
		return job.Result{
			State:  job.Skipped,
			Reason: "this file already has lyrics and it was not to be replaced",
			LRC:    src,
		}, nil
	case cfg.LRCConflict == store.ConflictMerge:
		want.Merged = lyric.Combine(have.Merged, body)
		want.Original = lyric.Combine(originalOf(have), body)
	default:
		// Overwrite: the .lrc becomes the file's lyrics. The translation and
		// romanisation keys are deliberately left alone — they are separate
		// timelines, and an import replaces the words, not the work.
		want.Merged, want.Original = strings.TrimSpace(body), strings.TrimSpace(body)
	}
	want.Merged = strings.TrimSpace(want.Merged)
	want.Original = strings.TrimSpace(want.Original)

	// The credit is the file's own identity, which fills nothing: this is a
	// write of words the user supplied, and nothing here was matched against a
	// catalogue.
	credit := tag.Credit{
		MusicID: info.MusicID,
		Title:   info.Title,
		Artists: info.Artists,
		Album:   info.Album,
	}
	report("write", 0, 0)
	out, err := ApplyTimelines(info, credit, want, store.LyricsEmbed, func(done, total int64) {
		report("write", done, total)
	})
	if err != nil {
		return job.Result{}, err
	}
	if !out.Embedded {
		// Nothing to do: the tags already say this, so the file was not copied
		// again. Reported as done rather than skipped because the outcome the
		// user asked for is true either way.
		return job.Result{LRC: src, Lyrics: "ok"}, nil
	}
	return job.Result{LRC: src, Lyrics: "ok"}, nil
}

// originalOf is the file's original timeline, falling back to the merged one
// when the file only ever carried the merged form. Most files do.
func originalOf(l tag.Lyrics) string {
	if strings.TrimSpace(l.Original) != "" {
		return l.Original
	}
	return l.Merged
}

// LRCPath is where a file's exported .lrc goes.
//
// With no folder configured it is the file's own name beside it, which is the
// layout every player looks for. In a folder that collects songs from several
// albums, the name is built from the track's own title and artist when it has
// them: a flat folder of files that all begin "01 - " would otherwise give
// every track of an album the same name, and the last one written would be the
// only one left.
func LRCPath(info *tag.AudioInfo, dir string) string {
	if strings.TrimSpace(dir) == "" {
		return SidecarPath(info.Path)
	}
	name := strings.TrimSpace(info.Title)
	if name == "" {
		return filepath.Join(dir, strings.TrimSuffix(filepath.Base(info.Path), filepath.Ext(info.Path))+".lrc")
	}
	if len(info.Artists) > 0 {
		name = strings.Join(info.Artists, ", ") + " - " + name
	}
	return filepath.Join(dir, safeFileName(name)+".lrc")
}

// FindLRC reports where a file's .lrc is, when it has one anywhere the import
// would look: beside the audio file first, because that is the one a player
// would have found too, then in the folder the settings collect them in.
//
// It is exported so a list can say whether a file has a .lrc from the same
// answer the import will act on, rather than a second opinion that can differ.
func FindLRC(info *tag.AudioInfo, dir string) (string, bool) {
	for _, p := range lrcCandidates(info, dir) {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// lrcCandidates is every path a file's .lrc could be at, in the order the
// import prefers them.
func lrcCandidates(info *tag.AudioInfo, dir string) []string {
	side := SidecarPath(info.Path)
	out := []string{side}
	if custom := LRCPath(info, dir); custom != side {
		out = append(out, custom)
	}
	return out
}

// readLRC finds the .lrc for a file and returns its path and text.
func readLRC(info *tag.AudioInfo, dir string) (string, string, error) {
	src, ok := FindLRC(info, dir)
	if !ok {
		return "", "", os.ErrNotExist
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", "", err
	}
	return src, string(data), nil
}

// safeFileName strips what a file name cannot hold. A title is free text — it
// may contain a slash, or a name the filesystem reserves — and a write that
// fails on one track should not take the batch down with it.
func safeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		return "lyrics"
	}
	if len(s) > 150 {
		s = strings.TrimSpace(s[:150])
	}
	return s
}
