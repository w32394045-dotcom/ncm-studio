package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ncm-studio/internal/backfill"
	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// inspectWorkers is how many files the scan reads at once.
//
// A metadata read is one short read per file, but a phone's storage makes each
// of those expensive: five hundred of them in a row takes seconds, and five
// hundred at once thrashes. Eight is enough to hide the latency without
// competing with the playback the user is probably also doing.
const inspectWorkers = 8

// audioExts are the containers the scan lists. Only the first two can be
// rewritten here; the rest are listed anyway, marked unwritable, because a file
// that silently vanishes from a list is harder to understand than one that says
// why it cannot be worked on.
var audioExts = map[string]bool{
	".flac": true,
	".mp3":  true,
	".m4a":  true,
	".aac":  true,
	".ogg":  true,
	".opus": true,
	".wav":  true,
}

// audioEntry is one row of the scan, shared by the lyrics and cover pages.
type audioEntry struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`

	// Format is the container as this program reads it, and Writable says
	// whether it is one this program can write back to.
	Format   string `json:"format"`
	Writable bool   `json:"writable"`

	Title      string   `json:"title,omitempty"`
	Artists    []string `json:"artists,omitempty"`
	Album      string   `json:"album,omitempty"`
	MusicID    int64    `json:"musicId,omitempty"`
	DurationMS int64    `json:"durationMs,omitempty"`

	// The platform's identifiers, for the info page: they are what the track
	// can be looked up by elsewhere, and the only trace of where a file came
	// from once it has been renamed.
	AlbumID   int64   `json:"albumId,omitempty"`
	ArtistIDs []int64 `json:"artistIds,omitempty"`
	Bitrate   int     `json:"bitrate,omitempty"`

	// HasLyrics is what the file already carries. The list uses it to offer
	// "skip what is done" without reading anything twice.
	HasLyrics bool `json:"hasLyrics"`
	HasCover  bool `json:"hasCover"`
	// HasTranslation says the file carries a translation timeline, and
	// TranslationSource who wrote it — "" for a person, or a provider id for a
	// model. The AI page reads it to offer a re-run rather than a first run.
	HasTranslation    bool   `json:"hasTranslation,omitempty"`
	TranslationSource string `json:"translationSource,omitempty"`
	TranslationModel  string `json:"translationModel,omitempty"`
	// HasLRC is a .lrc anywhere the import would find one, and LRCPath is
	// where that is — or where the next export would put it, when there is
	// none yet. The .lrc page shows both: it lists what can be imported, and
	// it says where an export is going.
	HasLRC  bool   `json:"hasLrc,omitempty"`
	LRCPath string `json:"lrcPath,omitempty"`
	// LooksInstrumental is a hint from the name, not a fact about the audio:
	// it marks the row for a person to look at, and the matcher refuses to
	// write a backing track unasked.
	LooksInstrumental bool `json:"looksInstrumental,omitempty"`
	// Skipped is why this file is listed but out of reach, or "" when it is
	// not: today only the size filter, which the settings decide. The row stays
	// in the list so that a file cannot vanish without explanation; it is
	// marked unwritable, so every batch leaves it alone.
	Skipped string `json:"skipped,omitempty"`
}

// handleAudio lists the audio files under a directory with everything both the
// lyrics page and the cover page needs to draw a row.
//
// The size filter in the settings is applied to the rows, not to the listing:
// a file under the minimum is still shown, marked as skipped and impossible to
// select. Hiding it outright would make a folder's count disagree with what a
// file manager shows, and give no clue about where the missing track went.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = s.store.Config().Dir
	}
	paths, err := scanAudio(dir, r.URL.Query().Get("recursive") == "1")
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	// The .lrc folder is part of what a file "has": a scan that only looked
	// beside the audio would say a file has no .lrc while the import is about
	// to find one in the folder the user keeps them in.
	out := inspectAll(paths, s.store.Config().LCROutput)
	applyMinSize(out, s.store.ScanFilter(), statted)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// applyMinSize marks the rows the size filter would leave out.
//
// It runs after the metadata read rather than before, because the row is the
// thing being marked: a file that was filtered out still shows its title and
// duration, which is what tells the user whether the minimum they chose is the
// one they meant.
//
// A file whose size could not be read — it went away between the directory walk
// and the stat, or its metadata is unreadable — keeps whatever the read gave
// it. It is not reported as "too small": a filter with no size to compare
// against has no opinion, and saying otherwise would send the user looking for
// a setting that is not the problem.
func applyMinSize(entries []audioEntry, filter store.ScanFilter, sizeKnown func(audioEntry) bool) {
	for i := range entries {
		if !sizeKnown(entries[i]) {
			continue
		}
		if ok, reason := filter(entries[i].Path, entries[i].Size); !ok {
			entries[i].Skipped = reason
			entries[i].Writable = false
		}
	}
}

// statted reports whether a row carries a size the filter can be applied to.
// A file of zero bytes is a real answer — an empty file is exactly what the
// filter is for — so the ModTime the same stat call set is what tells the two
// apart.
func statted(e audioEntry) bool { return e.ModTime != "" }

// scanAudio lists the files under dir this program might be able to write to.
// Unreadable subdirectories are skipped rather than failing the scan, which is
// what walking a phone's storage with its permission-restricted corners needs.
func scanAudio(dir string, recursive bool) ([]string, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if recursive {
				if sub, err := scanAudio(full, true); err == nil {
					out = append(out, sub...)
				}
			}
			continue
		}
		if audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, full)
		}
	}
	return out, nil
}

// inspectAll reads the metadata for every path, in parallel, keeping the order.
func inspectAll(paths []string, lrcDir string) []audioEntry {
	out := make([]audioEntry, len(paths))
	if len(paths) == 0 {
		return out
	}

	work := make(chan int)
	var wg sync.WaitGroup
	for range inspectWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				out[i] = inspectOne(paths[i], lrcDir)
			}
		}()
	}
	for i := range paths {
		work <- i
	}
	close(work)
	wg.Wait()
	return out
}

func inspectOne(path, lrcDir string) audioEntry {
	e := audioEntry{Path: path, Name: filepath.Base(path)}
	if fi, err := os.Stat(path); err == nil {
		e.Size = fi.Size()
		e.ModTime = fi.ModTime().Format(time.RFC3339)
	}
	// The size filter is deliberately not applied here: the row's own metadata
	// is read either way, and whether it is offered is decided once, on the
	// assembled list, so the listing and the server's own scans cannot disagree
	// about which files the minimum covers.

	info, err := tag.Inspect(path)
	if err != nil {
		// Either unreadable or a container this program cannot rewrite. The row
		// still exists so the user can see the file was considered; it just
		// cannot be selected for a write.
		e.Format = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		return e
	}

	e.Format = string(info.Format)
	e.Writable = true
	e.Title = info.Title
	e.Artists = info.Artists
	e.Album = info.Album
	e.MusicID = info.MusicID
	e.AlbumID = info.AlbumID
	e.ArtistIDs = info.ArtistIDs
	e.Bitrate = info.Bitrate
	e.DurationMS = info.Duration.Milliseconds()
	e.HasLyrics = info.HasLyrics()
	e.HasCover = info.HasCover
	e.LooksInstrumental = lyric.LooksInstrumental(e.Name, info.Title)
	e.HasTranslation = strings.TrimSpace(info.Lyrics.Translation) != ""
	e.TranslationSource = info.Lyrics.TranslationSource
	e.TranslationModel = info.Lyrics.TranslationModel
	// Where the .lrc is, or where the next export would put it. The two are the
	// same question asked at different times, and the .lrc page shows both: a
	// file with one says so, and a file without one shows where it would land.
	e.LRCPath = backfill.LRCPath(info, lrcDir)
	if _, ok := backfill.FindLRC(info, lrcDir); ok {
		e.HasLRC = true
	}
	return e
}

// handleLyricsState answers for the backfill pool; see poolState.
func (s *Server) handleLyricsState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, stateOf(s.lyrics))
}

// handleLyricsStart queues a backfill batch.
//
// Each path may carry its own landing mode, because the list lets a user
// override one row without touching the setting that governs the rest.
func (s *Server) handleLyricsStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string       `json:"paths"`
		Modes map[string]int `json:"modes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "no files selected")
		return
	}

	items := make([]*job.Item, 0, len(req.Paths))
	for _, p := range req.Paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		items = append(items, &job.Item{ID: abs, Path: abs, Name: filepath.Base(abs)})
	}
	applyModes(items, req.Modes)
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "no usable paths")
		return
	}

	s.lyrics.Start(items)
	log.Printf("started a lyrics batch of %d file(s)", len(items))
	writeJSON(w, http.StatusOK, map[string]int{"queued": len(items)})
}

func (s *Server) handleLyricsCancel(w http.ResponseWriter, r *http.Request) {
	s.lyrics.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

// handleLyricsMatch searches for one file, for a review row's "look again" or
// for a user who would rather pick from the list themselves.
func (s *Server) handleLyricsMatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "no path")
		return
	}

	info, err := tag.Inspect(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	sug, err := s.searcher.Match(r.Context(), s.lyricClient(), info, filepath.Base(req.Path))
	if err != nil {
		// Reported as a bad gateway rather than a server fault: the failure is
		// the catalogue not answering, and the message says which way.
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, sug)
}

// handleLyricsApply writes the candidate a person chose.
//
// The write is bounded by a semaphore because embedding copies the whole file:
// a phone asked to rewrite several hundred megabytes of FLAC at once will stall
// everything else, including the browser waiting on this response.
func (s *Server) handleLyricsApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		MusicID int64  `json:"musicId"`
		// The candidate's own spelling of the song, sent back by the page the
		// user confirmed it on. It is what a file with no tags of its own gets
		// named with; see tag.Credit. Only ever written into fields the file
		// leaves empty, so a page cannot overwrite the user's own tags.
		Name    string   `json:"name"`
		Artists []string `json:"artists"`
		Album   string   `json:"album"`
		Mode    *int     `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" || req.MusicID <= 0 {
		writeError(w, http.StatusBadRequest, "a path and a music id are required")
		return
	}

	mode := store.LyricsMode(s.store.Config().Lyrics)
	if req.Mode != nil {
		if m := store.LyricsMode(*req.Mode); m.Valid() {
			mode = m
		}
	}

	info, err := tag.Inspect(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	release, ok := s.acquireWrite(r)
	if !ok {
		return
	}
	defer release()

	credit := tag.Credit{
		MusicID: req.MusicID,
		Title:   req.Name,
		Artists: req.Artists,
		Album:   req.Album,
	}
	out, err := backfill.Apply(r.Context(), s.lyricClient(), info, credit, mode, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}

	// The row is updated through the pool so that every open page sees the same
	// thing, and so that a batch running alongside does not overwrite the
	// decision when it next touches the item.
	abs, _ := filepath.Abs(req.Path)
	s.lyrics.Mutate(abs, func(it *job.Item) {
		it.MusicID = req.MusicID
		it.Lyrics = out.Lyrics
		it.LRC = out.LRC
		it.Reason = ""
		it.Candidates = nil
		it.State = job.Done
		if len(out.Warnings) > 0 {
			it.Warn = out.Warnings[0]
		}
	})

	writeJSON(w, http.StatusOK, out)
}

// applyModes hangs each file's landing override on its item.
//
// An override out of range is dropped rather than clamped: the batch default is
// a real choice, and silently turning a typo into one of the modes would write
// a file the user did not ask for. Paths the batch did not queue are ignored,
// so a stale page's map cannot affect anything.
func applyModes(items []*job.Item, modes map[string]int) {
	for _, it := range items {
		mode, ok := modes[it.Path]
		if !ok {
			continue
		}
		if m := store.LyricsMode(mode); m.Valid() {
			it.LyricsMode = &mode
		}
	}
}

// acquireWrite takes a slot in the write semaphore, reporting false when the
// request went away while waiting.
func (s *Server) acquireWrite(r *http.Request) (func(), bool) {
	select {
	case s.writeSem <- struct{}{}:
		return func() { <-s.writeSem }, true
	case <-r.Context().Done():
		return nil, false
	}
}

// lyricClient returns the client for the workspace configured right now, which
// is what keeps a workspace change from writing into a cache the user moved
// away from.
func (s *Server) lyricClient() *lyric.Client {
	if s.clients == nil {
		return nil
	}
	cfg := s.store.Config()
	c := s.clients(cfg.Workspace)
	// A lookup the user asked for by hand is answered from the catalogue when
	// the setting says to ask again, exactly as a batch's would be.
	if c != nil {
		c.SetRefresh(cfg.LyricsSource == store.LyricsSourceFetch)
	}
	return c
}
