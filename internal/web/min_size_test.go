package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ncm-studio/internal/store"
)

// A scan has to leave out the files the size filter excludes and say why, and
// it has to be a decision the settings can change: the listing is the contract
// the page draws its rows from.
func TestFilesHonoursTheMinimumSize(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)
	cfg := st.Config()

	big := filepath.Join(cfg.Dir, "big.ncm")
	small := filepath.Join(cfg.Dir, "small.ncm")
	if err := os.WriteFile(big, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The default is 500 KB, so both are tiny and both are refused. The point
	// of the fixture is that the *setting* decides: 4 KB is over the line
	// below and under it now.
	var files []fileEntry
	getJSON(t, srv.URL+"/api/files", &files)
	if len(files) != 2 {
		t.Fatalf("got %d rows, want 2: the filter marks rows, it does not hide them", len(files))
	}
	for _, f := range files {
		if f.Skipped == "" {
			t.Errorf("%s was not marked as skipped under the 500 KB default", f.Name)
		}
		if f.Size == 0 {
			t.Errorf("%s has no size", f.Name)
		}
	}

	// Lower the minimum below 4 KB and the big one becomes convertible while
	// the 4-byte one stays out.
	postJSON(t, srv.URL+"/api/config", map[string]any{
		"minSize": map[string]any{"bytes": 2048, "unit": "kb"},
	})
	if got := st.Config().MinSize.Bytes; got != 2048 {
		t.Fatalf("min size = %d, want 2048", got)
	}
	getJSON(t, srv.URL+"/api/files", &files)
	for _, f := range files {
		switch f.Name {
		case "big.ncm":
			if f.Skipped != "" {
				t.Errorf("big.ncm is still skipped: %q", f.Skipped)
			}
		case "small.ncm":
			if f.Skipped == "" {
				t.Error("small.ncm is no longer skipped, but it is 4 bytes")
			}
		}
	}

	// Turning it off by clearing the field is the whole reason an empty number
	// is accepted rather than refused.
	postJSON(t, srv.URL+"/api/config", map[string]any{
		"minSize": map[string]any{"bytes": 0, "unit": "kb"},
	})
	getJSON(t, srv.URL+"/api/files", &files)
	for _, f := range files {
		if f.Skipped != "" {
			t.Errorf("%s is still skipped with the filter off: %q", f.Name, f.Skipped)
		}
	}

	// And the setting is on disk in that shape, not only in memory: a filter
	// that came back on at the next start would be worse than one that never
	// worked. Nothing is flushed by hand — a settings change does not wait for
	// the write budget.
	reopened, err := store.Open(st.Dir(), store.DefaultConfig(cfg.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Config().MinSize.Enabled() {
		t.Errorf("the filter came back on after a reload: %+v", reopened.Config().MinSize)
	}
}

// The audio listing is what the lyrics, cover, .lrc and info pages all draw
// from, so the same setting has to reach it — and a row the filter excludes
// must not be selectable, or a batch would rewrite a file the setting is there
// to keep out of the way.
//
// This test states the minimum it means to test rather than leaning on the
// default: the fixture is two kilobytes, and a test whose meaning depends on
// the shipped default would start passing for the wrong reason the day that
// default moves.
func TestAudioListingMarksFilteredRowsUnwritable(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, nil)

	// A minimum far below the fixture: the row must come back usable.
	if err := s.store.Update(func(c *store.Config) {
		c.MinSize = store.MinSize{Bytes: 1024, Unit: store.SizeKB}
	}); err != nil {
		t.Fatal(err)
	}

	var entries []audioEntry
	if err := json.Unmarshal(s.get(t, "/api/audio?dir="+s.dir), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if !entries[0].Writable || entries[0].Skipped != "" {
		t.Fatalf("the fixture was filtered out by a 1 KB minimum: %+v", entries[0])
	}

	// Raise the minimum past the fixture and the same file becomes a row that
	// is listed, explained, and out of reach.
	if err := s.store.Update(func(c *store.Config) {
		c.MinSize = store.MinSize{Bytes: 1 << 40, Unit: store.SizeGB}
	}); err != nil {
		t.Fatal(err)
	}
	entries = nil
	if err := json.Unmarshal(s.get(t, "/api/audio?dir="+s.dir), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the filtered file disappeared from the listing: %d rows", len(entries))
	}
	if entries[0].Writable {
		t.Error("a file below the minimum was reported writable")
	}
	if entries[0].Skipped == "" {
		t.Error("a file below the minimum came back with no reason")
	}
	if entries[0].Path != path {
		t.Errorf("path = %q, want %q", entries[0].Path, path)
	}
}
