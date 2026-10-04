package tag

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A real cover is the largest thing in a file's metadata by an order of
// magnitude, and the scan only needs to know that one is there. These tests pin
// the two halves of that: the reader must not need the bytes to report them, and
// the writer must still put them back.

// bigCover builds an image-shaped blob large enough that buffering it or not is
// visible in an allocation count.
func bigCover() []byte {
	img := make([]byte, 3<<20)
	// A JPEG signature so the MIME sniffing has something real to find.
	copy(img, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	for i := 4; i < len(img); i++ {
		img[i] = byte(i * 7)
	}
	return img
}

func writeFLACWithCover(t *testing.T, path string, cover []byte) {
	t.Helper()
	si := make([]byte, 34)
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40
	prefix := BuildFLACMetadata(&FLACSource{StreamInfo: append([]byte{0, 0, 0, 34}, si...)}, &Tags{
		Title:     "Cover Test",
		Artists:   []string{"Someone"},
		Cover:     cover,
		CoverMIME: "image/jpeg",
	})
	audio := make([]byte, 8192)
	for i := range audio {
		audio[i] = byte(i)
	}
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Inspect reports the cover without reading its bytes into memory.
func TestInspectDoesNotBufferTheCover(t *testing.T) {
	cover := bigCover()
	path := filepath.Join(t.TempDir(), "with-cover.flac")
	writeFLACWithCover(t, path, cover)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const reps = 3
	for i := 0; i < reps; i++ {
		info, err := Inspect(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.HasCover {
			t.Fatal("Inspect did not see the cover")
		}
		if info.Title != "Cover Test" {
			t.Fatalf("title = %q", info.Title)
		}
	}
	runtime.ReadMemStats(&after)

	perCall := (after.TotalAlloc - before.TotalAlloc) / reps
	// The window itself is the metadata chain; the cover must not add a copy of
	// itself on top of that. The chain here is a little over 3 MB, so anything
	// near 6 MB means the image was buffered as well.
	if perCall > uint64(len(cover))+2<<20 {
		t.Errorf("Inspect allocated %d bytes per call for a %d-byte cover: the picture is being buffered",
			perCall, len(cover))
	}
}

// The offset the fingerprint uses still counts the blocks whose payloads were
// skipped: a wrong offset there would digest the metadata instead of the audio.
func TestAudioOffsetCountsSkippedPictures(t *testing.T) {
	cover := bigCover()
	path := filepath.Join(t.TempDir(), "with-cover.flac")
	writeFLACWithCover(t, path, cover)

	format, offset, err := AudioOffset(path)
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatFLAC {
		t.Fatalf("format = %q", format)
	}
	want, err := metadataLength(path)
	if err != nil {
		t.Fatal(err)
	}
	if int(offset) != want {
		t.Errorf("AudioOffset = %d, want %d (the byte after the metadata chain)", offset, want)
	}

	// The same offset is what the cover library keys a track by, so a wrong
	// answer here would attach one track's artwork to another.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	audioLen := fi.Size() - offset
	if audioLen != 8192 {
		t.Errorf("audio payload = %d bytes, want the 8192 the fixture wrote", audioLen)
	}
}

// Writing lyrics back must keep the cover that was there, including when the
// chain was walked without buffering it.
func TestLyricsRewriteKeepsSkippedPicture(t *testing.T) {
	cover := bigCover()
	path := filepath.Join(t.TempDir(), "with-cover.flac")
	writeFLACWithCover(t, path, cover)

	before, err := ReadCover(path)
	if err != nil {
		t.Fatal(err)
	}
	if before == nil || !bytes.Equal(before.Data, cover) {
		t.Fatal("the fixture's cover did not read back")
	}

	if err := SetLyrics(path, Lyrics{Merged: "[00:01.000]hello"}, Credit{}, nil); err != nil {
		t.Fatalf("SetLyrics: %v", err)
	}

	after, err := ReadCover(path)
	if err != nil {
		t.Fatal(err)
	}
	if after == nil {
		t.Fatal("the cover is gone after the lyrics rewrite")
	}
	if !bytes.Equal(after.Data, cover) {
		t.Errorf("the cover changed: %d bytes before, %d after", len(cover), len(after.Data))
	}

	info, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasLyrics() {
		t.Error("the lyrics were not written")
	}
	if !info.HasCover {
		t.Error("Inspect no longer sees the cover")
	}
}

// metadataLength walks a FLAC file's chain with a fresh reader, so the test is
// not comparing the code under test with itself.
func metadataLength(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if len(raw) < 4 || string(raw[:4]) != "fLaC" {
		return 0, ErrNotFLAC
	}
	pos := 4
	for i := 0; i < maxMetadataBlocks; i++ {
		if pos+4 > len(raw) {
			return pos, nil
		}
		h := raw[pos : pos+4]
		last := h[0]&0x80 != 0
		length := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		pos += 4 + length
		if last {
			return pos, nil
		}
	}
	return pos, nil
}
