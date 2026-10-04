// Package backfill fetches lyrics for audio files that are already on disk.
//
// The decryptor writes lyrics as a side effect of decoding a .ncm file. This is
// the other half: files that were decoded before lyrics were fetched — by an
// older build, or by a different tool entirely — have nothing in their tags to
// say which NetEase track they are, so the track has to be found by searching
// and then confirmed.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/pipeline"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// ClientFor returns the client whose cache lives in a workspace. It is a
// function rather than a client because the workspace can be changed from the
// settings page while a batch is running, and a batch must not keep writing
// into a cache directory the user has moved away from.
type ClientFor func(workspace string) *lyric.Client

// Options wires the processor to the rest of the program.
type Options struct {
	Store   *store.Store
	Clients ClientFor
	// Searcher holds the per-song candidate pools. A nil one is created here,
	// which is what a test wants; the program passes the same one to the web
	// layer so that a manual search for a file whose song the batch already
	// resolved costs no request.
	Searcher *Searcher
	// Log receives problems worth recording but not worth failing a file over.
	// A nil Log discards them.
	Log func(format string, args ...any)
}

// searchLimit is how many candidates to ask for.
//
// One request now answers for every recording of a song, so the pool has to be
// wide enough to hold them all: the library this was built for has eight
// versions of one single, and a limit that cuts the family short would leave
// the scorer choosing from whatever happened to arrive first.
const searchLimit = 30

// Processor returns the job processor for a lyrics backfill batch.
func Processor(opts Options) job.Processor {
	// One pool per song, shared by every file in every batch this processor
	// runs — and with the web layer, which passes its own.
	searches := opts.Searcher
	if searches == nil {
		searches = NewSearcher()
	}

	return func(ctx context.Context, it *job.Item, report job.Reporter) (job.Result, error) {
		cfg := opts.Store.Config()

		mode := store.LyricsMode(cfg.Lyrics)
		if it.LyricsMode != nil {
			// An out-of-range override is ignored rather than obeyed: the value
			// arrives from the web API, and a switch on it would silently mean
			// "off" for a number this build does not know.
			if m := store.LyricsMode(*it.LyricsMode); m.Valid() {
				mode = m
			}
		}
		if !pipeline.LyricsMode(mode).WantsLyrics() {
			return job.Result{State: job.Skipped, Reason: "lyrics are off for this file"}, nil
		}

		client := opts.Clients(cfg.Workspace)
		if client == nil {
			return job.Result{}, errors.New("no lyrics client")
		}
		// Read per file rather than once at startup, so a change made while a
		// batch is running applies to the files it has not reached yet.
		client.SetRefresh(cfg.LyricsSource == store.LyricsSourceFetch)

		report("read", 0, 0)
		info, err := tag.Inspect(it.Path)
		if err != nil {
			return job.Result{}, fmt.Errorf("read tags: %w", err)
		}

		// A file that already names its track is taken at its word: the id is
		// only ever written after a match has been confirmed, so searching
		// again could only replace a known answer with a guess.
		musicID := info.MusicID
		credit := tag.Credit{MusicID: musicID}
		var ranked []job.Match
		if musicID > 0 && !named(info) {
			// The id says which track this is, but the file does not carry the
			// name itself — and a player that cannot name a track will not show
			// its lyrics. One search turns the id into the catalogue's own
			// spelling of the title and artists, which is the spelling any
			// online lookup will be done with.
			//
			// Nothing here can fail the file: a search that is refused, or that
			// does not offer this id, leaves the credit as the bare id and the
			// lyrics are still written.
			if q := queryFor(info, it.Name); q.Title != "" {
				if cands, err := searches.pool(ctx, client, q); err == nil {
					for _, c := range cands {
						if c.MusicID == musicID {
							credit.Title, credit.Artists, credit.Album = c.Name, c.Artists, c.Album
							break
						}
					}
				}
			}
		}
		if musicID <= 0 {
			q := queryFor(info, it.Name)
			if q.Title == "" {
				return job.Result{State: job.Review, Reason: "nothing in the name or tags to search for"}, nil
			}
			cands, err := searches.pool(ctx, client, q)
			if err != nil {
				return job.Result{}, err
			}
			d := lyric.Decide(q, cands, info.Duration, instrumentalFor(it, info))
			ranked = rank(q, cands, info.Duration)
			if !d.Found {
				return job.Result{
					State:      job.Review,
					Reason:     "no search result looked like this file",
					Candidates: ranked,
				}, nil
			}
			if !d.Auto {
				// Everything needed to answer is carried back on the item, so
				// the user's decision costs no second search.
				return job.Result{
					State:      job.Review,
					Reason:     d.Reason,
					MusicID:    d.Best.MusicID,
					Candidates: ranked,
				}, nil
			}
			musicID = d.Best.MusicID
			credit = tag.Credit{
				MusicID: musicID,
				Title:   d.Best.Name,
				Artists: d.Best.Artists,
				Album:   d.Best.Album,
			}
		}

		report("lyrics", 0, 0)
		out, err := Apply(ctx, client, info, credit, mode, func(done, total int64) {
			report("write", done, total)
		})
		if errors.Is(err, lyric.ErrNotFound) {
			// The track exists and the catalogue simply has no lyrics for it.
			// That is an answer, not a failure, and re-running must not retry it.
			return job.Result{
				State:      job.Done,
				MusicID:    musicID,
				Lyrics:     "none",
				Reason:     "this track has no lyrics",
				Candidates: ranked,
			}, nil
		}
		if err != nil {
			return job.Result{}, err
		}

		return job.Result{
			State:      job.Done,
			MusicID:    musicID,
			Lyrics:     out.Lyrics,
			LRC:        out.LRC,
			Candidates: ranked,
			Warnings:   out.Warnings,
		}, nil
	}
}

// Outcome is what applying lyrics to one file did.
//
// It goes over the wire as the answer to a confirmed match, so the field names
// are the ones the page reads rather than the ones Go would choose.
type Outcome struct {
	MusicID int64 `json:"musicId"`
	// Embedded is true when the audio file itself was rewritten. It is the
	// expensive half — the whole file is copied — and the UI says so before it
	// starts rather than after.
	Embedded bool `json:"embedded"`
	// LRC is the sidecar that was written, or "" when none was wanted or the
	// one on disk already said the same thing.
	LRC string `json:"lrc,omitempty"`
	// Lyrics is "ok" or "none".
	Lyrics   string   `json:"lyrics"`
	Warnings []string `json:"warnings,omitempty"`
}

// Apply fetches one track's lyrics and writes them to one file.
//
// It is the single write path, shared by the batch and by a match the user
// confirmed by hand, so that both honour the same mode, the same skip-what-is-
// already-there rule, and the same idea of where a sidecar goes.
func Apply(ctx context.Context, client *lyric.Client, info *tag.AudioInfo, credit tag.Credit, mode store.LyricsMode, onProgress tag.ProgressFunc) (*Outcome, error) {
	musicID := credit.MusicID
	l, err := client.Fetch(ctx, musicID)
	if err != nil {
		return nil, err
	}

	want := tag.Lyrics{
		Merged:       l.Bilingual(),
		Original:     l.Original,
		Translation:  l.Translation,
		Romanization: l.Romanization,
	}
	return ApplyTimelines(info, credit, want, mode, onProgress)
}

// ApplyTimelines writes timelines that did not come from the catalogue — a
// translation a model produced, or a timeline imported from a .lrc — through
// the same rules as a fetched one.
//
// The reasons a caller wants its own timelines are all the same shape: the
// words are already in hand, and what matters is that they land in the file the
// way every other write does, with the same skip-a-file-that-already-says-this
// check and the same sidecar rule.
func ApplyTimelines(info *tag.AudioInfo, credit tag.Credit, want tag.Lyrics, mode store.LyricsMode, onProgress tag.ProgressFunc) (*Outcome, error) {
	musicID := credit.MusicID
	if want.Empty() {
		return &Outcome{MusicID: musicID, Lyrics: "none"}, nil
	}

	out := &Outcome{MusicID: musicID, Lyrics: "ok"}
	m := pipeline.LyricsMode(mode)

	// Embedding is the expensive half — it rewrites the whole file — so it is
	// skipped when the file already says exactly this. Without the check a
	// second pass over a finished library would copy every byte of it again to
	// change nothing. The id is part of the comparison because a file can carry
	// the right lyrics and still be missing the tag that says which track it
	// is, and writing that tag is what makes the next pass free — as is a name
	// the file does not have, which is the one thing a player needs before it
	// will show the lyrics at all.
	if m.WantsEmbed() && !(info.Lyrics.Equal(want) && info.MusicID == musicID && !credit.Fills(info)) {
		if err := tag.SetLyrics(info.Path, want, credit, onProgress); err != nil {
			return nil, fmt.Errorf("write lyrics: %w", err)
		}
		out.Embedded = true
	}

	if m.WantsFile() {
		path := SidecarPath(info.Path)
		if err := writeSidecar(path, want.Merged); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("write .lrc: %v", err))
		} else {
			out.LRC = path
		}
	}
	return out, nil
}

// named reports whether a file already says what it is. The album is not
// consulted: plenty of tracks have no album, and one that does is not thereby
// nameless. A player looks a track up by its title and artist.
func named(info *tag.AudioInfo) bool {
	return strings.TrimSpace(info.Title) != "" || len(info.Artists) > 0
}

// queryFor builds the search from the tags where they say something and from
// the file name where they do not. The tags win because the name is a
// convention this device follows, not a guarantee.
func queryFor(info *tag.AudioInfo, name string) lyric.Query {
	q := lyric.Query{Title: info.Title, Album: info.Album, Artists: info.Artists}
	if strings.TrimSpace(q.Title) != "" {
		return q
	}
	fromName := lyric.ParseName(name)
	q.Title = fromName.Title
	if len(q.Artists) == 0 {
		q.Artists = fromName.Artists
	}
	return q
}

// instrumentalFor reports whether this file looks like a backing track. The
// album is deliberately not consulted: a search response seen on this device
// carried an album called "…(Instrumental)" for a song that was not one, and
// treating that as evidence would refuse to write correct lyrics.
func instrumentalFor(it *job.Item, info *tag.AudioInfo) bool {
	return lyric.LooksInstrumental(it.Name, info.Title)
}

// Searcher holds one candidate pool per song, so that a batch holding eight
// recordings of one single asks the catalogue about that single once.
//
// It lives for the life of the program rather than for a run because the answer
// for a song does not change between runs: the next batch over the same library
// costs no searches at all for the songs this one resolved. The mutex is held
// across the request, which serialises lookups — they are network-bound and the
// client paces its requests to one at a time anyway, so nothing is lost, and
// two workers asking about the same song cannot both go to the endpoint.
type Searcher struct {
	mu sync.Mutex
	m  map[string][]lyric.Candidate
}

// NewSearcher returns an empty Searcher.
func NewSearcher() *Searcher { return &Searcher{} }

func (s *Searcher) pool(ctx context.Context, client *lyric.Client, q lyric.Query) ([]lyric.Candidate, error) {
	key := lyric.GroupKey(q)

	s.mu.Lock()
	defer s.mu.Unlock()
	if cands, ok := s.m[key]; ok {
		return cands, nil
	}
	cands, err := search(ctx, client, q)
	if err != nil {
		// Not remembered: a refusal is a property of the moment, and the next
		// file in the batch may arrive after the cooldown has passed.
		return nil, err
	}
	if s.m == nil {
		s.m = make(map[string][]lyric.Candidate)
	}
	s.m[key] = cands
	return cands, nil
}

// Suggestion is what a search for one file turned up: the ranked list to show,
// and the decision the batch would have made from it.
type Suggestion struct {
	Candidates []job.Match `json:"candidates,omitempty"`
	Best       int64       `json:"musicId,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Found      bool        `json:"found"`
	Auto       bool        `json:"auto"`
}

// Match searches for one file and ranks what came back.
//
// This is the manual half of the decision the batch makes: a file parked for
// review can be asked about again, and a user who would rather choose for
// themselves gets the same list the matcher saw. A song the batch already
// resolved is answered from the pool it left behind, at no request.
func (s *Searcher) Match(ctx context.Context, client *lyric.Client, info *tag.AudioInfo, name string) (Suggestion, error) {
	q := queryFor(info, name)
	if strings.TrimSpace(q.Title) == "" {
		return Suggestion{Reason: "nothing in the name or tags to search for"}, nil
	}
	instrumental := lyric.LooksInstrumental(name, info.Title)
	cands, err := s.pool(ctx, client, q)
	if err != nil {
		return Suggestion{}, err
	}
	d := lyric.Decide(q, cands, info.Duration, instrumental)
	return Suggestion{
		Candidates: rank(q, cands, info.Duration),
		Best:       d.Best.MusicID,
		Reason:     d.Reason,
		Found:      d.Found,
		Auto:       d.Auto,
	}, nil
}

// search fetches the candidate pool for a song.
//
// The keyword is the version-stripped title, so one request answers for every
// recording of it. Both forms are tried when the first finds nothing at all,
// and the second is never skipped because an early candidate looked good — the
// pool a file is scored against must not depend on which file in the group
// happened to be processed first.
func search(ctx context.Context, client *lyric.Client, q lyric.Query) ([]lyric.Candidate, error) {
	var (
		all     []lyric.Candidate
		seen    = map[int64]bool{}
		lastErr error
	)
	for _, keyword := range q.GroupQueries() {
		cands, err := client.Search(ctx, keyword, searchLimit)
		if err != nil {
			if errors.Is(err, lyric.ErrSearchUnavailable) {
				// The endpoint is refusing to answer at all. Trying the next
				// form would be a second request into the same refusal.
				return nil, err
			}
			lastErr = err
			continue
		}
		for _, c := range cands {
			if !seen[c.MusicID] {
				seen[c.MusicID] = true
				all = append(all, c)
			}
		}
		if len(all) > 0 {
			break
		}
	}
	if len(all) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return all, nil
}

func rank(q lyric.Query, cands []lyric.Candidate, dur time.Duration) []job.Match {
	scored := make([]lyric.Scored, 0, len(cands))
	for _, c := range cands {
		s := lyric.Score(q, c, dur)
		if s.TitleScore == 0 {
			continue // a different song; offering it would be noise
		}
		scored = append(scored, s)
	}
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].Score > scored[j-1].Score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	out := make([]job.Match, 0, len(scored))
	for _, s := range scored {
		out = append(out, job.Match{
			MusicID:    s.MusicID,
			Name:       s.Name,
			Artists:    s.Artists,
			Album:      s.Album,
			DurationMS: s.DurationMS,
			Score:      s.Score,
		})
	}
	return out
}

// SidecarPath is where a file's .lrc goes: beside it, same stem. The decryptor
// uses the same rule, so a backfilled track and a freshly decrypted one are
// indistinguishable to a player.
func SidecarPath(audio string) string {
	return strings.TrimSuffix(audio, filepath.Ext(audio)) + ".lrc"
}

// writeSidecar writes the .lrc, leaving an identical one alone.
//
// Only the merged timeline goes in: players expect the original text here and
// pick a translation from their own settings.
func writeSidecar(path, body string) error {
	if body == "" {
		return nil
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == body {
		return nil
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
