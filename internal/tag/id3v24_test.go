package tag

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Tests for ID3v2.4 tags.
//
// 2.4 differs from 2.3 in one respect that matters here: frame sizes are
// synchsafe, seven bits per byte, where 2.3 uses a plain big-endian integer.
// The two encodings agree only below 128, so a 2.4 frame of 128 bytes or more
// read as 2.3 appears larger than it is, and the walk runs past the frames that
// follow it — either swallowing them or, when the inflated size runs off the
// end, giving up and reporting no frames at all. A rewrite that trusts that
// answer writes a tag containing only what it replaced.
//
// The fixtures are assembled from literal field values rather than with the
// code under test, so a bug in the reader cannot make these tests agree with it.

// v24Body is a v2.4 tag body holding three frames: a short TIT2, an APIC well
// over the 128-byte boundary where the size encodings diverge, and a TXXX
// behind it. The TXXX is the canary — it is what the pre-fix walk lost.
func v24Body() []byte {
	var body []byte
	body = append(body, v24Frame("TIT2", [2]byte{}, []byte{3, 'H', 'i'})...)
	body = append(body, v24Frame("APIC", [2]byte{}, append([]byte{0, 'i', '/', 'j', 0, 3, 0}, make([]byte, 200)...))...)
	body = append(body, v24Frame("TXXX", [2]byte{}, []byte{0, 'k', 0, 'v'})...)
	return body
}

// v24Frame builds one v2.4 frame, encoding the size seven bits per byte.
func v24Frame(id string, flags [2]byte, body []byte) []byte {
	out := []byte(id)
	out = append(out, synchsafeBytes(len(body))...)
	out = append(out, flags[0], flags[1])
	return append(out, body...)
}

// v24Tag wraps a body in a v2.4 tag header. headerFlags carries the
// unsynchronisation, extended-header and footer bits.
func v24Tag(headerFlags byte, body []byte) []byte {
	tag := []byte{'I', 'D', '3', 4, 0, headerFlags}
	tag = append(tag, synchsafeBytes(len(body))...)
	return append(tag, body...)
}

func synchsafeBytes(v int) []byte {
	return []byte{byte(v >> 21 & 0x7f), byte(v >> 14 & 0x7f), byte(v >> 7 & 0x7f), byte(v & 0x7f)}
}

// framesIn walks a tag of either version and returns id -> bodies, applying
// whichever size encoding that version uses. Written out longhand so the
// assertions do not rest on the code they are checking.
func framesIn(t *testing.T, tag []byte) map[string][][]byte {
	t.Helper()
	if string(tag[:3]) != "ID3" {
		t.Fatalf("bad ID3 signature: % x", tag[:3])
	}
	version := tag[3]
	size := int(tag[6]&0x7f)<<21 | int(tag[7]&0x7f)<<14 | int(tag[8]&0x7f)<<7 | int(tag[9]&0x7f)
	if size > len(tag)-10 {
		t.Fatalf("declared size %d exceeds the %d bytes present", size, len(tag)-10)
	}

	frames := map[string][][]byte{}
	pos := 10
	for pos+frameHeaderSize <= 10+size {
		id := string(tag[pos : pos+4])
		if id == "\x00\x00\x00\x00" {
			break // padding
		}
		var n int
		if version >= 4 {
			n = int(tag[pos+4]&0x7f)<<21 | int(tag[pos+5]&0x7f)<<14 |
				int(tag[pos+6]&0x7f)<<7 | int(tag[pos+7]&0x7f)
		} else {
			n = int(binary.BigEndian.Uint32(tag[pos+4 : pos+8]))
		}
		if pos+frameHeaderSize+n > 10+size {
			t.Fatalf("frame %q overruns the tag", id)
		}
		frames[id] = append(frames[id], tag[pos+frameHeaderSize:pos+frameHeaderSize+n])
		pos += frameHeaderSize + n
	}
	return frames
}

// TestV24FixtureIsWellFormed checks the fixture against the reader written for
// these tests, so a later failure points at the code under test rather than at
// a mistyped literal.
func TestV24FixtureIsWellFormed(t *testing.T) {
	tag := v24Tag(0, v24Body())
	frames := framesIn(t, tag)
	if got := len(frames["TIT2"]); got != 1 {
		t.Fatalf("TIT2 frames = %d, want 1", got)
	}
	if got := len(frames["APIC"][0]); got != 207 {
		t.Fatalf("APIC body = %d bytes, want 207", got)
	}
	if got := len(frames["TXXX"]); got != 1 {
		t.Fatalf("TXXX frames = %d, want 1", got)
	}
}

// TestParseFrameListReadsV24Sizes is the parser-level regression test. Reading
// the 207-byte APIC as a plain big-endian size inflates it to 328, which runs
// off the end of the 251-byte body and yielded no frames at all.
func TestParseFrameListReadsV24Sizes(t *testing.T) {
	frames, clean, err := parseFrameList(v24Body(), 4)
	if err != nil {
		t.Fatalf("parseFrameList: %v", err)
	}
	if !clean {
		t.Error("parseFrameList reported a partial parse of a well-formed tag")
	}
	var ids []string
	for _, fr := range frames {
		ids = append(ids, fr.ID)
	}
	want := []string{"TIT2", "APIC", "TXXX"}
	if len(ids) != len(want) {
		t.Fatalf("parsed frames = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("parsed frames = %v, want %v", ids, want)
		}
	}
	if got := len(frames[1].Body); got != 207 {
		t.Errorf("APIC body = %d bytes, want 207", got)
	}
}

// TestParseFrameListRejectsPartialWalks pins the signal the write path relies
// on: a walk that stops anywhere but padding did not understand the tag.
func TestParseFrameListRejectsPartialWalks(t *testing.T) {
	// Cut the body mid-frame, so the last frame's declared size runs off the
	// end. The walk stops there and what follows is not padding.
	body := v24Body()
	if frames, clean, err := parseFrameList(body[:len(body)-3], 4); err != nil || clean {
		t.Errorf("parseFrameList(truncated) = %d frames, clean %v, err %v; want clean false",
			len(frames), clean, err)
	}

	// Trailing bytes that are neither a frame nor padding: the declared size
	// says they belong to the tag, but nothing explains them.
	tail := append(append([]byte{}, body...), 0xFF)
	if _, clean, err := parseFrameList(tail, 4); err != nil || clean {
		t.Errorf("parseFrameList(unexplained tail) = clean %v, err %v; want clean false", clean, err)
	}

	// Pad the same body properly and the walk is clean again.
	padded := append(append([]byte{}, body...), make([]byte, 8)...)
	if _, clean, err := parseFrameList(padded, 4); err != nil || !clean {
		t.Errorf("parseFrameList(padded) = clean %v, err %v; want clean true", clean, err)
	}
}

// v24Fixture writes a v2.4-tagged file with an audio payload after the tag.
func v24Fixture(t *testing.T, audio []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "track.mp3")
	data := append(v24Tag(0, v24Body()), audio...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCoverEditKeepsV24Frames is the regression test for the corruption this
// parser fix addresses. Before it, editing the cover of a v2.4 file whose tag
// held any frame of 128 bytes or more wrote a tag containing nothing but the
// new picture — every other frame in the file was silently discarded.
func TestCoverEditKeepsV24Frames(t *testing.T) {
	audio := audioPayload()
	path := v24Fixture(t, audio)

	if err := SetCover(path, []byte("new-cover-bytes"), "image/jpeg", nil); err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	frames := framesIn(t, data)
	if got := len(frames["TIT2"]); got != 1 {
		t.Errorf("TIT2 frames after cover edit = %d, want 1 (the title was destroyed)", got)
	}
	if got := len(frames["TXXX"]); got != 1 {
		t.Errorf("TXXX frames after cover edit = %d, want 1 (the frame behind the picture was destroyed)", got)
	}
	if got := len(frames["APIC"]); got != 1 {
		t.Errorf("APIC frames = %d, want 1", got)
	}
	if !bytes.HasSuffix(data, audio) {
		t.Error("audio payload changed")
	}
}

// TestLyricsEditKeepsV24Frames is the same guarantee for the lyrics path.
func TestLyricsEditKeepsV24Frames(t *testing.T) {
	audio := audioPayload()
	path := v24Fixture(t, audio)

	l := Lyrics{Merged: "[00:01.00] line", Original: "[00:01.00] line", Translation: "[00:01.00] 译文"}
	if err := SetLyrics(path, l, Credit{MusicID: 2008994719}, nil); err != nil {
		t.Fatalf("SetLyrics: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	frames := framesIn(t, data)
	if got := len(frames["TIT2"]); got != 1 {
		t.Errorf("TIT2 frames after lyrics edit = %d, want 1 (the title was destroyed)", got)
	}
	if got := len(frames["USLT"]); got != 1 {
		t.Errorf("USLT frames = %d, want 1", got)
	}
	// The fixture's own TXXX must survive alongside the ones this rewrite adds
	// for the individual timelines.
	want := []byte{0, 'k', 0, 'v'}
	found := false
	for _, body := range frames["TXXX"] {
		if bytes.Equal(body, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("the fixture's TXXX was destroyed; TXXX frames = %d", len(frames["TXXX"]))
	}
	if !bytes.HasSuffix(data, audio) {
		t.Error("audio payload changed")
	}
}

// TestV24TagStaysV24 pins the version: a frame's flag bits mean different
// things in the two versions, so writing a 2.4 frame under a 2.3 header would
// misread it. A 2.4 file must come back out as 2.4.
func TestV24TagStaysV24(t *testing.T) {
	path := v24Fixture(t, audioPayload())
	if err := SetLyrics(path, Lyrics{Merged: "x"}, Credit{}, nil); err != nil {
		t.Fatalf("SetLyrics: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[3] != 4 {
		t.Errorf("rewrote a v2.4 tag as v2.%d", data[3])
	}
	// A freshly tagged file with no source tag has no version to inherit. It
	// has to start with an MPEG frame sync, or it is not an MP3 at all.
	raw := audioPayload()
	raw[0], raw[1] = 0xFF, 0xFB
	bare := filepath.Join(t.TempDir(), "bare.mp3")
	if err := os.WriteFile(bare, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetLyrics(bare, Lyrics{Merged: "x"}, Credit{}, nil); err != nil {
		t.Fatalf("SetLyrics(bare): %v", err)
	}
	out, err := os.ReadFile(bare)
	if err != nil {
		t.Fatal(err)
	}
	if out[3] != id3v2Version {
		t.Errorf("bare file tagged as v2.%d, want v2.%d", out[3], id3v2Version)
	}
}

// TestStatusFlagsSurviveRewrite checks that a frame's flag bytes come through
// untouched. Flags[0] describes the frame rather than its encoding, so dropping
// them loses information even though the body copies cleanly.
func TestStatusFlagsSurviveRewrite(t *testing.T) {
	flags := [2]byte{0x40, 0x00} // tag alter preserve
	body := v24Frame("TIT2", flags, []byte{3, 'H', 'i'})
	body = append(body, v24Frame("APIC", [2]byte{}, make([]byte, 200))...)

	path := filepath.Join(t.TempDir(), "track.mp3")
	if err := os.WriteFile(path, v24Tag(0, body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetCover(path, []byte("cover"), "image/jpeg", nil); err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(data, []byte("TIT2"))
	if idx < 0 {
		t.Fatal("TIT2 is missing")
	}
	if got := [2]byte{data[idx+8], data[idx+9]}; got != flags {
		t.Errorf("TIT2 flags = % x, want % x", got, flags)
	}
}

// TestWritePathRefusesTagsItCannotRoundTrip covers the tags this package does
// not fully understand. The write paths must fail rather than write a tag
// rebuilt from a partial read, and the file must be left exactly as it was.
func TestWritePathRefusesTagsItCannotRoundTrip(t *testing.T) {
	body := v24Body()

	// v2.2 uses three-character ids and three-byte sizes.
	v22 := []byte{'I', 'D', '3', 2, 0, 0}
	v22 = append(v22, synchsafeBytes(len(body))...)
	v22 = append(v22, body...)

	// A format flag that changes what the body bytes mean.
	flagged := v24Frame("TIT2", [2]byte{0, 0x08}, []byte{3, 'H', 'i'}) // compressed
	flagged = append(flagged, v24Frame("APIC", [2]byte{}, make([]byte, 200))...)

	// A non-zero byte inside the declared tag after the last frame: the size
	// says it is part of the tag, but it is not padding and not a frame.
	trailing := append(v24Body(), 0xFF)
	trailingTag := []byte{'I', 'D', '3', 4, 0, 0}
	trailingTag = append(trailingTag, synchsafeBytes(len(trailing))...)
	trailingTag = append(trailingTag, trailing...)

	cases := map[string][]byte{
		"version 2.2":      v22,
		"unsynchronised":   v24Tag(0x80, body),
		"extended header":  v24Tag(0x40, body),
		"compressed frame": v24Tag(0, flagged),
		"truncated frame":  v24Tag(0, v24Body()[:len(v24Body())-3]),
		"unexplained tail": trailingTag,
	}

	for name, tag := range cases {
		t.Run(name, func(t *testing.T) {
			audio := audioPayload()
			path := filepath.Join(t.TempDir(), "track.mp3")
			before := append(append([]byte{}, tag...), audio...)
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}

			if err := SetCover(path, []byte("cover"), "image/jpeg", nil); !errors.Is(err, ErrUnsupportedTag) {
				t.Errorf("SetCover error = %v, want ErrUnsupportedTag", err)
			}
			if err := SetLyrics(path, Lyrics{Merged: "x"}, Credit{MusicID: 1}, nil); !errors.Is(err, ErrUnsupportedTag) {
				t.Errorf("SetLyrics error = %v, want ErrUnsupportedTag", err)
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("a refused rewrite modified the file")
			}
			if _, err := os.Stat(path + ".part"); err == nil {
				t.Error("a refused rewrite left its .part file behind")
			}
		})
	}
}

// TestInspectReadsV24 checks the lookup path, which reads leniently: a tag it
// cannot parse yields no metadata rather than an error, because there is
// nothing to find in it either way.
func TestInspectReadsV24(t *testing.T) {
	path := v24Fixture(t, audioPayload())
	info, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Format != FormatMP3 {
		t.Errorf("Format = %v, want MP3", info.Format)
	}
	if info.Title != "Hi" {
		t.Errorf("Title = %q, want %q", info.Title, "Hi")
	}
	if !info.HasCover {
		t.Error("HasCover = false, want true")
	}
	if info.HasLyrics() {
		t.Error("HasLyrics = true, want false")
	}

	// An unparseable tag must read as "nothing found", not as an error.
	bad := filepath.Join(t.TempDir(), "bad.mp3")
	data := append(v24Tag(0x80, v24Body()), audioPayload()...)
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = Inspect(bad)
	if err != nil {
		t.Fatalf("Inspect(unsynchronised): %v", err)
	}
	if info.Title != "" || info.HasCover {
		t.Error("Inspect reported metadata from a tag it cannot parse")
	}
}

// TestInspectFLACDuration checks the STREAMINFO decode, whose fields straddle
// byte boundaries: the sample rate takes 20 bits and the sample count 36.
func TestInspectFLACDuration(t *testing.T) {
	// 44100 Hz, 44100 * 3 samples — three seconds exactly.
	//
	// Bytes 10..17 pack a 20-bit sample rate, three bits of channels, five of
	// bit depth and a 36-bit sample count, so every field but the first
	// straddles a byte boundary.
	si := make([]byte, 34)
	const (
		rate    = 44100
		samples = rate * 3
	)
	si[10] = byte(rate >> 12)
	si[11] = byte(rate >> 4 & 0xFF)
	si[12] = byte(rate&0x0F)<<4 | 0<<1 | 0  // mono, 16-bit (high bit)
	si[13] = 15<<4 | byte(samples>>32&0x0F) // 16-bit (low nibble), samples high
	si[14] = byte(samples >> 24 & 0xFF)
	si[15] = byte(samples >> 16 & 0xFF)
	si[16] = byte(samples >> 8 & 0xFF)
	si[17] = byte(samples & 0xFF)

	var buf bytes.Buffer
	buf.WriteString("fLaC")
	if err := writeBlock(&buf, blockStreamInfo, si, true); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "track.flac")
	if err := os.WriteFile(path, append(buf.Bytes(), audioPayload()...), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Duration.Seconds() != 3 {
		t.Errorf("Duration = %v, want 3s", info.Duration)
	}
}
