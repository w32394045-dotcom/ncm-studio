package tag

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The in-place cover rewrite is the difference between changing artwork on a
// 500 MB track costing half a gigabyte of I/O and costing a few kilobytes. It
// only applies when the chain carries padding, which every encoder that expects
// its tags to be edited does — but which our own writer does not, so a fixture
// has to build one.

// flacWithPadding writes a FLAC whose metadata chain ends in a PADDING block,
// the way a normal encoder leaves a file. audioBytes of payload follow the
// chain, which is where FLAC frames live — appending them to a finished file
// instead would put them behind the padding block and produce a chain no reader
// can walk.
func flacWithPadding(t *testing.T, path string, cover []byte, padding int) {
	flacWithPaddingAndAudio(t, path, cover, padding, 65536)
}

func flacWithPaddingAndAudio(t *testing.T, path string, cover []byte, padding, audioBytes int) {
	t.Helper()
	si := make([]byte, 34)
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40 // 44100 Hz
	chain := BuildFLACMetadata(&FLACSource{StreamInfo: append([]byte{0, 0, 0, 34}, si...)}, &Tags{
		Title:     "Padded",
		Artists:   []string{"Someone"},
		Cover:     cover,
		CoverMIME: "image/jpeg",
	})
	// BuildFLACMetadata marks its last block as final; a padding block after it
	// means that flag has to come off.
	clearLastFlag(chain)

	var buf bytes.Buffer
	buf.Write(chain)
	hdr := []byte{blockPadding | 0x80, byte(padding >> 16), byte(padding >> 8), byte(padding)}
	buf.Write(hdr)
	buf.Write(make([]byte, padding))

	audio := make([]byte, audioBytes)
	for i := range audio {
		audio[i] = byte(i * 3)
	}
	buf.Write(audio)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clearLastFlag walks the block headers in a rendered chain and clears the
// is-last bit on all of them, which a caller appending padding must do.
func clearLastFlag(chain []byte) {
	pos := 4
	for pos+4 <= len(chain) {
		chain[pos] &^= 0x80
		length := int(chain[pos+1])<<16 | int(chain[pos+2])<<8 | int(chain[pos+3])
		pos += 4 + length
	}
}

func TestCoverRewriteInPlace(t *testing.T) {
	oldCover := jpegBlob(40 << 10)
	newCover := jpegBlob(90 << 10)
	path := filepath.Join(t.TempDir(), "padded.flac")
	flacWithPadding(t, path, oldCover, 256<<10)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, audioOff, err := AudioOffset(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetCover(path, newCover, "image/jpeg", nil); err != nil {
		t.Fatalf("SetCover: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// The two things that make it "in place": the file did not change length,
	// and the bytes after the metadata did not move.
	if len(after) != len(before) {
		t.Errorf("file length changed: %d -> %d (the rewrite copied the audio)", len(before), len(after))
	}
	if !bytes.Equal(before[audioOff:], after[audioOff:]) {
		t.Error("the audio payload changed")
	}
	if beforeInfo.ModTime() == afterInfo.ModTime() && beforeInfo.Size() == afterInfo.Size() {
		// Not a failure — a fast filesystem can do this inside one clock tick —
		// but it means the test cannot lean on the timestamp either.
		t.Log("mtime unchanged; the size and payload checks are what matter")
	}

	// The artwork really was replaced, and the rest of the chain survived.
	art, err := ReadCover(path)
	if err != nil || art == nil {
		t.Fatalf("no cover after the rewrite: %v", err)
	}
	if !bytes.Equal(art.Data, newCover) {
		t.Errorf("cover is %d bytes, want the %d-byte replacement", len(art.Data), len(newCover))
	}
	info, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Padded" || len(info.Artists) != 1 {
		t.Errorf("the other tags did not survive: %+v", info)
	}
	if !info.HasCover {
		t.Error("Inspect cannot see the cover")
	}
}

// A cover too large for the padding must still work; it just costs the copy.
func TestCoverRewriteFallsBackWhenItDoesNotFit(t *testing.T) {
	oldCover := jpegBlob(40 << 10)
	hugeCover := jpegBlob(2 << 20)
	path := filepath.Join(t.TempDir(), "small-pad.flac")
	flacWithPadding(t, path, oldCover, 4<<10) // far less room than 2 MB needs

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCover(path, hugeCover, "image/jpeg", nil); err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Errorf("the file did not grow to hold a %d-byte cover: %d -> %d", len(hugeCover), len(before), len(after))
	}
	art, err := ReadCover(path)
	if err != nil || art == nil {
		t.Fatalf("no cover after the fallback: %v", err)
	}
	if !bytes.Equal(art.Data, hugeCover) {
		t.Errorf("cover is %d bytes, want %d", len(art.Data), len(hugeCover))
	}
	// The audio has to have survived the copy as well: the fixture's payload is
	// its last 64 KiB, and it must still be the same bytes at the end.
	const payload = 65536
	if !bytes.Equal(before[len(before)-payload:], after[len(after)-payload:]) {
		t.Error("the audio payload changed across the fallback rewrite")
	}
}

// Removing the cover has to keep working, which is the one case the in-place
// path deliberately declines.
func TestRemoveCoverStillWorksWithPadding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "padded.flac")
	flacWithPadding(t, path, jpegBlob(30<<10), 64<<10)

	if err := SetCover(path, nil, "", nil); err != nil {
		t.Fatalf("SetCover(nil): %v", err)
	}
	art, err := ReadCover(path)
	if err != nil {
		t.Fatal(err)
	}
	if art != nil {
		t.Errorf("the cover is still there: %d bytes", len(art.Data))
	}
	if info, err := Inspect(path); err != nil {
		t.Fatal(err)
	} else if info.HasCover {
		t.Error("Inspect still reports a cover")
	}
}

// jpegBlob builds a JPEG-signature blob of about the given size.
func jpegBlob(size int) []byte {
	out := make([]byte, size)
	copy(out, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'})
	for i := 10; i < len(out); i++ {
		out[i] = byte(i*17 + size)
	}
	out[len(out)-2], out[len(out)-1] = 0xFF, 0xD9
	return out
}
