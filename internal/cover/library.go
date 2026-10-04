// Package cover keeps the artwork history for each decrypted track.
//
// A track is keyed by the fingerprint of its audio payload rather than by its
// path, so renaming or moving the file does not orphan its covers, and editing
// the artwork does not either — see tag.AudioFingerprint.
package cover

import (
	"crypto/sha256"
	"encoding/hex"
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

// Kind groups an entry in the UI.
type Kind string

const (
	// KindOriginal is the artwork the track had the first time this tool saw
	// it. It is kept apart from the rest and can never be deleted, because it
	// is the one image that cannot be recovered from anywhere else.
	KindOriginal Kind = "original"
	// KindNetease is artwork pulled from the NetEase API, which can be a larger
	// render than the one shipped inside the .ncm container.
	KindNetease Kind = "netease"
	// KindHistory is artwork the user applied. Every change adds one.
	KindHistory Kind = "history"
)

// IDOriginal and IDNetease are fixed ids, since a track has at most one of each.
const (
	IDOriginal = "original"
	IDNetease  = "netease"
)

// Entry is one stored image.
type Entry struct {
	ID    string    `json:"id"`
	Kind  Kind      `json:"kind"`
	MIME  string    `json:"mime"`
	Ext   string    `json:"ext"`
	Size  int64     `json:"size"`
	At    time.Time `json:"at"`
	Label string    `json:"label,omitempty"`
	// Digest identifies the image's bytes, so the same picture applied twice is
	// stored once however it was added.
	Digest string `json:"digest"`
}

// meta is the on-disk index for one track.
type meta struct {
	AudioHash string  `json:"audioHash"`
	Path      string  `json:"path,omitempty"`
	MusicID   int64   `json:"musicId,omitempty"`
	Current   string  `json:"current,omitempty"`
	Entries   []Entry `json:"entries"`
}

// Library is the cover set for a single track.
type Library struct {
	dir  string
	hash string

	mu   sync.Mutex
	data meta
}

// Store hands out libraries and keeps one instance per track, so concurrent
// requests for the same track serialise instead of clobbering each other's
// writes to the index.
type Store struct {
	root string

	mu   sync.Mutex
	libs map[string]*Library
}

// ErrProtected reports an attempt to delete the original artwork.
var ErrProtected = errors.New("cover: the original artwork cannot be deleted")

// ErrNoEntry reports an unknown cover id.
var ErrNoEntry = errors.New("cover: no such cover")

// NewStore opens the cover root, creating it if needed.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root, libs: map[string]*Library{}}, nil
}

// Root returns the directory covers are kept under.
func (s *Store) Root() string { return s.root }

// Library returns the cover set for a track, loading it on first use.
func (s *Store) Library(hash string) (*Library, error) {
	if hash == "" {
		return nil, errors.New("cover: empty track fingerprint")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if lib, ok := s.libs[hash]; ok {
		return lib, nil
	}
	dir := filepath.Join(s.root, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lib := &Library{dir: dir, hash: hash}
	lib.data = meta{AudioHash: hash}
	if raw, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		var loaded meta
		if json.Unmarshal(raw, &loaded) == nil {
			loaded.AudioHash = hash
			lib.data = loaded
		}
	}
	s.libs[hash] = lib
	return lib, nil
}

// Entries returns the stored covers, newest first within each kind.
//
// The list is never nil, because it goes over JSON as it is: a nil slice
// marshals as null, and the page filters this list — a track with nothing in
// the library yet would fail to open the editor instead of showing it empty.
func (l *Library) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]Entry{}, l.data.Entries...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// Current returns the id of the cover last applied, which is a hint for the UI
// rather than the truth — the file itself is the truth.
func (l *Library) Current() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Current
}

// MusicID returns the NetEase song id recorded for this track, if any.
func (l *Library) MusicID() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.MusicID
}

// SetMusicID records the song id so the artwork can be fetched later even after
// the file is renamed.
func (l *Library) SetMusicID(id int64) {
	if id == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data.MusicID = id
	_ = l.saveLocked()
}

// RememberPath notes where the track currently lives, purely so the workspace
// stays legible to someone browsing it by hand.
func (l *Library) RememberPath(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.data.Path == path {
		return
	}
	l.data.Path = path
	_ = l.saveLocked()
}

// Find returns one entry by id.
func (l *Library) Find(id string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.data.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Read returns the bytes of one stored cover.
func (l *Library) Read(id string) ([]byte, Entry, error) {
	e, ok := l.Find(id)
	if !ok {
		return nil, Entry{}, ErrNoEntry
	}
	data, err := os.ReadFile(filepath.Join(l.dir, id+"."+e.Ext))
	if err != nil {
		return nil, Entry{}, err
	}
	return data, e, nil
}

// Add stores an image and returns its entry.
//
// Identical artwork is not stored twice: an image that is already in the set
// comes back as the entry that is already there, which makes switching back and
// forth between two covers leave the history the same size as switching once.
// The original is captured once and never replaced, so the very first artwork
// this tool saw stays recoverable however many times the cover is changed.
func (l *Library) Add(kind Kind, data []byte, mime, label string) (Entry, error) {
	if len(data) == 0 {
		return Entry{}, errors.New("cover: empty image")
	}
	digest := contentDigest(data)
	mime = normaliseMIME(mime, data)
	ext := extensionFor(mime)

	l.mu.Lock()
	defer l.mu.Unlock()

	// This exact image is already stored, so applying it again is a no-op
	// rather than a new history entry.
	for _, e := range l.data.Entries {
		if e.Digest == digest {
			return e, nil
		}
	}

	switch kind {
	case KindOriginal:
		// Captured once, on first contact with the track. Anything already
		// there wins, because the point of this slot is to preserve what the
		// file originally had.
		if e, ok := l.entryLocked(IDOriginal); ok {
			return e, nil
		}
	case KindNetease:
		// A freshly fetched render replaces the stored one but keeps its id,
		// which is fixed so references to it stay valid.
		if old, ok := l.entryLocked(IDNetease); ok {
			os.Remove(filepath.Join(l.dir, old.ID+"."+old.Ext))
			l.data.Entries = removeEntry(l.data.Entries, IDNetease)
		}
	}

	id := contentID(kind, digest)
	e := Entry{
		ID:     id,
		Kind:   kind,
		MIME:   mime,
		Ext:    ext,
		Size:   int64(len(data)),
		At:     time.Now(),
		Label:  label,
		Digest: digest,
	}
	if err := os.WriteFile(filepath.Join(l.dir, id+"."+ext), data, 0o644); err != nil {
		return Entry{}, err
	}
	l.data.Entries = append(l.data.Entries, e)
	if err := l.saveLocked(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// SetCurrent records which cover was last written into the file.
func (l *Library) SetCurrent(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.data.Current == id {
		return // nothing changed, and this is called on every manifest read
	}
	l.data.Current = id
	_ = l.saveLocked()
}

// Remove deletes one stored cover from the library. The audio file is not
// touched; use SetCover with no image for that.
func (l *Library) Remove(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entryLocked(id)
	if !ok {
		return ErrNoEntry
	}
	if e.Kind == KindOriginal {
		return ErrProtected
	}
	if err := os.Remove(filepath.Join(l.dir, e.ID+"."+e.Ext)); err != nil && !os.IsNotExist(err) {
		return err
	}
	l.data.Entries = removeEntry(l.data.Entries, id)
	if l.data.Current == id {
		l.data.Current = ""
	}
	return l.saveLocked()
}

func (l *Library) entryLocked(id string) (Entry, bool) {
	for _, e := range l.data.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

func (l *Library) saveLocked() error {
	raw, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.dir, "meta.json"))
}

func contentID(kind Kind, digest string) string {
	switch kind {
	case KindOriginal:
		return IDOriginal
	case KindNetease:
		return IDNetease
	default:
		return "h-" + digest
	}
}

// Digest identifies an image by its bytes. The caller uses it to work out which
// stored cover a file is currently carrying.
func Digest(data []byte) string { return contentDigest(data) }

func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

func removeEntry(entries []Entry, id string) []Entry {
	out := entries[:0]
	for _, e := range entries {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return out
}

// mimeAliases maps the spellings servers actually use onto the ones the tag
// formats specify. NetEase answers with "image/jpg", and writing that into a
// FLAC PICTURE block or an ID3 APIC frame produces artwork that conforming
// players are entitled to ignore.
var mimeAliases = map[string]string{
	"image/jpg":   "image/jpeg",
	"image/jpe":   "image/jpeg",
	"image/x-png": "image/png",
}

func normaliseMIME(mime string, data []byte) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if alias, ok := mimeAliases[mime]; ok {
		mime = alias
	}
	if strings.HasPrefix(mime, "image/") {
		return mime
	}
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	}
	return "image/jpeg"
}

func extensionFor(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "jpg"
	}
}

// SupportedImage reports whether a content type is one this package stores.
func SupportedImage(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}

// Describe renders an entry for a log line.
func (e Entry) Describe() string {
	return fmt.Sprintf("%s (%s, %d bytes)", e.ID, e.MIME, e.Size)
}
