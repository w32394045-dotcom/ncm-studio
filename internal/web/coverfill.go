package web

import (
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"ncm-studio/internal/cover"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/tag"
)

// coverFillStates are what one file's cover lookup did.
const (
	// coverFillApplied means artwork was fetched and written into the file.
	coverFillApplied = "applied"
	// coverFillExists means the file already carried artwork and the request
	// did not ask to replace it.
	coverFillExists = "exists"
	// coverFillReview means a song was found but this program will not write it
	// unasked: the match was not certain enough, or the file does not say
	// enough to search with. It is the same door the lyrics backfill leaves
	// open, and the same reason: a wrong cover is worse than none.
	coverFillReview = "review"
	// coverFillFailed means the lookup itself broke — no song id, no artwork,
	// or a rewrite that did not finish.
	coverFillFailed = "failed"
)

// coverFillResult is one file's answer, sent back on its own.
//
// One file per request rather than a batch, because the batch this drives is
// the page's: a batch here would need its own pool, its own event stream and
// its own cancel, and the page can already stop between two files. What the
// server must not do is let two rewrites of one file race, and that is what the
// write semaphore is for.
type coverFillResult struct {
	Path    string `json:"path"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	MusicID int64  `json:"musicId,omitempty"`
	// Title is the catalogue's spelling of the song, so a row can say what it
	// matched rather than only that it did.
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	// Bytes is the artwork that was written, for a log line or a tooltip.
	Bytes int64 `json:"bytes,omitempty"`
}

// handleCoverFill gives one file the official artwork of the song it is.
//
// This is the cover page's half of what the lyrics backfill does: the files
// that were decoded before this tool wrote a song id into them, or by another
// tool entirely, carry nothing to look a cover up by — so the track is found by
// searching with the tags, and with the file's own name when the tags say
// nothing. A match is only written when the matcher is certain; anything else
// comes back as a review with the reason on it.
func (s *Server) handleCoverFill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		// Overwrite replaces artwork the file already carries. Off by default:
		// the usual request is to fill the gaps in a library, and the covers
		// that are already there are the ones somebody chose.
		Overwrite bool `json:"overwrite"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := requireFile(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	// A file the scan was told not to offer is not filled either: the minimum
	// size is about which files this program will touch, and a lookup writes
	// into the file it is asked about.
	if ok, err := s.store.MeetsMinSize(req.Path); err != nil || !ok {
		writeError(w, http.StatusBadRequest, "this file is below the minimum size in the settings")
		return
	}

	path := req.Path
	info, err := tag.Inspect(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	// A file that already has artwork is left alone unless asked: the cover in
	// it may be the only copy of that picture anywhere.
	if info.HasCover && !req.Overwrite {
		writeJSON(w, http.StatusOK, coverFillResult{
			Path:   path,
			State:  coverFillExists,
			Reason: "this file already has a cover",
		})
		return
	}

	hash, err := tag.AudioFingerprint(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	lib, err := s.libraryFor(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	lib.RememberPath(path)

	musicID, credit, result, err := s.coverFillTarget(r, path, info, lib)
	if err != nil {
		// The search is the only thing that can refuse here, and its own words
		// are the useful part — a rate limit and a login wall call for
		// different things from the reader.
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}
	if result != nil {
		writeJSON(w, http.StatusOK, *result)
		return
	}

	data, mime, err := cover.FetchOfficial(r.Context(), nil, musicID)
	if err != nil {
		writeJSON(w, http.StatusOK, coverFillResult{
			Path:    path,
			State:   coverFillFailed,
			MusicID: musicID,
			Reason:  err.Error(),
		})
		return
	}

	// The same lock the other four cover handlers take, and for the same
	// reason: it serialises this rewrite against them, so a fill running from
	// one page cannot rewrite a file while the editor on another is rewriting
	// the same one. tag.SetCover holds a per-file lock of its own, which is
	// what covers a rewrite started from somewhere else entirely — the lyrics
	// page, or a backfill batch.
	s.coverMu.Lock()
	defer s.coverMu.Unlock()
	// And the write semaphore, which bounds how many whole-file copies are in
	// flight at once: filling in a library touches files of a hundred megabytes
	// each, and doing several at once is how a phone's storage starts thrashing.
	release, ok := s.acquireWrite(r)
	if !ok {
		return
	}
	defer release()

	entry, err := lib.Add(cover.KindNetease, data, mime, "NetEase")
	if err != nil {
		writeJSON(w, http.StatusOK, coverFillResult{
			Path:    path,
			State:   coverFillFailed,
			MusicID: musicID,
			Reason:  "store the image: " + err.Error(),
		})
		return
	}
	if err := tag.SetCover(path, data, mime, nil); err != nil {
		writeJSON(w, http.StatusOK, coverFillResult{
			Path:    path,
			State:   coverFillFailed,
			MusicID: musicID,
			Reason:  "write the cover: " + err.Error(),
		})
		return
	}

	lib.SetCurrent(entry.ID)
	// The song id is recorded in the library even though it is not written into
	// the file: that would mean a second full rewrite for the tag alone, and
	// the library already answers "which song is this track" for the editor,
	// the fetcher and the next run of this.
	lib.SetMusicID(musicID)
	log.Printf("cover: %s <- song %d (%s), %d bytes",
		filepath.Base(path), musicID, credit.Title, len(data))

	writeJSON(w, http.StatusOK, coverFillResult{
		Path:    path,
		State:   coverFillApplied,
		MusicID: musicID,
		Title:   credit.Title,
		Artist:  strings.Join(credit.Artists, ", "),
		Bytes:   int64(len(data)),
	})
}

// coverFillTarget works out which song a file is, returning either a song id to
// fetch or a result that answers the request without fetching anything.
//
// The order is the one that wastes the fewest requests: an id the file already
// carries needs no search at all, and only a file that names nothing is
// searched for by name — which is the case this exists for, a track that was
// decoded before the id was written into it.
func (s *Server) coverFillTarget(r *http.Request, path string, info *tag.AudioInfo, lib *cover.Library) (int64, tag.Credit, *coverFillResult, error) {
	if id := s.resolveMusicID(lib, path); id > 0 {
		return id, tag.Credit{MusicID: id, Title: info.Title, Artists: info.Artists, Album: info.Album}, nil, nil
	}

	client := s.lyricClient()
	if client == nil {
		return 0, tag.Credit{}, nil, errors.New("no lyrics client")
	}
	if s.searcher == nil {
		// A server without a searcher is one that has no catalogue to ask —
		// a bare test harness, not a running program.
		return 0, tag.Credit{}, nil, errors.New("searching is not available")
	}
	sug, err := s.searcher.Match(r.Context(), client, info, filepath.Base(path))
	if err != nil {
		return 0, tag.Credit{}, nil, err
	}
	if !sug.Found || !sug.Auto || sug.Best <= 0 {
		reason := sug.Reason
		if strings.TrimSpace(reason) == "" {
			reason = "no search result looked like this file"
		}
		return 0, tag.Credit{}, &coverFillResult{
			Path:   path,
			State:  coverFillReview,
			Reason: reason,
		}, nil
	}

	// What the search matched, so the log line and the row can name it.
	best := lyric.Candidate{MusicID: sug.Best}
	for _, c := range sug.Candidates {
		if c.MusicID == sug.Best {
			best = lyric.Candidate{
				MusicID: c.MusicID,
				Name:    c.Name,
				Artists: c.Artists,
				Album:   c.Album,
			}
			break
		}
	}
	return sug.Best, tag.Credit{
		MusicID: sug.Best,
		Title:   best.Name,
		Artists: best.Artists,
		Album:   best.Album,
	}, nil, nil
}
