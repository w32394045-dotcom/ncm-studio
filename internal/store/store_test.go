package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	st, err := Open(filepath.Join(root, "cfg"), DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	return st, root
}

func TestFingerprintTracksContentNotName(t *testing.T) {
	_, root := openStore(t)

	a := filepath.Join(root, "a.ncm")
	if err := os.WriteFile(a, []byte("header-bytes-then-audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, size1, _, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}

	// A rename must not change the digest: the record is keyed by content so
	// that moving a file does not force it to be decrypted a second time.
	b := filepath.Join(root, "renamed.ncm")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	h2, _, _, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("rename changed the fingerprint")
	}

	// Different content of the same length must be distinguished.
	if err := os.WriteFile(a, []byte("HEADER-bytes-then-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	h3, size3, _, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Error("different content produced the same fingerprint")
	}
	if size3 != size1 {
		t.Errorf("sizes differ unexpectedly: %d vs %d", size3, size1)
	}
}

func TestAlreadyDoneForgetsDeletedOutput(t *testing.T) {
	st, root := openStore(t)

	out := filepath.Join(root, "song.flac")
	if err := os.WriteFile(out, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	const hash = "deadbeef"
	if err := st.Record(Record{SourceHash: hash, OutputPath: out, Title: "Song"}); err != nil {
		t.Fatal(err)
	}

	if !st.AlreadyDone(hash) {
		t.Fatal("a recorded file with its output present should count as done")
	}

	// Deleting the output must re-queue the track rather than skip it forever.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if st.AlreadyDone(hash) {
		t.Error("a missing output should not count as done")
	}
}

func TestRecordSurvivesReopen(t *testing.T) {
	st, root := openStore(t)
	cfgDir := filepath.Join(root, "cfg")

	out := filepath.Join(root, "song.flac")
	if err := os.WriteFile(out, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Record(Record{SourceHash: "abc", OutputPath: out, MusicID: 42, Lyrics: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *Config) { c.Workers = 7; c.Lang = "ja" }); err != nil {
		t.Fatal(err)
	}

	again, err := Open(cfgDir, DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := again.Lookup("abc")
	if !ok || rec.MusicID != 42 || !rec.Lyrics {
		t.Errorf("record did not survive reopen: %+v (ok=%v)", rec, ok)
	}
	if cfg := again.Config(); cfg.Workers != 7 || cfg.Lang != "ja" {
		t.Errorf("config did not survive reopen: %+v", cfg)
	}
}

// TestFlushWritesBufferedChanges pins the trade the store makes: a change made
// inside the write budget is visible at once but reaches the file only when
// something flushes it. Scanning a directory touches both files once per file
// found, so saving on every change would rewrite them hundreds of times.
func TestFlushWritesBufferedChanges(t *testing.T) {
	st, root := openStore(t)
	cfgDir := filepath.Join(root, "cfg")

	// The very first write always goes out, so the budget window has to be
	// opened by one that lands before the interesting ones.
	if err := st.Record(Record{SourceHash: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Record(Record{SourceHash: "second"}); err != nil {
		t.Fatal(err)
	}
	st.RememberHash("/music/a.ncm", "hash-a", 100, 200)

	// Both are readable immediately, whatever the file says.
	if _, ok := st.Lookup("second"); !ok {
		t.Fatal("a buffered record is not visible to Lookup")
	}
	if _, ok := st.KnownHash("/music/a.ncm", 100, 200); !ok {
		t.Fatal("a buffered hash is not visible to KnownHash")
	}

	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	again, err := Open(cfgDir, DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Lookup("second"); !ok {
		t.Error("a record buffered inside the budget window did not survive Flush")
	}
	if _, ok := again.KnownHash("/music/a.ncm", 100, 200); !ok {
		t.Error("a hash buffered inside the budget window did not survive Flush")
	}
}

// TestBufferedChangesLandOnTheirOwn covers the safety net behind the budget: a
// change nobody flushes must still reach the file on its own, or a crash after
// the last write of a run would lose it.
func TestBufferedChangesLandOnTheirOwn(t *testing.T) {
	st, root := openStore(t)
	cfgDir := filepath.Join(root, "cfg")

	if err := st.Record(Record{SourceHash: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Record(Record{SourceHash: "second"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * writeBudget)
	for {
		again, err := Open(cfgDir, DefaultConfig(root))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := again.Lookup("second"); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a buffered record never reached the file without an explicit Flush")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCorruptConfigFallsBackToDefaults covers the promise Open makes: a
// half-written or hand-mangled settings file must not stop the tool starting.
func TestCorruptConfigFallsBackToDefaults(t *testing.T) {
	st, root := openStore(t)
	cfgDir := filepath.Join(root, "cfg")
	if err := st.Update(func(c *Config) { c.Workers = 9 }); err != nil {
		t.Fatal(err)
	}

	// A crash mid-write leaves truncated JSON; a hand-edit leaves valid JSON of
	// the wrong shape. Both must land on the defaults.
	for name, body := range map[string]string{
		"truncated":   `{"config":{"workers":`,
		"wrong shape": `{"config": "{not an object"}`,
	} {
		if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		again, err := Open(cfgDir, DefaultConfig(root))
		if err != nil {
			t.Fatalf("%s config: Open failed: %v", name, err)
		}
		if got := again.Config().Workers; got != 3 {
			t.Errorf("%s config: workers = %d, want the default 3", name, got)
		}
	}
}

func TestFingerprintCacheInvalidatesOnChange(t *testing.T) {
	st, root := openStore(t)
	path := filepath.Join(root, "a.ncm")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := st.FingerprintCached(path)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := st.FingerprintCached(path)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("cached fingerprint changed between calls")
	}

	// Rewrite with a different size; the cached digest must not be reused.
	if err := os.WriteFile(path, []byte("second-and-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	h3, err := st.FingerprintCached(path)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Error("cache served a stale fingerprint after the file changed")
	}
}
