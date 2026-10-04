// Package store persists user settings and the record of what has already
// been decrypted.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LyricsMode mirrors pipeline.LyricsMode as a JSON-friendly integer.
type LyricsMode int

const (
	LyricsOff LyricsMode = iota
	LyricsEmbed
	LyricsEmbedAndFile
	// LyricsFileOnly writes a sidecar .lrc and leaves the audio file's tags
	// alone. Appended last so the integers already in a saved config keep
	// meaning what they meant when they were written.
	LyricsFileOnly
)

// Valid reports whether a mode is one this build knows. Settings arrive from
// the web API as plain integers, so an out-of-range value has to be caught
// before it reaches a switch that would silently treat it as "off".
func (m LyricsMode) Valid() bool { return m >= LyricsOff && m <= LyricsFileOnly }

// Conflict is what a .lrc export or import does when the destination already
// holds something different.
type Conflict int

const (
	// ConflictOverwrite replaces what is there. It is the zero value because it
	// is the default: a file whose lyrics are being brought up to date is the
	// ordinary case, and stopping to ask about each one would be the surprise.
	ConflictOverwrite Conflict = iota
	// ConflictSkip leaves the destination alone and reports it as skipped.
	ConflictSkip
	// ConflictMerge combines the two by timestamp, the incoming side winning
	// where both have a line. It is how a hand-edited .lrc keeps the lines
	// somebody added to it.
	ConflictMerge
)

// Valid reports whether a conflict policy is one this build knows.
func (c Conflict) Valid() bool { return c >= ConflictOverwrite && c <= ConflictMerge }

// DeleteAfter is what happens to a .ncm file once it has been decrypted.
type DeleteAfter int

const (
	// DeleteNever leaves the source where it is. It is the zero value because
	// deleting a file nobody asked about is the one mistake here that cannot be
	// undone — and a .ncm is often the only copy of what was bought.
	DeleteNever DeleteAfter = iota
	// DeleteAuto removes the source as soon as its output has been written.
	DeleteAuto
	// DeleteAsk leaves the source alone and lets the page offer to remove the
	// batch's files when the run finishes. It is the middle answer for somebody
	// who wants the cleanup but not without seeing what it covers.
	DeleteAsk
)

// Valid reports whether a delete policy is one this build knows.
func (d DeleteAfter) Valid() bool { return d >= DeleteNever && d <= DeleteAsk }

// LyricsSource is where a lyric comes from when it has been fetched before.
type LyricsSource int

const (
	// LyricsSourceCache reuses what was downloaded earlier. It is the zero
	// value because it is what makes a second pass over a library nearly free,
	// and because the words to a song mostly do not change.
	LyricsSourceCache LyricsSource = iota
	// LyricsSourceFetch ignores the stored copy and asks the catalogue again.
	// It exists because "does not change" is not the same as "cannot": a lyric
	// that was wrong when it was written, or that upstream has since improved —
	// a translation added, a timing corrected — is only ever picked up by
	// asking again. What comes back replaces the stored copy, so the next run
	// is back to being free.
	LyricsSourceFetch
)

// Valid reports whether a lyric source is one this build knows.
func (s LyricsSource) Valid() bool { return s >= LyricsSourceCache && s <= LyricsSourceFetch }

// AITranslated is what a translation batch does with a file that already
// carries a machine translation.
type AITranslated int

const (
	// AITranslatedSkip leaves such a file alone. It is the zero value because a
	// translation costs money per request, and translating a library that was
	// translated yesterday is how the same words get paid for twice.
	AITranslatedSkip AITranslated = iota
	// AITranslatedPick lists them without ticking them, so a batch can take in
	// the handful somebody wants redone — after switching model, say — without
	// redoing the rest.
	AITranslatedPick
	// AITranslatedRedo ticks them like anything else and translates them again,
	// which is what a user who has just moved to a better model wants for the
	// whole library.
	AITranslatedRedo
)

// Valid reports whether a translation policy is one this build knows.
func (a AITranslated) Valid() bool {
	return a >= AITranslatedSkip && a <= AITranslatedRedo
}

// Redoes reports whether the setting asks for every file that is already
// translated to be translated again.
//
// Deliberately false for AITranslatedPick: that setting leaves the decision on
// the row, where ticking it carries its own force. A processor that redid the
// file anyway would take that choice back.
func (a AITranslated) Redoes() bool { return a == AITranslatedRedo }

// Config is the subset of settings the web UI can change. Address and port
// stay on the command line, since changing them would need a restart anyway.
//
// Every field is safe to serialise: nothing secret lives here. The AI keys are
// kept in their own file for exactly that reason, so a config copied into a bug
// report cannot carry one; see ai.go.
//
// The three directories are deliberately separate: Dir is what gets scanned,
// Output is where decrypted audio lands, and Workspace is where this tool keeps
// its own files — the cover library, the lyric cache and the history index.
// Keeping them apart means the output folder can be handed to a music player
// without dragging the tool's bookkeeping along with it.
type Config struct {
	Dir       string     `json:"dir"`
	Output    string     `json:"output"`
	Workspace string     `json:"workspace"`
	Workers   int        `json:"workers"`
	Lyrics    LyricsMode `json:"lyrics"`
	Lang      string     `json:"lang"`
	// LCROutput is where the .lrc page exports its files. Empty means beside
	// the audio file, which is where a player looks for one. The backfill's own
	// sidecars always go beside the file whatever this says: a player has to
	// find them.
	LCROutput string `json:"lrcOutput,omitempty"`
	// LRCConflict is what the .lrc page does when what it is writing over says
	// something else.
	LRCConflict Conflict `json:"lrcConflict"`

	// AITarget is the language translations are written into, named the way a
	// person would name it. The provider and model are the last ones chosen;
	// the key is not here, see AISettings.
	AITarget   string `json:"aiTarget,omitempty"`
	AIProvider string `json:"aiProvider,omitempty"`
	// AIModel is the model the next batch will call. Switching provider
	// rewrites it from AIModels, so it always describes the provider beside it.
	AIModel string `json:"aiModel,omitempty"`
	// AIModels remembers the model each provider was last used with, so
	// switching away and back lands on the one that was working rather than on
	// the vendor's default again.
	AIModels map[string]string `json:"aiModels,omitempty"`
	// AIModelList is the last model list fetched from each vendor. It is a
	// cache of what a key can call, kept so the suggestion list does not go
	// blank on every reload; it is never the only source, since the model field
	// is free text and the built-in list is always there.
	AIModelList map[string][]string `json:"aiModelList,omitempty"`
	// AIThinking is how hard a reasoning model is asked to think: empty for the
	// vendor's default, or "off", "low", "high" or "max". Only a provider that
	// takes thinking reads it.
	AIThinking string `json:"aiThinking,omitempty"`

	// DeleteSource is what happens to a .ncm after its audio has been written
	// out. See DeleteAfter.
	DeleteSource DeleteAfter `json:"deleteSource"`

	// LyricsSource is where a lyric comes from when one has been fetched
	// before; see LyricsSource. AITranslated is what a translation batch does
	// with a file that already carries a translation; see AITranslated.
	LyricsSource LyricsSource `json:"lyricsSource"`
	AITranslated AITranslated `json:"aiTranslated"`

	// MinSize is the smallest .ncm a scan will offer, and the unit the
	// settings page shows it in. A minimum rather than a maximum because the
	// files worth hiding are the tiny ones — a truncated download decrypts
	// into a few kilobytes of noise and then sits in the list looking like a
	// track. The zero value means no filtering at all; see MinSize.
	MinSize MinSize `json:"minSize"`

	// Initialized records that the user has been through first-run setup and
	// chosen a workspace. Until it is set the UI offers that choice instead of
	// assuming the default is what they want.
	Initialized bool `json:"initialized"`
}

// DefaultConfig returns the settings a fresh install starts from.
func DefaultConfig(home string) Config {
	return Config{
		Dir:       home,
		Output:    filepath.Join(home, "ncm-output"),
		Workspace: DefaultWorkspace(home),
		Workers:   3,
		Lyrics:    LyricsEmbedAndFile,
		Lang:      "",
		MinSize:   DefaultMinSize(),
	}
}

// DefaultWorkspace picks where a fresh install keeps its own files.
//
// A directory beside the executable is preferred, so that copying the program
// folder somewhere else carries the settings, the cover library and the caches
// with it. That only makes sense when the program is somewhere durable and
// writable, so a binary under the system temp directory — which is what
// `go run` produces — falls back to the home directory instead of leaving the
// library in a folder the OS will sweep away.
func DefaultWorkspace(home string) string {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); usableForData(dir) {
			return filepath.Join(dir, "ncm-studio-data")
		}
	}
	return filepath.Join(home, "ncm-studio-data")
}

func usableForData(dir string) bool {
	if dir == "" {
		return false
	}
	if tmp := os.TempDir(); tmp != "" {
		if rel, err := filepath.Rel(tmp, dir); err == nil && !strings.HasPrefix(rel, "..") {
			return false
		}
	}
	probe := filepath.Join(dir, ".ncm-studio-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}

// Record is one successfully decrypted track.
type Record struct {
	SourceHash string    `json:"sourceHash"`
	SourcePath string    `json:"sourcePath"`
	OutputPath string    `json:"outputPath"`
	Title      string    `json:"title"`
	MusicID    int64     `json:"musicId"`
	Lyrics     bool      `json:"lyrics"`
	At         time.Time `json:"at"`
}

// hashEntry caches a file digest so a directory scan does not re-hash files
// that have not changed.
type hashEntry struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Hash  string `json:"hash"`
}

// writeBudget is how long a change may sit unwritten before it is forced out.
//
// Saving is a full create-write-rename cycle of the whole file, and on a phone
// that lands on FUSE storage where each cycle costs several syscalls. Both
// files change once per file processed — a record when a track finishes, a
// hash entry for every file a scan looks at — so writing on every change turns
// a scan of a large library into thousands of rewrites of the same two small
// files, each one growing, which is quadratic in the number of files.
//
// Holding changes for a couple of seconds collapses that into a handful of
// writes. Nothing is lost by waiting: the in-memory state is always current,
// and the only cost of a crash inside the window is that a scan re-hashes some
// files or a track is decrypted twice.
const writeBudget = 2 * time.Second

// Store owns the on-disk config, the decrypted-file record and the hash cache.
// All access is serialised; the maps are small and contention is negligible.
type Store struct {
	mu       sync.Mutex
	dir      string
	config   Config
	records  map[string]Record    // source hash -> record
	hashes   map[string]hashEntry // source path -> cached digest
	hashPath string

	// dirty flags say the in-memory state has moved ahead of the file, and
	// pending is the timer that guarantees the gap closes even if no caller
	// ever asks for it.
	dirtyConfig bool
	dirtyHashes bool
	pending     *time.Timer
	lastWrite   time.Time

	// keysOnce guards the lazily loaded AI key file; see ai.go for why the keys
	// are not part of the settings.
	keysOnce sync.Once
	aiKeys   *aiKeys
}

// Open loads the store from dir, creating it when absent. A corrupt or
// unreadable file is replaced with defaults rather than blocking startup.
func Open(dir string, defaults Config) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:      dir,
		config:   defaults,
		records:  map[string]Record{},
		hashes:   map[string]hashEntry{},
		hashPath: filepath.Join(dir, "hashes.json"),
	}

	var file struct {
		Config  Config               `json:"config"`
		Records map[string]Record    `json:"records"`
		Hashes  map[string]hashEntry `json:"hashes"`
	}
	if data, err := os.ReadFile(s.configPath()); err == nil {
		if json.Unmarshal(data, &file) == nil {
			if file.Config.Workers > 0 {
				s.config = file.Config
			}
			// A saved config describes every setting it knows about, so one
			// that does not mention the size filter predates it. Coming up with
			// the default is right there, while an explicit 0 — which is how
			// the setting is turned off — is a value and is kept.
			if !patchKeys(data)["minSize"] {
				s.config.MinSize = DefaultMinSize()
			} else {
				s.config.MinSize = s.config.MinSize.normalise()
			}
			// A settings file written before the workspace existed has no
			// value for it; fill in the default rather than leaving it empty.
			if s.config.Workspace == "" {
				s.config.Workspace = defaults.Workspace
			}
			if file.Records != nil {
				s.records = file.Records
			}
		}
	}
	if data, err := os.ReadFile(s.hashPath); err == nil {
		var hashes map[string]hashEntry
		if json.Unmarshal(data, &hashes) == nil && hashes != nil {
			s.hashes = hashes
		}
	}
	return s, nil
}

func (s *Store) configPath() string { return filepath.Join(s.dir, "config.json") }

// Dir returns the directory the store keeps its files in.
func (s *Store) Dir() string { return s.dir }

// Config returns a copy of the current settings.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// Update merges a change into the settings and persists them.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.config)
	return s.saveLocked()
}

// Patch merges a JSON settings patch into the config and saves it.
//
// The web API hands its request body straight to this rather than deciding in
// the handler which fields were sent: a field that is absent has to leave the
// setting alone, which is a question about the bytes that arrived, and the
// settings file itself uses the same shape. Values outside what this build
// understands are rejected with a message naming the field, so the page can say
// what it did not like instead of quietly writing something else.
func (s *Store) Patch(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	setString := func(key string, dst *string, allowEmpty bool) error {
		raw, ok := fields[key]
		if !ok {
			return nil
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if !allowEmpty && strings.TrimSpace(v) == "" {
			return nil
		}
		*dst = v
		return nil
	}
	setInt := func(key string, dst *int) error {
		raw, ok := fields[key]
		if !ok {
			return nil
		}
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = v
		return nil
	}

	if err := setString("dir", &s.config.Dir, false); err != nil {
		return err
	}
	if err := setString("output", &s.config.Output, false); err != nil {
		return err
	}
	if err := setString("workspace", &s.config.Workspace, false); err != nil {
		return err
	}
	if raw, ok := fields["minSize"]; ok {
		// A patch that names the field is taken at its word, including a null
		// or an empty object: both mean "no minimum", which is the value that
		// has to stay reachable.
		if min, present, err := decodeMinSize(raw); err != nil {
			return fmt.Errorf("minSize: %w", err)
		} else if present {
			s.config.MinSize = min
		}
		if raw, ok := fields["minSizeUnit"]; ok {
			var unit string
			if err := json.Unmarshal(raw, &unit); err != nil {
				return fmt.Errorf("minSizeUnit: %w", err)
			}
			if u, ok := ParseSizeUnit(unit); ok {
				// The unit is presentation only; it must not move the line, so
				// only the remembered unit changes here.
				s.config.MinSize.Unit = u
			}
		}
	}
	if err := setInt("workers", &s.config.Workers); err != nil {
		return err
	}
	if raw, ok := fields["lyrics"]; ok {
		var mode LyricsMode
		if err := json.Unmarshal(raw, &mode); err != nil {
			return fmt.Errorf("lyrics: %w", err)
		}
		if mode.Valid() {
			s.config.Lyrics = mode
		}
	}
	if err := setString("lang", &s.config.Lang, true); err != nil {
		return err
	}
	if raw, ok := fields["initialized"]; ok {
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("initialized: %w", err)
		}
		s.config.Initialized = v
	}
	if raw, ok := fields["deleteSource"]; ok {
		var v DeleteAfter
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("deleteSource: %w", err)
		}
		if v.Valid() {
			s.config.DeleteSource = v
		}
	}
	if raw, ok := fields["lyricsSource"]; ok {
		var v LyricsSource
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("lyricsSource: %w", err)
		}
		if v.Valid() {
			s.config.LyricsSource = v
		}
	}
	if raw, ok := fields["aiTranslated"]; ok {
		var v AITranslated
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("aiTranslated: %w", err)
		}
		if v.Valid() {
			s.config.AITranslated = v
		}
	}
	return s.saveLocked()
}

// KnownHash returns a cached digest for path when the file is unchanged.
func (s *Store) KnownHash(path string, size, mtime int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.hashes[path]
	if !ok || e.Size != size || e.MTime != mtime {
		return "", false
	}
	return e.Hash, true
}

// RememberHash caches a digest for path.
//
// The entry is visible to KnownHash immediately, but the file on disk catches
// up on the write budget rather than on every call — see writeBudget.
func (s *Store) RememberHash(path, hash string, size, mtime int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashes[path] = hashEntry{Size: size, MTime: mtime, Hash: hash}
	s.dirtyHashes = true
	_ = s.deferSaveLocked()
}

// Lookup returns the record for a source digest.
func (s *Store) Lookup(hash string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[hash]
	return r, ok
}

// AlreadyDone reports whether a source file has been decrypted into a file
// that still exists. A record whose output has since been deleted does not
// count, so removing an output re-queues the track instead of silently
// skipping it forever.
func (s *Store) AlreadyDone(hash string) bool {
	r, ok := s.Lookup(hash)
	if !ok {
		return false
	}
	if r.OutputPath == "" {
		return false
	}
	_, err := os.Stat(r.OutputPath)
	return err == nil
}

// Record stores a completed conversion. Like RememberHash it is written on the
// next budget tick rather than immediately, so a batch of tracks finishing
// together rewrites the file once instead of once per track.
func (s *Store) Record(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.SourceHash] = r
	s.dirtyConfig = true
	return s.deferSaveLocked()
}

// Records returns every record, newest first.
func (s *Store) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// Forget drops the record for a source digest.
func (s *Store) Forget(hash string) error {
	s.mu.Lock()
	delete(s.records, hash)
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

// Flush writes out anything the budget has held back. Callers that need the
// files on disk right now — a scan that has just finished, or shutdown — say so
// through this rather than by saving on every change.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

// saveLocked writes immediately. It is for changes the user just made in the
// settings panel, where losing the change to a crash a second later would be
// felt.
func (s *Store) saveLocked() error {
	s.dirtyConfig = true
	return s.flushLocked()
}

// deferSaveLocked writes now if the last write is old enough, and otherwise
// arranges for the write to happen when the budget runs out. The timer is what
// makes this safe to use from a code path that has no natural end: without it,
// the last change of a run would sit in memory until something else happened
// to trigger a save.
func (s *Store) deferSaveLocked() error {
	if time.Since(s.lastWrite) >= writeBudget {
		return s.flushLocked()
	}
	s.armLocked()
	return nil
}

func (s *Store) armLocked() {
	if s.pending != nil {
		return
	}
	s.pending = time.AfterFunc(writeBudget, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pending = nil
		_ = s.flushLocked()
	})
}

func (s *Store) flushLocked() error {
	if s.pending != nil {
		s.pending.Stop()
		s.pending = nil
	}
	if !s.dirtyConfig && !s.dirtyHashes {
		s.lastWrite = time.Now()
		return nil
	}

	var err error
	if s.dirtyConfig {
		if e := s.writeConfigLocked(); e != nil {
			err = e // still dirty: the next tick retries rather than dropping it
		} else {
			s.dirtyConfig = false
		}
	}
	if s.dirtyHashes {
		// The cache is rebuildable, so a failure here only means the next scan
		// re-hashes a few files; it is not worth surfacing.
		if writeAtomic(s.hashPath, s.hashes) == nil {
			s.dirtyHashes = false
		}
	}

	s.lastWrite = time.Now()
	if s.dirtyConfig || s.dirtyHashes {
		s.armLocked()
	}
	return err
}

func (s *Store) writeConfigLocked() error {
	payload := struct {
		Config  Config            `json:"config"`
		Records map[string]Record `json:"records"`
	}{s.config, s.records}
	return writeAtomic(s.configPath(), payload)
}

// writeAtomic writes JSON via a temporary file so a crash mid-write cannot
// leave a truncated file that fails to parse on the next start.
func writeAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ── scan filtering ────────────────────────────────────────────────────────

// ScanFilter is the decision a scan makes about one candidate file. It is
// carried out of the store as a closure so that the code listing a directory
// does not have to know how a minimum is spelled, and so that a scan and the
// "convert everything here" button cannot drift apart.
type ScanFilter func(path string, size int64) (bool, string)

// ScanFilterConfig builds the filter the current settings describe.
func ScanFilterConfig(cfg Config) ScanFilter {
	min := cfg.MinSize
	if !min.Enabled() {
		return func(string, int64) (bool, string) { return true, "" }
	}
	reason := "smaller than " + min.Description()
	return func(_ string, size int64) (bool, string) {
		if size < min.Bytes {
			return false, reason
		}
		return true, ""
	}
}

// ScanFilter is the filter the current settings describe.
func (s *Store) ScanFilter() ScanFilter { return ScanFilterConfig(s.Config()) }

// MeetsMinSize reports whether a path is big enough to be offered.
//
// A file that cannot be stat'ed is refused rather than guessed at: it is about
// to be opened and read, and one that has just been deleted or is unreadable
// would only fail later, further from the reason.
func (s *Store) MeetsMinSize(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	ok, _ := s.ScanFilter()(path, fi.Size())
	return ok, nil
}

// ErrNotFound is returned when a requested path is outside every configured
// root.
var ErrNotFound = errors.New("store: path not found")

// maxRememberedModels bounds what one vendor's cached model list may hold. A
// vendor that answers with something enormous must not be able to grow the
// settings file without limit; the suggestion list is a convenience, and a
// hundred names is far more than anyone scrolls.
const maxRememberedModels = 100

// RememberAIModel notes the model a provider was last used with.
//
// It is called when a batch starts, not only when the field is edited, because
// what the user wants back next time is the model that actually ran — including
// the case where they typed a name that is not in any list. An empty model
// forgets the entry, which puts the provider back on its documented default.
func (s *Store) RememberAIModel(provider, model string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil
	}
	model = strings.TrimSpace(model)
	return s.Update(func(c *Config) {
		if c.AIModels == nil {
			c.AIModels = map[string]string{}
		}
		if model == "" {
			delete(c.AIModels, provider)
			return
		}
		c.AIModels[provider] = model
	})
}

// RememberAIModelList caches the ids a vendor's model list returned.
func (s *Store) RememberAIModelList(provider string, ids []string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil
	}
	if len(ids) > maxRememberedModels {
		ids = ids[:maxRememberedModels]
	}
	return s.Update(func(c *Config) {
		if c.AIModelList == nil {
			c.AIModelList = map[string][]string{}
		}
		if len(ids) == 0 {
			delete(c.AIModelList, provider)
			return
		}
		c.AIModelList[provider] = append([]string(nil), ids...)
	})
}
