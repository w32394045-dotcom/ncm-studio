package ncm

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// samplePath points at a real .ncm file. Tests that need actual audio skip
// when it is absent, so the suite still runs on a bare checkout.
const samplePath = "/storage/emulated/0/Download/netease/cloudmusic/Music/Lucenzo Don Omar - Danza Kuduro.ncm"

func openSample(t *testing.T) *Container {
	t.Helper()
	if _, err := os.Stat(samplePath); err != nil {
		t.Skipf("sample not available: %v", err)
	}
	c, err := Open(samplePath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestParseMetadata(t *testing.T) {
	c := openSample(t)

	if c.Meta == nil {
		t.Fatal("no metadata decoded")
	}
	if c.Meta.MusicID != 28722674 {
		t.Errorf("MusicID = %d, want 28722674", c.Meta.MusicID)
	}
	if c.Meta.MusicName != "Danza Kuduro" {
		t.Errorf("MusicName = %q", c.Meta.MusicName)
	}
	if c.Meta.Album != "Ritmo Do Brasil" {
		t.Errorf("Album = %q", c.Meta.Album)
	}
	if c.Meta.DurationMS != 201373 {
		t.Errorf("DurationMS = %d, want 201373", c.Meta.DurationMS)
	}

	// The whole point of the rewrite: every credited artist survives.
	names := c.Meta.ArtistNames()
	want := []string{"Lucenzo", "Don Omar"}
	if len(names) != len(want) {
		t.Fatalf("ArtistNames() = %q, want %q", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("artist[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	if got := c.Meta.DisplayName(); got != "Lucenzo, Don Omar - Danza Kuduro" {
		t.Errorf("DisplayName() = %q", got)
	}
	if len(c.Meta.TransNames) == 0 || c.Meta.TransNames[0] != "舞蹈《Kuduro》" {
		t.Errorf("TransNames = %q", c.Meta.TransNames)
	}
}

func TestCoverIsDecoded(t *testing.T) {
	c := openSample(t)
	if len(c.Cover) == 0 {
		t.Fatal("no cover extracted")
	}
	if c.CoverMIME != "image/jpeg" {
		t.Errorf("CoverMIME = %q, want image/jpeg", c.CoverMIME)
	}
}

func TestDetectFormatAndDecrypt(t *testing.T) {
	c := openSample(t)

	head, err := c.Head(64 << 10)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if got := DetectFormat(head); got != FormatFLAC {
		t.Fatalf("DetectFormat = %q, want flac", got)
	}
	if !bytes.HasPrefix(head, []byte("fLaC")) {
		t.Fatalf("head does not start with fLaC: % x", head[:8])
	}

	// Decrypt everything and confirm the byte count matches the container's
	// own accounting of the payload.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, c)
	if err != nil {
		t.Fatalf("copy audio: %v", err)
	}
	if n != c.AudioSize {
		t.Errorf("decrypted %d bytes, container reported %d", n, c.AudioSize)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("fLaC")) {
		t.Error("decrypted stream is not FLAC")
	}

	// A second pass must reproduce the first exactly; this catches any state
	// left behind in the keystream offset tracking.
	if err := c.Rewind(); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	var second bytes.Buffer
	if _, err := io.Copy(&second, c); err != nil {
		t.Fatalf("second copy: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), second.Bytes()) {
		t.Error("decryption is not deterministic across passes")
	}
}

func TestParseArtistsShapes(t *testing.T) {
	// A bare string.
	got := parseArtists([]byte(`"Solo Artist"`))
	if len(got) != 1 || got[0].Name != "Solo Artist" {
		t.Errorf("string form: %+v", got)
	}
	// A list of pairs, including one with a missing id.
	got = parseArtists([]byte(`[["A",1],["B",2],["C"]]`))
	want := []string{"A", "B", "C"}
	if len(got) != len(want) {
		t.Fatalf("pair form: %+v", got)
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("artist[%d] = %q, want %q", i, got[i].Name, w)
		}
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("ids not parsed: %+v", got)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"a/b:c*d?e":      "a_b_c_d_e",
		`AC/DC - T.N.T.`: "AC_DC - T.N.T",
		"  spaced  ":     "spaced",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
