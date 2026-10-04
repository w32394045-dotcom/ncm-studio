package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"ncm-studio/internal/backfill"
	"ncm-studio/internal/job"
	"ncm-studio/internal/tag"
)

// handleLRCState answers for the .lrc pool; see poolState.
func (s *Server) handleLRCState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, stateOf(s.lrc))
}

func (s *Server) handleLRCEvents(w http.ResponseWriter, r *http.Request) {
	s.streamPool(w, r, s.lrc)
}

// handleLRCStart queues one direction of the .lrc page.
//
// The direction is the batch's, not the file's: a user who has just exported a
// folder is about to edit those .lrc files and import them back, and mixing the
// two directions in one run would make the progress bar mean two different
// things at once.
func (s *Server) handleLRCStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
		Op    string   `json:"op"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.Op != backfill.LRCExport && req.Op != backfill.LRCImport {
		writeError(w, http.StatusBadRequest, "op must be %q or %q", backfill.LRCExport, backfill.LRCImport)
		return
	}

	items := queuePaths(req.Paths)
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "no files selected")
		return
	}
	for _, it := range items {
		it.Op = req.Op
	}
	s.lrc.Start(items)
	log.Printf("started a .lrc %s batch of %d file(s)", req.Op, len(items))
	writeJSON(w, http.StatusOK, map[string]int{"queued": len(items)})
}

func (s *Server) handleLRCCancel(w http.ResponseWriter, r *http.Request) {
	s.lrc.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

// lyricsView is everything the viewer shows for one file: what its tags say,
// what the .lrc says, and where that .lrc is.
type lyricsView struct {
	Path string `json:"path"`
	Name string `json:"name"`

	Merged       string `json:"merged,omitempty"`
	Original     string `json:"original,omitempty"`
	Translation  string `json:"translation,omitempty"`
	Romanization string `json:"romanization,omitempty"`
	// Source and Model name the model that wrote the translation, when one
	// did. Empty means a person did.
	Source string `json:"translationSource,omitempty"`
	Model  string `json:"translationModel,omitempty"`

	// LRC is the sidecar's body and LRCPath where it is, when there is one.
	LRC     string `json:"lrc,omitempty"`
	LRCPath string `json:"lrcPath,omitempty"`

	MusicID int64 `json:"musicId,omitempty"`

	// Candidates are what a search turns up, when the caller asked for one.
	Candidates []job.Match `json:"candidates,omitempty"`
}

// handleLyricsView reads one file's lyrics for the viewer.
//
// The .lrc page, the translation page and the info page all want the same
// thing — what does this file actually say — and reading it in one place means
// they cannot disagree about it.
func (s *Server) handleLyricsView(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		writeError(w, http.StatusBadRequest, "no path")
		return
	}

	out := lyricsView{Path: path, Name: filepath.Base(path)}
	info, err := tag.Inspect(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	out.MusicID = info.MusicID
	out.Merged = info.Lyrics.Merged
	out.Original = info.Lyrics.Original
	out.Translation = info.Lyrics.Translation
	out.Romanization = info.Lyrics.Romanization
	out.Source = info.Lyrics.TranslationSource
	out.Model = info.Lyrics.TranslationModel

	if data, err := os.ReadFile(backfill.SidecarPath(path)); err == nil {
		out.LRC = string(data)
		out.LRCPath = backfill.SidecarPath(path)
	}
	writeJSON(w, http.StatusOK, out)
}
