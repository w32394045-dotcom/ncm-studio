package cover

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newLibrary(t *testing.T, hash string) (*Store, *Library) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "covers")
	st, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := st.Library(hash)
	if err != nil {
		t.Fatal(err)
	}
	return st, lib
}

func img(tag byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = tag
	}
	// A JPEG header, so the MIME sniffing has something to work with.
	copy(b, []byte{0xFF, 0xD8, 0xFF})
	return b
}

func TestAddKeepsOneEntryPerImage(t *testing.T) {
	_, lib := newLibrary(t, "hash-a")

	first, err := lib.Add(KindHistory, img(1, 100), "image/jpeg", "one.jpg")
	if err != nil {
		t.Fatal(err)
	}
	// The same bytes under a different name are the same picture; storing it
	// twice would make the history grow every time the user switched back.
	again, err := lib.Add(KindHistory, img(1, 100), "image/jpeg", "two.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != again.ID {
		t.Errorf("the same image produced two entries: %s and %s", first.ID, again.ID)
	}
	if got := len(lib.Entries()); got != 1 {
		t.Errorf("%d entries, want 1", got)
	}

	if _, err := lib.Add(KindHistory, img(2, 100), "image/jpeg", ""); err != nil {
		t.Fatal(err)
	}
	if got := len(lib.Entries()); got != 2 {
		t.Errorf("%d entries after a second image, want 2", got)
	}
}

// TestOriginalIsCapturedOnce is the promise behind the "original" slot: whatever
// the file carried when this tool first saw it stays recoverable, no matter how
// many times the cover is changed afterwards.
func TestOriginalIsCapturedOnce(t *testing.T) {
	_, lib := newLibrary(t, "hash-b")

	orig, err := lib.Add(KindOriginal, img(9, 500), "image/jpeg", "")
	if err != nil {
		t.Fatal(err)
	}
	if orig.ID != IDOriginal {
		t.Errorf("id = %q, want %q", orig.ID, IDOriginal)
	}

	later, err := lib.Add(KindOriginal, img(8, 900), "image/jpeg", "")
	if err != nil {
		t.Fatal(err)
	}
	if later.ID != IDOriginal || later.Size != orig.Size {
		t.Errorf("the original was replaced: %+v, want the %d-byte first one", later, orig.Size)
	}

	data, _, err := lib.Read(IDOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, img(9, 500)) {
		t.Error("the stored original is not the image that was captured first")
	}
}

func TestOriginalCannotBeDeleted(t *testing.T) {
	_, lib := newLibrary(t, "hash-c")
	if _, err := lib.Add(KindOriginal, img(1, 60), "image/jpeg", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Add(KindHistory, img(2, 60), "image/jpeg", ""); err != nil {
		t.Fatal(err)
	}

	if err := lib.Remove(IDOriginal); !errors.Is(err, ErrProtected) {
		t.Errorf("err = %v, want ErrProtected", err)
	}
	if len(lib.Entries()) != 2 {
		t.Error("the original was removed anyway")
	}
}

func TestRemoveHistoryDeletesTheFile(t *testing.T) {
	st, lib := newLibrary(t, "hash-d")

	entry, err := lib.Add(KindHistory, img(3, 120), "image/jpeg", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Root(), "hash-d", entry.ID+"."+entry.Ext)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stored image is not on disk: %v", err)
	}

	if err := lib.Remove(entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the image file outlived its entry")
	}
	if len(lib.Entries()) != 0 {
		t.Error("the entry is still listed")
	}
	if _, _, err := lib.Read(entry.ID); !errors.Is(err, ErrNoEntry) {
		t.Errorf("reading a removed entry: %v, want ErrNoEntry", err)
	}
}

func TestNeteaseEntryIsReplacedNotDuplicated(t *testing.T) {
	_, lib := newLibrary(t, "hash-e")

	first, err := lib.Add(KindNetease, img(4, 100), "image/jpeg", "NetEase")
	if err != nil {
		t.Fatal(err)
	}
	// A later fetch may well return a different render; the slot is a single
	// one, so the newer image takes its place.
	second, err := lib.Add(KindNetease, img(5, 200), "image/jpeg", "NetEase")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("ids differ: %q then %q", first.ID, second.ID)
	}
	if second.Size != 200 {
		t.Errorf("size = %d, want the newer 200-byte image", second.Size)
	}
	if got := len(lib.Entries()); got != 1 {
		t.Errorf("%d entries, want 1", got)
	}
}

func TestLibrarySurvivesReopen(t *testing.T) {
	st, lib := newLibrary(t, "hash-f")

	orig, err := lib.Add(KindOriginal, img(1, 80), "image/jpeg", "")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := lib.Add(KindHistory, img(2, 90), "image/png", "mine.png")
	if err != nil {
		t.Fatal(err)
	}
	lib.SetMusicID(987654)
	lib.RememberPath("/music/track.flac")
	lib.SetCurrent(hist.ID)

	reopened, err := st.Library("hash-f")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.MusicID() != 987654 {
		t.Errorf("music id = %d, want 987654", reopened.MusicID())
	}
	if reopened.Current() != hist.ID {
		t.Errorf("current = %q, want %q", reopened.Current(), hist.ID)
	}
	entries := reopened.Entries()
	if len(entries) != 2 {
		t.Fatalf("%d entries after reopen, want 2", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ID] = true
		if e.Digest == "" {
			t.Errorf("entry %s lost its digest", e.ID)
		}
	}
	if !seen[orig.ID] || !seen[hist.ID] {
		t.Errorf("entries = %v, want %s and %s", seen, orig.ID, hist.ID)
	}

	data, entry, err := reopened.Read(hist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, img(2, 90)) {
		t.Error("the reopened image does not match what was stored")
	}
	if entry.MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", entry.MIME)
	}
}

func TestDifferentTracksGetDifferentLibraries(t *testing.T) {
	st, _ := newLibrary(t, "track-one")

	one, err := st.Library("track-one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := st.Library("track-two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.Add(KindOriginal, img(1, 50), "image/jpeg", ""); err != nil {
		t.Fatal(err)
	}
	if got := len(two.Entries()); got != 0 {
		t.Errorf("the second track has %d entries, want none", got)
	}
}

func TestAddRejectsEmptyImage(t *testing.T) {
	_, lib := newLibrary(t, "hash-g")
	if _, err := lib.Add(KindHistory, nil, "image/jpeg", ""); err == nil {
		t.Error("an empty image was accepted")
	}
}
