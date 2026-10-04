package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The size filter is a setting with a default, so the interesting cases are all
// about what happens to a config that does not mention it — the one written by
// an older build — versus one that deliberately says zero.
func TestMinSizeDefaultsAndOptOut(t *testing.T) {
	dir := t.TempDir()

	// A fresh install: the default the UI promises.
	if got := DefaultConfig("/home").MinSize; got != DefaultMinSize() {
		t.Errorf("DefaultConfig min size = %+v, want %+v", got, DefaultMinSize())
	}
	if DefaultMinSizeBytes != 500<<10 {
		t.Errorf("DefaultMinSizeBytes = %d, want 500 KB", DefaultMinSizeBytes)
	}

	// A config with no mention of the filter at all: written before it existed.
	// It has to come up with the default rather than with "no minimum", which
	// is what a plain json.Unmarshal of a missing field would produce.
	legacy := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.json"),
		[]byte(`{"config":{"dir":"/music","workers":2},"records":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(legacy, DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Config().MinSize; got.Bytes != DefaultMinSizeBytes || got.Unit != SizeKB {
		t.Errorf("legacy config min size = %+v, want the %d byte default in KB", got, DefaultMinSizeBytes)
	}

	// The same file, after the setting has been turned off. Zero is a value,
	// not an absence, and it has to survive a round trip through disk.
	off := filepath.Join(dir, "off")
	if err := os.MkdirAll(off, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(off, "config.json"),
		[]byte(`{"config":{"dir":"/music","workers":2,"minSize":{"bytes":0,"unit":"kb"}},"records":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(off, DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Config().MinSize; got.Enabled() {
		t.Errorf("an explicit zero came back as %+v, want no filter", got)
	}

	// The same thing through the program's own writer, which is the shape the
	// file really has: a settings object inside an envelope with the records
	// beside it. Reading a setting has to look inside that member — looking at
	// the envelope always answers "not set", which is the answer that means
	// "use the default", and that is how a saved filter would quietly come back
	// on at the next start.
	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	saved, err := Open(live, DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := saved.Update(func(c *Config) {
		c.MinSize = MinSize{Bytes: 7 << 20, Unit: SizeMB}
	}); err != nil {
		t.Fatal(err)
	}
	// The envelope really is what is on disk, so this test is about the real
	// file and not about a shape it made up.
	raw, err := os.ReadFile(filepath.Join(live, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"config"`)) || !bytes.Contains(raw, []byte(`"records"`)) {
		t.Fatalf("settings file is not the expected envelope: %s", raw)
	}
	back, err := Open(live, DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Config().MinSize; got != (MinSize{Bytes: 7 << 20, Unit: SizeMB}) {
		t.Errorf("saved min size came back as %+v, want 7 MB", got)
	}
}

func TestMinSizeArithmetic(t *testing.T) {
	cases := []struct {
		name   string
		amount float64
		unit   SizeUnit
		want   int64
	}{
		{"half a megabyte", 500, SizeKB, 500 << 10},
		{"one and a half megabytes", 1.5, SizeMB, 3 << 19},
		{"one gigabyte", 1, SizeGB, 1 << 30},
		{"zero means off", 0, SizeMB, 0},
		{"negative means off", -5, SizeMB, 0},
		{"an unknown unit falls back to KB", 2, SizeUnit("tb"), 2 << 10},
		{"clamped at the ceiling", 1e6, SizeGB, MaxMinSizeBytes},
	}
	for _, tc := range cases {
		if got := (MinSize{}).WithAmount(tc.amount, tc.unit).Bytes; got != tc.want {
			t.Errorf("%s: WithAmount(%v, %s) = %d bytes, want %d", tc.name, tc.amount, tc.unit, got, tc.want)
		}
	}

	// A unit that was never chosen presents as KB rather than as an empty
	// string in the dropdown.
	if got := (MinSize{Bytes: 1024}).UnitOrKB(); got != SizeKB {
		t.Errorf("UnitOrKB() = %q, want kb", got)
	}
	if got := (MinSize{Bytes: 2 << 20, Unit: SizeMB}).Amount(); got != 2 {
		t.Errorf("Amount() = %v, want 2", got)
	}
	if got := (MinSize{Bytes: 500 << 10, Unit: SizeKB}).Description(); got != "500 KB" {
		t.Errorf("Description() = %q, want %q", got, "500 KB")
	}
	if got := (MinSize{}).Description(); got != "no minimum size" {
		t.Errorf("Description() of the zero value = %q", got)
	}
}

func TestParseMinSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"500kb", 500 << 10},
		{"500 KB", 500 << 10},
		{"1.5mb", 3 << 19},
		{"2GB", 2 << 30},
		{"", 0},
		{"0", 0},
		{"nonsense", 0},
	}
	for _, tc := range cases {
		if got := ParseMinSize(tc.in).Bytes; got != tc.want {
			t.Errorf("ParseMinSize(%q) = %d bytes, want %d", tc.in, got, tc.want)
		}
	}
}

// A scan filter is a closure the callers hold for the length of one listing, so
// it must not be affected by a settings change made while that listing runs.
func TestScanFilterIsFixedWhenBuilt(t *testing.T) {
	cfg := DefaultConfig("/home")
	filter := ScanFilterConfig(cfg)

	big := int64(2 << 20)
	if ok, reason := filter("a.ncm", big); !ok {
		t.Errorf("a 2 MB file was filtered out: %s", reason)
	}
	if ok, reason := filter("b.ncm", 1024); ok || reason == "" {
		t.Errorf("a 1 KB file was allowed (%v, %q), want it out with a reason", ok, reason)
	}

	// The same file under a filter that was built with the setting off.
	cfg.MinSize = MinSize{}
	if ok, _ := ScanFilterConfig(cfg)("b.ncm", 1024); !ok {
		t.Error("a filter built with no minimum still left a file out")
	}
}

func TestScanFilterReadsTheStore(t *testing.T) {
	st, err := Open(t.TempDir(), DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *Config) { c.MinSize = MinSize{Bytes: 1 << 20, Unit: SizeMB} }); err != nil {
		t.Fatal(err)
	}

	small := filepath.Join(t.TempDir(), "small.ncm")
	if err := os.WriteFile(small, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.MeetsMinSize(small); err != nil || ok {
		t.Errorf("MeetsMinSize(small) = %v, %v; want false, nil", ok, err)
	}
	if _, err := st.MeetsMinSize(filepath.Join(t.TempDir(), "gone.ncm")); err == nil {
		t.Error("a missing file was reported as fine")
	}
}

// A patch may carry the unit on its own, which is how the page remembers how
// the number was said without moving the threshold.
func TestPatchKeepsTheUnitAndTheLine(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Patch([]byte(`{"minSize":{"bytes":1048576,"unit":"mb"}}`)); err != nil {
		t.Fatal(err)
	}
	// Then a patch that carries the unit on its own, which is what the dropdown
	// sends when it is moved without the number being touched: the remembered
	// unit changes and the line does not.
	if err := st.Patch([]byte(`{"minSizeUnit":"gb"}`)); err != nil {
		t.Fatal(err)
	}
	want := MinSize{Bytes: 1 << 20, Unit: SizeGB}
	if got := st.Config().MinSize; got != want {
		t.Errorf("min size = %+v, want %+v: a unit change must not move the line", got, want)
	}

	// A patch that says nothing about the filter leaves it alone.
	if err := st.Patch([]byte(`{"lang":"ja"}`)); err != nil {
		t.Fatal(err)
	}
	if again := st.Config().MinSize; again != want {
		t.Errorf("min size changed to %+v by an unrelated patch", again)
	}

	// A patch the store decodes but cannot make sense of must not poison the
	// settings file: a byte count outside the range is clamped, not stored.
	if err := st.Patch([]byte(`{"minSize":{"bytes":9999999999999999,"unit":"gb"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := st.Config().MinSize.Bytes; got != MaxMinSizeBytes {
		t.Errorf("bytes = %d, want them clamped to %d", got, MaxMinSizeBytes)
	}

	// And it is on disk in that shape, not only in memory: the next start has
	// to read the same setting back. Nothing is flushed by hand here, because
	// a settings change is one of the writes that must not wait for the budget.
	reopened, err := Open(dir, DefaultConfig("/home"))
	if err != nil {
		t.Fatal(err)
	}
	if again := reopened.Config().MinSize; again != st.Config().MinSize {
		t.Errorf("reopened min size = %+v, want %+v", again, st.Config().MinSize)
	}
	if lang := reopened.Config().Lang; lang != "ja" {
		t.Errorf("reopened lang = %q, want ja", lang)
	}
}
