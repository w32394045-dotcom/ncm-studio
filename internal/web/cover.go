package web

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"ncm-studio/internal/cover"
	"ncm-studio/internal/tag"
)

// maxCoverUpload caps an uploaded image. Album art is a few hundred kilobytes;
// this is generous enough for anything real and small enough that a mistake
// cannot fill the workspace.
const maxCoverUpload = 20 << 20

// coverManifest is what the cover editor renders itself from.
type coverManifest struct {
	Path      string        `json:"path"`
	Name      string        `json:"name"`
	AudioHash string        `json:"audioHash"`
	HasCover  bool          `json:"hasCover"`
	Current   string        `json:"current"`
	MusicID   int64         `json:"musicId"`
	Entries   []cover.Entry `json:"entries"`
}

// handleCoverManifest describes a track's artwork: what is in the file now and
// what is available to switch to.
func (s *Server) handleCoverManifest(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := requireFile(path); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	res, err := s.inspectCover(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// inspectCover builds the manifest and, on first contact with a track, files
// the artwork already in the file away as the original.
func (s *Server) inspectCover(path string) (*coverManifest, error) {
	hash, err := tag.AudioFingerprint(path)
	if err != nil {
		return nil, err
	}
	lib, err := s.libraryFor(hash)
	if err != nil {
		return nil, err
	}
	lib.RememberPath(path)

	art, err := tag.ReadCover(path)
	if err != nil && !errors.Is(err, tag.ErrNoCover) {
		return nil, err
	}

	current := ""
	if art != nil {
		// The first time a track is opened, whatever it currently carries is
		// the original — there is no earlier state to recover, so this is the
		// last chance to keep it.
		if _, ok := lib.Find(cover.IDOriginal); !ok {
			entry, err := lib.Add(cover.KindOriginal, art.Data, art.MIME, "")
			if err != nil {
				return nil, err
			}
			lib.SetCurrent(entry.ID)
			current = entry.ID
		} else if entry, err := lib.Add(cover.KindHistory, art.Data, art.MIME, ""); err == nil {
			// Already known? Add returns the existing entry rather than
			// duplicating it, which is what makes this idempotent.
			current = entry.ID
		}
	}

	if current != "" {
		lib.SetCurrent(current)
	} else {
		lib.SetCurrent("")
	}

	return &coverManifest{
		Path:      path,
		Name:      filepath.Base(path),
		AudioHash: hash,
		HasCover:  art != nil,
		Current:   current,
		MusicID:   s.resolveMusicID(lib, path),
		Entries:   lib.Entries(),
	}, nil
}

// resolveMusicID finds the NetEase song id for a track, trying the places it
// could have been recorded in order of how well they survive a rename.
func (s *Server) resolveMusicID(lib *cover.Library, path string) int64 {
	if id := lib.MusicID(); id != 0 {
		return id
	}
	if id := tag.ReadMusicID(path); id != 0 {
		lib.SetMusicID(id)
		return id
	}
	// Files decrypted before the id was written into the tag can still be
	// matched through the decryption record, as long as they have not moved.
	for _, rec := range s.store.Records() {
		if rec.OutputPath == path && rec.MusicID != 0 {
			lib.SetMusicID(rec.MusicID)
			return rec.MusicID
		}
	}
	return 0
}

// handleCoverImage serves one image: either an entry from the library, or
// "current" for whatever the file holds right now.
func (s *Server) handleCoverImage(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	id := r.URL.Query().Get("id")
	if path == "" || id == "" {
		writeError(w, http.StatusBadRequest, "path and id are required")
		return
	}

	var (
		data []byte
		mime string
		etag string
	)
	if id == "current" {
		art, err := tag.ReadCover(path)
		if err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if art == nil {
			writeError(w, http.StatusNotFound, "this file has no cover")
			return
		}
		data, mime = art.Data, art.MIME
		// The current artwork gets a validator too, and that is not the same
		// thing as caching it: an ETag lets the browser ask "still this one?"
		// and take a 304, so a list of two hundred rows costs two hundred tiny
		// requests instead of two hundred images after a reload. It is the
		// digest of the bytes, so it changes the moment the cover does.
		etag = cover.Digest(art.Data)
	} else {
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
		blob, entry, err := lib.Read(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "%v", err)
			return
		}
		data, mime, etag = blob, entry.MIME, entry.Digest
	}

	if mime == "" {
		mime = "image/jpeg"
	}
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
		if match := r.Header.Get("If-None-Match"); strings.Contains(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		return
	}
}

// handleCoverApply writes a chosen image into the audio file.
//
// Two ways in: an id naming an image already in the library, or an uploaded
// file. An upload joins the library first, so a failed rewrite still leaves the
// image available to try again rather than losing it.
func (s *Server) handleCoverApply(w http.ResponseWriter, r *http.Request) {
	var (
		path string
		id   string
		data []byte
		mime string
		// label is the original filename, kept only to make the library
		// readable when browsing it by hand.
		label string
	)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "parse upload: %v", err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		path = r.FormValue("path")
		id = r.FormValue("id")

		if f, header, err := r.FormFile("file"); err == nil {
			defer f.Close()
			blob, err := io.ReadAll(io.LimitReader(f, maxCoverUpload+1))
			if err != nil {
				writeError(w, http.StatusBadRequest, "read the image: %v", err)
				return
			}
			if len(blob) > maxCoverUpload {
				writeError(w, http.StatusRequestEntityTooLarge, "image is larger than %d MB", maxCoverUpload>>20)
				return
			}
			if !looksLikeImage(blob) {
				writeError(w, http.StatusBadRequest, "that file is not a JPEG, PNG, WebP or GIF image")
				return
			}
			data, mime, label = blob, header.Header.Get("Content-Type"), filepath.Base(header.Filename)
		}
	} else {
		var req struct{ Path, ID string }
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		path, id = req.Path, req.ID
	}

	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := requireFile(path); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(data) == 0 && id == "" {
		writeError(w, http.StatusBadRequest, "give either an image to upload or the id of one to apply")
		return
	}

	s.coverMu.Lock()
	defer s.coverMu.Unlock()

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

	var entry cover.Entry
	if len(data) > 0 {
		entry, err = lib.Add(cover.KindHistory, data, mime, label)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store the image: %v", err)
			return
		}
	} else {
		blob, e, err := lib.Read(id)
		if err != nil {
			writeError(w, http.StatusNotFound, "%v", err)
			return
		}
		data, mime, entry = blob, e.MIME, e
	}

	if err := tag.SetCover(path, data, mime, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "write the cover: %v", err)
		return
	}
	lib.SetCurrent(entry.ID)
	lib.RememberPath(path)

	manifest, err := s.inspectCover(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

// handleCoverRemove strips the artwork out of a file. The library is left
// alone, so any of the stored images can be put back afterwards.
func (s *Server) handleCoverRemove(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := requireFile(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	s.coverMu.Lock()
	defer s.coverMu.Unlock()

	if err := tag.SetCover(req.Path, nil, "", nil); err != nil {
		writeError(w, http.StatusInternalServerError, "remove the cover: %v", err)
		return
	}
	if hash, err := tag.AudioFingerprint(req.Path); err == nil {
		if lib, err := s.libraryFor(hash); err == nil {
			lib.SetCurrent("")
		}
	}

	manifest, err := s.inspectCover(req.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

// handleCoverFetch pulls the official artwork down and adds it to the library.
//
// It is stored rather than applied: the point is to offer it alongside the
// others so the choice stays with the user.
func (s *Server) handleCoverFetch(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := requireFile(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	hash, err := tag.AudioFingerprint(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	lib, err := s.libraryFor(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	musicID := s.resolveMusicID(lib, req.Path)
	if musicID == 0 {
		writeError(w, http.StatusBadRequest,
			"no NetEase song id is known for this track, so there is nothing to look up")
		return
	}

	data, mime, err := cover.FetchOfficial(r.Context(), nil, musicID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}

	s.coverMu.Lock()
	defer s.coverMu.Unlock()
	if _, err := lib.Add(cover.KindNetease, data, mime, "NetEase"); err != nil {
		writeError(w, http.StatusInternalServerError, "store the image: %v", err)
		return
	}

	manifest, err := s.inspectCover(req.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

// handleCoverForget deletes one image from the library. It refuses to touch the
// original, and refuses to delete the image the file is currently using, since
// that would leave the list disagreeing with the file.
func (s *Server) handleCoverForget(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path, ID string }
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.Path == "" || req.ID == "" {
		writeError(w, http.StatusBadRequest, "path and id are required")
		return
	}

	s.coverMu.Lock()
	defer s.coverMu.Unlock()

	hash, err := tag.AudioFingerprint(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	lib, err := s.libraryFor(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}

	if entry, ok := lib.Find(req.ID); ok && currentCoverIs(lib, req.Path) == entry.ID {
		writeError(w, http.StatusConflict,
			"this is the cover the file is using — switch to another one first")
		return
	}
	switch err := lib.Remove(req.ID); {
	case errors.Is(err, cover.ErrProtected):
		writeError(w, http.StatusConflict, "the original cover is kept so you can always go back to it")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}

	manifest, err := s.inspectCover(req.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

// currentCoverIs reports the id of the library entry the file is using, or ""
// when the file has no cover or one the library does not know about.
func currentCoverIs(lib *cover.Library, path string) string {
	art, err := tag.ReadCover(path)
	if err != nil || art == nil {
		return ""
	}
	digest := cover.Digest(art.Data)
	for _, e := range lib.Entries() {
		if e.Digest == digest {
			return e.ID
		}
	}
	return ""
}

// ── helpers ───────────────────────────────────────────────────────────────

// libraryFor returns the cover library for a track, in the current workspace.
func (s *Server) libraryFor(hash string) (*cover.Library, error) {
	store, err := s.coverStore()
	if err != nil {
		return nil, err
	}
	return store.Library(hash)
}

func requireFile(path string) error {
	if path == "" {
		return errors.New("path is required")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("that is a directory, not an audio file")
	}
	return nil
}

func looksLikeImage(data []byte) bool {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return true
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return true
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return true
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return true
	}
	return false
}
