// Package web serves the single-page UI and its JSON API.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ncm-studio/internal/ai"
	"ncm-studio/internal/backfill"
	"ncm-studio/internal/cover"
	"ncm-studio/internal/job"
	"ncm-studio/internal/store"
)

//go:embed static
var staticFS embed.FS

// Server wires the store, the job pools and the HTTP surface together.
type Server struct {
	store *store.Store
	// The four pools are separate because starting one cancels whatever it is
	// running (see job.Pool.Start), and a user who starts a backfill has not
	// asked to stop their conversion, their .lrc export or their translation.
	// Each page drives its own and streams its own progress.
	pool    *job.Pool
	lyrics  *job.Pool
	lrc     *job.Pool
	ai      *job.Pool
	version string

	// searcher is the candidate pools the lyrics batch resolved. The manual
	// search shares it so that asking about a song the batch already found
	// costs no request — which matters, because the search endpoint refuses a
	// client after a handful of them.
	searcher *backfill.Searcher
	// clients hands out the lyric client for a workspace.
	clients backfill.ClientFor
	// writeSem bounds how many files are rewritten at once.
	writeSem chan struct{}

	// mu serialises the small mutable bits that are not the store's.
	mu sync.Mutex
	// sseCount guards against an unbounded number of idle SSE clients.
	sseCount int

	// covers is the library for the workspace in coversRoot. It is built on
	// first use and rebuilt when the workspace changes, so the setup screen can
	// point the workspace somewhere new and have it be true immediately rather
	// than after a restart.
	covers     *cover.Store
	coversRoot string

	// coverMu serialises rewriting an audio file. A rewrite copies the whole
	// track, so two of them racing on one file would interleave into garbage.
	coverMu sync.Mutex
}

// Pools are the job pools a server drives, one per page that runs a batch.
type Pools struct {
	// Convert decrypts .ncm files.
	Convert *job.Pool
	// Lyrics backfills lyrics for files already on disk.
	Lyrics *job.Pool
	// LRC moves lyrics between a file's tags and a .lrc beside it.
	LRC *job.Pool
	// AI translates a file's lyrics with a language model.
	AI *job.Pool
}

// New builds a server around an existing store and its pools. The version is
// shown in the UI's about panel.
func New(st *store.Store, pools Pools, searcher *backfill.Searcher, clients backfill.ClientFor, version string) *Server {
	return &Server{
		store:    st,
		pool:     pools.Convert,
		lyrics:   pools.Lyrics,
		lrc:      pools.LRC,
		ai:       pools.AI,
		searcher: searcher,
		clients:  clients,
		version:  version,
		// Two at a time: one rewrite already saturates the storage on a phone,
		// and the second keeps a batch moving while the first is syncing.
		writeSem: make(chan struct{}, 2),
	}
}

// coverStore returns the cover library for the configured workspace.
//
// The library belongs to the workspace because that is the directory the user
// chose to keep their data in, so a change there has to move the library, not
// merely be recorded for the next start.
func (s *Server) coverStore() (*cover.Store, error) {
	root := filepath.Join(s.store.Config().Workspace, "covers")

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.covers != nil && s.coversRoot == root {
		return s.covers, nil
	}
	st, err := cover.NewStore(root)
	if err != nil {
		return nil, err
	}
	s.covers, s.coversRoot = st, root
	return st, nil
}

// Handler returns the fully routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Static assets, served from the embedded copy so the binary is
	// self-contained.
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(static)))

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/files", s.handleFiles)
	mux.HandleFunc("POST /api/start", s.handleStart)
	mux.HandleFunc("POST /api/cancel", s.handleCancel)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	// The lyrics page and the cover page both list audio files, and the lyrics
	// batch drives its own pool and its own stream.
	mux.HandleFunc("GET /api/audio", s.handleAudio)
	mux.HandleFunc("GET /api/lyrics/state", s.handleLyricsState)
	mux.HandleFunc("GET /api/lyrics/events", s.handleLyricsEvents)
	mux.HandleFunc("POST /api/lyrics/start", s.handleLyricsStart)
	mux.HandleFunc("POST /api/lyrics/cancel", s.handleLyricsCancel)
	mux.HandleFunc("POST /api/lyrics/match", s.handleLyricsMatch)
	mux.HandleFunc("POST /api/lyrics/apply", s.handleLyricsApply)
	// Reading one file's lyrics back, for the viewer the .lrc, AI and info
	// pages all open.
	mux.HandleFunc("GET /api/lyrics/view", s.handleLyricsView)

	// The .lrc page: one pool, two directions, chosen per batch.
	mux.HandleFunc("GET /api/lrc/state", s.handleLRCState)
	mux.HandleFunc("GET /api/lrc/events", s.handleLRCEvents)
	mux.HandleFunc("POST /api/lrc/start", s.handleLRCStart)
	mux.HandleFunc("POST /api/lrc/cancel", s.handleLRCCancel)

	// The translation page and the keys it needs.
	mux.HandleFunc("GET /api/ai/state", s.handleAIState)
	mux.HandleFunc("GET /api/ai/events", s.handleAIEvents)
	mux.HandleFunc("POST /api/ai/start", s.handleAIStart)
	mux.HandleFunc("POST /api/ai/cancel", s.handleAICancel)
	mux.HandleFunc("GET /api/ai/providers", s.handleAIProviders)
	mux.HandleFunc("POST /api/ai/key", s.handleAIKey)
	mux.HandleFunc("POST /api/ai/models", s.handleAIModels)
	mux.HandleFunc("POST /api/ai/preview", s.handleAIPreview)

	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("POST /api/mkdir", s.handleMkdir)
	mux.HandleFunc("POST /api/rename", s.handleRename)
	mux.HandleFunc("POST /api/delete", s.handleDelete)
	mux.HandleFunc("GET /api/download", s.handleDownload)
	mux.HandleFunc("POST /api/upload", s.handleUpload)

	mux.HandleFunc("GET /api/cover", s.handleCoverManifest)
	mux.HandleFunc("GET /api/cover/image", s.handleCoverImage)
	mux.HandleFunc("POST /api/cover/apply", s.handleCoverApply)
	mux.HandleFunc("POST /api/cover/remove", s.handleCoverRemove)
	mux.HandleFunc("POST /api/cover/fetch", s.handleCoverFetch)
	mux.HandleFunc("POST /api/cover/forget", s.handleCoverForget)
	// The cover page's backfill: one file, one request, driven by the page so
	// that stopping between two files needs no cancel of its own.
	mux.HandleFunc("POST /api/cover/fill", s.handleCoverFill)

	return logRequests(sameOrigin(mux))
}

// sameOrigin refuses a state-changing request that a browser made from a page
// this program does not serve.
//
// Every endpoint here answers the page in static/, and that page is same-origin
// with all of them, so a request carrying another site's Origin is not the UI.
// It matters because the endpoints behind this guard delete files, rewrite the
// settings and replace API keys — and because this guard is not the browser's
// CORS check: a POST whose body is JSON but whose Content-Type says text/plain
// is a "simple" request, sent without a preflight, and it reaches the handler,
// which parses the body without ever looking at that header. The label below is
// what is left to tell the two apart.
//
// A request carrying neither header is not a page load at all — curl, a script,
// a WebView fetch from the user's own code — and is left alone. The program may
// be bound to 0.0.0.0, but the person at the keyboard is not the threat here.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		// "none" is a navigation the user typed, which is still not another page.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameHost(origin, r.Host) {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameHost reports whether an Origin header names the host the request was sent
// to. The scheme is deliberately not compared: the same page opened over the
// LAN address is the same page, and comparing schemes would refuse exactly the
// case a phone on the same network uses.
func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host != "" && strings.EqualFold(u.Host, host)
}

// logRequests logs only failures; a local tool does not need a line per asset.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 {
			log.Printf("%s %s -> %d", r.Method, r.URL.Path, rec.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so the event stream can push each
// event out as it happens.
//
// This has to be spelled out: embedding the http.ResponseWriter interface does
// not promote Flush, because the interface does not declare it. Without this,
// the Flusher assertion in handleEvents fails and every SSE client is refused.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// decodeJSON reads a small JSON request body.
func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// stateResponse is everything the UI needs to render itself on load.
type stateResponse struct {
	Config  store.Config `json:"config"`
	Running bool         `json:"running"`
	Items   []*job.Item  `json:"items"`
	Workers int          `json:"workers"`
	Home    string       `json:"home"`
	Version string       `json:"version"`
	// ConfigDir is where the settings file lives, which is worth showing but is
	// not the workspace: that is the user's, and holds the covers and caches.
	ConfigDir string `json:"configDir"`
	// DefaultWorkspace lets the setup screen pre-fill the directory this
	// install would pick on its own.
	DefaultWorkspace string `json:"defaultWorkspace"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, stateResponse{
		Config:           s.store.Config(),
		Running:          s.pool.Running(),
		Items:            s.pool.Snapshot(),
		Workers:          s.pool.Workers(),
		Home:             home,
		Version:          s.version,
		ConfigDir:        s.store.Dir(),
		DefaultWorkspace: store.DefaultWorkspace(home),
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	// The body is read whole rather than decoded into a struct of pointers: the
	// store has to see which keys arrived, not only their values, because a key
	// that is absent means "leave this setting alone" and one that is present
	// with an empty value sometimes means "clear it".
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read settings: %v", err)
		return
	}

	// The AI panel's fields stay here: choosing a provider rewrites the model
	// beside it and remembers the pair, which is a decision about the shape of
	// the settings rather than a value being written into one of them. Workers
	// is read here only to clamp it; Language and the directories are the
	// store's to write.
	var patch struct {
		Workspace   *string         `json:"workspace"`
		Workers     *int            `json:"workers"`
		LCROutput   *string         `json:"lrcOutput"`
		LRCConflict *store.Conflict `json:"lrcConflict"`
		AITarget    *string         `json:"aiTarget"`
		AIProvider  *string         `json:"aiProvider"`
		AIModel     *string         `json:"aiModel"`
		AIThinking  *string         `json:"aiThinking"`
	}
	if err := decodeJSONBytes(body, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	// A workspace is where the cover library and caches live, so it has to be
	// usable before it is adopted; failing later would strand the user with a
	// settings file pointing somewhere unwritable.
	if patch.Workspace != nil && *patch.Workspace != "" {
		if err := os.MkdirAll(*patch.Workspace, 0o755); err != nil {
			writeError(w, http.StatusBadRequest, "cannot use that workspace: %v", err)
			return
		}
	}

	// Everything with a plain value on it goes through the store, which is what
	// decides which keys were sent and which of them this build understands.
	if err := s.store.Patch(body); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	err = s.store.Update(func(c *store.Config) {
		if patch.Workers != nil {
			// Clamped here rather than only in the pool, so the number written
			// to the settings file is the one that will actually run. Storing
			// an out-of-range value would leave the UI showing a setting it
			// cannot represent.
			c.Workers = job.ClampWorkers(*patch.Workers)
		}
		if patch.LCROutput != nil {
			// Cleared on purpose: an empty value means "beside the audio", and
			// refusing it would leave a user unable to undo the choice.
			c.LCROutput = strings.TrimSpace(*patch.LCROutput)
		}
		if patch.LRCConflict != nil && patch.LRCConflict.Valid() {
			c.LRCConflict = *patch.LRCConflict
		}
		if patch.AITarget != nil {
			c.AITarget = strings.TrimSpace(*patch.AITarget)
		}
		if patch.AIProvider != nil {
			if _, ok := ai.ProviderByID(*patch.AIProvider); ok {
				c.AIProvider = *patch.AIProvider
				// The model follows the provider: what the panel shows after a
				// switch is what the next batch would call, and the two cannot
				// be allowed to disagree. A provider never used before has no
				// remembered model, and empty means its documented default.
				c.AIModel = c.AIModels[strings.ToLower(strings.TrimSpace(c.AIProvider))]
			}
		}
		if patch.AIModel != nil {
			c.AIModel = strings.TrimSpace(*patch.AIModel)
			// Remembered against the provider it was chosen for, so switching
			// away and back does not lose it.
			provider := strings.ToLower(strings.TrimSpace(c.AIProvider))
			if provider == "" {
				provider = "deepseek"
			}
			if c.AIModels == nil {
				c.AIModels = map[string]string{}
			}
			if c.AIModel == "" {
				delete(c.AIModels, provider)
			} else {
				c.AIModels[provider] = c.AIModel
			}
		}
		if patch.AIThinking != nil && ai.ValidThinking(*patch.AIThinking) {
			c.AIThinking = *patch.AIThinking
		}
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save settings: %v", err)
		return
	}
	if patch.Workers != nil {
		s.pool.SetWorkers(job.ClampWorkers(*patch.Workers))
	}
	writeJSON(w, http.StatusOK, s.store.Config())
}

// decodeJSONBytes reads a small JSON request body that has already been read
// into memory, so that one body can answer several questions about itself.
func decodeJSONBytes(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// fileEntry is one .ncm file found during a scan.
type fileEntry struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"` // "new" or "done"
	Output string `json:"output,omitempty"`
	// Skipped is why this file is here but will not be converted: today only
	// "too small", which the settings decide. The row is listed rather than
	// dropped so that a file the user is looking for cannot simply disappear —
	// the reason is on it, and the way to get it back is one setting away.
	Skipped string `json:"skipped,omitempty"`
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = s.store.Config().Dir
	}
	entries, err := scanNCM(dir, r.URL.Query().Get("recursive") == "1")
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	minFilter := s.store.ScanFilter()

	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		fi, err := os.Stat(e)
		if err != nil {
			continue
		}
		fe := fileEntry{Path: e, Name: filepath.Base(e), Size: fi.Size(), State: "new"}
		if ok, reason := minFilter(e, fi.Size()); !ok {
			fe.Skipped = reason
		}
		if hash, err := s.store.FingerprintCached(e); err == nil {
			if rec, ok := s.store.Lookup(hash); ok {
				fe.Output = rec.OutputPath
				if s.store.AlreadyDone(hash) {
					fe.State = "done"
				}
			}
		}
		out = append(out, fe)
	}
	// Every file that was not already cached just added an entry. The scan is
	// over, so write them out here — once — rather than letting the cache file
	// be rewritten a few hundred times on the way through the loop.
	if err := s.store.Flush(); err != nil {
		log.Printf("save hash cache: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// scanNCM lists .ncm files under dir. Unreadable subdirectories are skipped
// rather than failing the whole scan, which is what browsing a phone's storage
// with its permission-restricted corners requires.
func scanNCM(dir string, recursive bool) ([]string, error) {
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
				if sub, err := scanNCM(full, true); err == nil {
					out = append(out, sub...)
				}
			}
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".ncm") {
			out = append(out, full)
		}
	}
	return out, nil
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
		Dir   string   `json:"dir"`
		// Modes overrides the lyrics landing for individual files, as the
		// integer the settings use; see applyModes.
		Modes map[string]int `json:"modes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}

	paths := req.Paths
	if req.All {
		dir := req.Dir
		if dir == "" {
			dir = s.store.Config().Dir
		}
		found, err := scanNCM(dir, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		// The size filter applies here too, and not only to the listing: this
		// button exists to convert whatever the scan would have shown, so a
		// file the page deliberately left out must not be converted by it.
		minFilter := s.store.ScanFilter()
		paths = paths[:0]
		for _, p := range found {
			if fi, err := os.Stat(p); err == nil {
				if ok, _ := minFilter(p, fi.Size()); !ok {
					continue
				}
			}
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		writeError(w, http.StatusBadRequest, "no files selected")
		return
	}

	items := make([]*job.Item, 0, len(paths))
	for _, p := range paths {
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

	s.pool.Start(items)
	log.Printf("started a batch of %d file(s)", len(items))
	writeJSON(w, http.StatusOK, map[string]int{"queued": len(items)})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.pool.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

// handleEvents streams conversion updates as server-sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.streamPool(w, r, s.pool)
}

// handleLyricsEvents streams backfill updates on their own stream.
//
// It is a second stream rather than a second kind of event on the first one
// because the two pages are open at different times and neither should have to
// filter the other's traffic — and because a page that has just opened the
// lyrics tab has no use for a running conversion's progress.
func (s *Server) handleLyricsEvents(w http.ResponseWriter, r *http.Request) {
	s.streamPool(w, r, s.lyrics)
}

// streamPool forwards one pool's events over SSE until the client goes away.
func (s *Server) streamPool(w http.ResponseWriter, r *http.Request, pool *job.Pool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	s.mu.Lock()
	if s.sseCount >= 16 {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "too many open event streams")
		return
	}
	s.sseCount++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sseCount--
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := pool.Subscribe()
	defer unsubscribe()

	// A periodic comment keeps intermediaries from closing an idle stream.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case evt, ok := <-events:
			if !ok {
				return
			}
			fmt.Fprint(w, "data: ")
			if err := enc.Encode(evt); err != nil {
				return
			}
			fmt.Fprint(w, "\n")
			flusher.Flush()
		}
	}
}

// dirEntry is one row in the file browser.
type dirEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.store.Config().Dir
	}
	path = filepath.Clean(path)

	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, dirEntry{
			Name:    e.Name(),
			Path:    filepath.Join(path, e.Name()),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    path,
		"parent":  parent,
		"entries": out,
		"roots":   browseRoots(),
	})
}

// browseRoots lists the places worth offering as shortcuts. Missing ones are
// simply omitted, so the same list works on a phone and a desktop.
func browseRoots() []map[string]string {
	home, _ := os.UserHomeDir()
	candidates := []struct{ name, path string }{
		{"home", home},
		{"storage", "/storage/emulated/0"},
		{"sdcard", "/sdcard"},
		{"downloads", "/storage/emulated/0/Download"},
		{"music", "/storage/emulated/0/Music"},
		{"root", "/"},
	}
	out := make([]map[string]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, c := range candidates {
		if c.path == "" || seen[c.path] {
			continue
		}
		if fi, err := os.Stat(c.path); err != nil || !fi.IsDir() {
			continue
		}
		seen[c.path] = true
		out = append(out, map[string]string{"name": c.name, "path": c.path})
	}
	return out
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := os.MkdirAll(req.Path, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": req.Path})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct{ From, To string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.From == "" || req.To == "" {
		writeError(w, http.StatusBadRequest, "from and to are required")
		return
	}
	// Refuse to clobber: a rename onto an existing file is far more likely to
	// be a mistake than an intent.
	if _, err := os.Stat(req.To); err == nil {
		writeError(w, http.StatusConflict, "%s already exists", filepath.Base(req.To))
		return
	}
	if err := os.Rename(req.From, req.To); err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": req.To})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "no paths given")
		return
	}

	var failed []string
	for _, p := range req.Paths {
		if err := os.RemoveAll(p); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", filepath.Base(p), err))
		}
	}
	if len(failed) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"deleted": len(req.Paths) - len(failed), "errors": failed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(req.Paths)})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "no such file")
			return
		}
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeError(w, http.StatusBadRequest, "not a file")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEscape(filepath.Base(path)))
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// handleUpload accepts dragged-in .ncm files and stores them in the configured
// source directory.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	// Long uploads of large files need a generous ceiling; the limit here is
	// per-request memory, not total size, because the body streams to disk.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parse upload: %v", err)
		return
	}
	defer r.MultipartForm.RemoveAll()

	dir := s.store.Config().Dir
	if dir == "" {
		writeError(w, http.StatusBadRequest, "no destination directory configured")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}

	var saved []string
	for _, headers := range r.MultipartForm.File {
		for _, h := range headers {
			name := filepath.Base(h.Filename)
			if !strings.EqualFold(filepath.Ext(name), ".ncm") {
				continue
			}
			dst, err := uniquePath(filepath.Join(dir, name))
			if err != nil {
				writeError(w, http.StatusInternalServerError, "%v", err)
				return
			}
			if err := saveUploadedFile(h, dst); err != nil {
				writeError(w, http.StatusInternalServerError, "save %s: %v", name, err)
				return
			}
			saved = append(saved, dst)
		}
	}
	if len(saved) == 0 {
		writeError(w, http.StatusBadRequest, "no .ncm files in the upload")
		return
	}
	log.Printf("uploaded %d file(s) to %s", len(saved), dir)
	writeJSON(w, http.StatusOK, map[string]any{"saved": saved})
}

func uniquePath(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path, nil
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}
