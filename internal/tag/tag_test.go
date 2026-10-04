package tag

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func sampleTags() *Tags {
	return &Tags{
		Title:              "Danza Kuduro",
		Artists:            []string{"Lucenzo", "Don Omar"},
		Album:              "Ritmo Do Brasil",
		Lyrics:             "[00:01.000]Hello\n[00:01.000]你好\n",
		LyricsOriginal:     "[00:01.000]Hello\n",
		LyricsTranslation:  "[00:01.000]你好\n",
		LyricsRomanization: "[00:01.000]Hola\n",
		Cover:              []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3, 4},
		CoverMIME:          "image/jpeg",
	}
}

// decodeUTF16 reads a BOM-prefixed UTF-16 string, as used by ID3v2.3 frames.
func decodeUTF16(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 {
		t.Fatalf("utf16 payload too short: % x", b)
	}
	if b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("missing little-endian BOM: % x", b[:2])
	}
	units := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i+1 < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(units))
}

// splitUTF16 reads a BOM-prefixed, NUL-terminated UTF-16 string and returns it
// along with whatever follows the terminator.
//
// The terminator has to be found by walking whole code units: scanning for the
// byte pair 00 00 would also match the high byte of a Latin character followed
// by the first byte of the terminator, landing one byte early.
func splitUTF16(t *testing.T, b []byte) (string, []byte) {
	t.Helper()
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("expected a little-endian BOM, got % x", b[:min(2, len(b))])
	}
	var units []uint16
	for i := 2; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i : i+2])
		if u == 0 {
			return string(utf16.Decode(units)), b[i+2:]
		}
		units = append(units, u)
	}
	t.Fatal("UTF-16 string is not NUL-terminated")
	return "", nil
}

// parseID3Frames walks an ID3v2.3 tag and returns frame id -> body.
func parseID3Frames(t *testing.T, tag []byte) map[string][][]byte {
	t.Helper()
	if string(tag[:3]) != "ID3" {
		t.Fatalf("bad ID3 signature: % x", tag[:3])
	}
	if tag[3] != 3 {
		t.Errorf("ID3 version = 2.%d, want 2.3", tag[3])
	}
	if tag[6]&0x80 != 0 || tag[7]&0x80 != 0 || tag[8]&0x80 != 0 || tag[9]&0x80 != 0 {
		t.Errorf("tag size is not synchsafe: % x", tag[6:10])
	}
	size := int(tag[6])<<21 | int(tag[7])<<14 | int(tag[8])<<7 | int(tag[9])
	if size != len(tag)-10 {
		t.Errorf("declared size %d, actual %d", size, len(tag)-10)
	}

	frames := map[string][][]byte{}
	pos := 10
	for pos+10 <= len(tag) {
		id := string(tag[pos : pos+4])
		if id == "\x00\x00\x00\x00" {
			break // padding
		}
		frameLen := int(binary.BigEndian.Uint32(tag[pos+4 : pos+8]))
		if pos+10+frameLen > len(tag) {
			t.Fatalf("frame %q overruns the tag", id)
		}
		frames[id] = append(frames[id], tag[pos+10:pos+10+frameLen])
		pos += 10 + frameLen
	}
	return frames
}

func TestBuildID3v2Structure(t *testing.T) {
	tag := BuildID3v2(sampleTags())
	frames := parseID3Frames(t, tag)

	check := func(id, want string) {
		bodies := frames[id]
		if len(bodies) == 0 {
			t.Fatalf("frame %s missing", id)
		}
		if bodies[0][0] != 1 {
			t.Errorf("%s encoding = %d, want 1 (UTF-16)", id, bodies[0][0])
		}
		if got := decodeUTF16(t, bodies[0][1:]); got != want {
			t.Errorf("%s = %q, want %q", id, got, want)
		}
	}
	check("TIT2", "Danza Kuduro")
	check("TALB", "Ritmo Do Brasil")
	// Both credited artists survive, comma separated.
	check("TPE1", "Lucenzo, Don Omar")
	check("TPE2", "Lucenzo")

	if len(frames["USLT"]) != 1 {
		t.Fatal("USLT frame missing")
	}
	uslt := frames["USLT"][0]
	if uslt[0] != 1 {
		t.Errorf("USLT encoding = %d, want 1", uslt[0])
	}
	if string(uslt[1:4]) != "XXX" {
		t.Errorf("USLT language = %q, want XXX", uslt[1:4])
	}
	// An empty descriptor, then the lyrics.
	desc, rest := splitUTF16(t, uslt[4:])
	if desc != "" {
		t.Errorf("USLT descriptor = %q, want empty", desc)
	}
	if got := decodeUTF16(t, rest); got != sampleTags().Lyrics {
		t.Errorf("USLT lyrics = %q", got)
	}

	// The same lyrics ride along as user text as well as in USLT: players
	// disagree about where to look, and one that reads only LYRICS or only
	// UNSYNCEDLYRICS must still find them.
	if len(frames["TXXX"]) != 5 {
		t.Fatalf("want 5 TXXX frames, got %d", len(frames["TXXX"]))
	}
	// Each TXXX must carry a descriptor and a value, not one glued string.
	descriptors := map[string]string{}
	for _, body := range frames["TXXX"] {
		// The descriptor is NUL-terminated; the value runs to the frame end.
		desc, rest := splitUTF16(t, body[1:])
		descriptors[desc] = decodeUTF16(t, rest)
	}
	for _, want := range []string{"LYRICS", "UNSYNCEDLYRICS", "LYRICS_ORIGINAL", "LYRICS_TRANSLATION", "LYRICS_ROMANIZATION"} {
		if _, ok := descriptors[want]; !ok {
			t.Errorf("TXXX %s missing (got %v)", want, descriptors)
		}
	}
	if descriptors["LYRICS"] != sampleTags().Lyrics || descriptors["UNSYNCEDLYRICS"] != sampleTags().Lyrics {
		t.Errorf("the plain-text lyric frames do not carry the lyrics: %q", descriptors)
	}
	if descriptors["LYRICS_TRANSLATION"] != sampleTags().LyricsTranslation {
		t.Errorf("TXXX translation = %q", descriptors["LYRICS_TRANSLATION"])
	}

	apic := frames["APIC"][0]
	if apic[0] != 0 {
		t.Errorf("APIC encoding = %d, want 0 (Latin-1)", apic[0])
	}
	rest = apic[1:]
	nul := bytes.IndexByte(rest, 0)
	if got := string(rest[:nul]); got != "image/jpeg" {
		t.Errorf("APIC mime = %q", got)
	}
	rest = rest[nul+1:]
	if rest[0] != 3 {
		t.Errorf("APIC picture type = %d, want 3 (front cover)", rest[0])
	}
	if rest[1] != 0 {
		t.Error("APIC description is not an empty Latin-1 string")
	}
	if !bytes.Equal(rest[2:], sampleTags().Cover) {
		t.Error("APIC image data does not round-trip")
	}
}

func TestBuildID3v2Empty(t *testing.T) {
	if got := BuildID3v2(&Tags{}); got != nil {
		t.Errorf("empty tags produced a %d-byte tag", len(got))
	}
	if got := BuildID3v2(nil); got != nil {
		t.Error("nil tags produced output")
	}
}

func TestExistingID3v2Size(t *testing.T) {
	tag := BuildID3v2(sampleTags())
	if got := ExistingID3v2Size(tag); got != len(tag) {
		t.Errorf("ExistingID3v2Size = %d, want %d", got, len(tag))
	}
	if got := ExistingID3v2Size([]byte{0xFF, 0xFB, 0x90, 0x00}); got != 0 {
		t.Errorf("ExistingID3v2Size on raw MPEG audio = %d, want 0", got)
	}
}

// parseVorbisComment reads back a comment block.
func parseVorbisComment(t *testing.T, b []byte) map[string][]string {
	t.Helper()
	pos := 0
	vendorLen := int(binary.LittleEndian.Uint32(b[pos : pos+4]))
	pos += 4 + vendorLen
	count := int(binary.LittleEndian.Uint32(b[pos : pos+4]))
	pos += 4

	out := map[string][]string{}
	for range count {
		n := int(binary.LittleEndian.Uint32(b[pos : pos+4]))
		pos += 4
		entry := string(b[pos : pos+n])
		pos += n
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("comment %q has no separator", entry)
		}
		out[key] = append(out[key], value)
	}
	return out
}

func TestBuildFLACMetadata(t *testing.T) {
	streamInfo := []byte{0x00, 0x00, 0x00, 34}
	streamInfo = append(streamInfo, make([]byte, 34)...)
	src := &FLACSource{StreamInfo: streamInfo, FrameOffset: 4 + 38}

	out := BuildFLACMetadata(src, sampleTags())
	if string(out[:4]) != "fLaC" {
		t.Fatal("output does not start with fLaC")
	}

	// Walk the block chain we just produced.
	pos := 4
	var types []byte
	var comment, picture []byte
	for {
		header := out[pos : pos+4]
		isLast := header[0]&0x80 != 0
		blockType := header[0] & 0x7F
		length := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		payload := out[pos+4 : pos+4+length]
		types = append(types, blockType)
		switch blockType {
		case blockVorbisComment:
			comment = payload
		case blockPicture:
			picture = payload
		}
		pos += 4 + length
		if isLast {
			break
		}
	}
	if len(types) != 3 || types[0] != blockStreamInfo || types[1] != blockVorbisComment || types[2] != blockPicture {
		t.Fatalf("block types = %v, want [0 4 6]", types)
	}
	if pos != len(out) {
		t.Errorf("trailing bytes after the last block: %d", len(out)-pos)
	}

	fields := parseVorbisComment(t, comment)
	if got := fields["TITLE"]; len(got) != 1 || got[0] != "Danza Kuduro" {
		t.Errorf("TITLE = %q", got)
	}
	// Each artist is its own ARTIST entry.
	if got := fields["ARTIST"]; len(got) != 2 || got[0] != "Lucenzo" || got[1] != "Don Omar" {
		t.Errorf("ARTIST = %q", got)
	}
	if got := fields["ALBUM"]; len(got) != 1 || got[0] != "Ritmo Do Brasil" {
		t.Errorf("ALBUM = %q", got)
	}
	if got := fields["LYRICS"]; len(got) != 1 || got[0] != sampleTags().Lyrics {
		t.Errorf("LYRICS = %q", got)
	}
	// Both spellings carry the same timeline.
	if got := fields["UNSYNCEDLYRICS"]; len(got) != 1 || got[0] != sampleTags().Lyrics {
		t.Errorf("UNSYNCEDLYRICS = %q", got)
	}
	if got := fields["LYRICS_TRANSLATION"]; len(got) != 1 || got[0] != sampleTags().LyricsTranslation {
		t.Errorf("LYRICS_TRANSLATION = %q", got)
	}

	// The picture block must carry type, mime and the exact image bytes.
	p := picture
	if binary.BigEndian.Uint32(p[0:4]) != 3 {
		t.Error("picture type is not 3")
	}
	mimeLen := int(binary.BigEndian.Uint32(p[4:8]))
	if got := string(p[8 : 8+mimeLen]); got != "image/jpeg" {
		t.Errorf("picture mime = %q", got)
	}
	if !bytes.HasSuffix(p, sampleTags().Cover) {
		t.Error("picture data does not round-trip")
	}
}

func TestParseFLACMetadataNeedsMore(t *testing.T) {
	streamInfo := append([]byte{0x00, 0x00, 0x00, 34}, make([]byte, 34)...)
	// A chain whose first block is not last, truncated before the real end.
	partial := append([]byte("fLaC"), streamInfo...)
	src, more, err := ParseFLACMetadata(partial)
	if err != nil {
		t.Fatalf("ParseFLACMetadata: %v", err)
	}
	if !more {
		t.Error("expected more=true for a truncated chain")
	}
	if src != nil && src.FrameOffset != 0 {
		t.Error("a truncated chain must not report a frame offset")
	}

	full := append(partial, 0x84, 0, 0, 4, 1, 2, 3, 4) // last block, type 4
	src, more, err = ParseFLACMetadata(full)
	if err != nil || more {
		t.Fatalf("full chain: err=%v more=%v", err, more)
	}
	if src.FrameOffset != len(full) {
		t.Errorf("FrameOffset = %d, want %d", src.FrameOffset, len(full))
	}
}

func TestParseFLACMetadataRejectsNonFLAC(t *testing.T) {
	if _, _, err := ParseFLACMetadata([]byte("ID3\x03\x00\x00")); err != ErrNotFLAC {
		t.Errorf("err = %v, want ErrNotFLAC", err)
	}
}

// TestPlatformIDsRoundTrip covers the NetEase identifiers the info page reads
// back. They are written in both containers under the same names and must come
// back exactly, including the empty slot that keeps an id lined up with the
// artist it belongs to.
func TestPlatformIDsRoundTrip(t *testing.T) {
	tags := &Tags{
		Title:     "ひめごと",
		Artists:   []string{"高野麻里佳", "石原夏織", "金元寿子"},
		MusicID:   2008994719,
		AlbumID:   987654,
		ArtistIDs: []int64{111, 0, 333},
		Bitrate:   999000,
	}
	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			path := build(t, tags, audioPayload())
			info, err := Inspect(path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if info.MusicID != 2008994719 {
				t.Errorf("MusicID = %d", info.MusicID)
			}
			if info.AlbumID != 987654 {
				t.Errorf("AlbumID = %d", info.AlbumID)
			}
			if len(info.ArtistIDs) != 3 || info.ArtistIDs[0] != 111 ||
				info.ArtistIDs[1] != 0 || info.ArtistIDs[2] != 333 {
				t.Errorf("ArtistIDs = %v, want [111 0 333]", info.ArtistIDs)
			}
			if info.Bitrate != 999000 {
				t.Errorf("Bitrate = %d", info.Bitrate)
			}
		})
	}
}

// TestLyricsRewriteKeepsPlatformIDs is the safety net for every writer that
// does not know the platform data: a translation, an imported .lrc, or a
// re-fetch must leave the identifiers exactly as they were. The song id is the
// one at risk, because the rewrite has to replace its frame to keep it unique.
func TestLyricsRewriteKeepsPlatformIDs(t *testing.T) {
	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			path := build(t, &Tags{
				Title:     "Ride",
				MusicID:   4242,
				AlbumID:   77,
				ArtistIDs: []int64{5, 6},
				Bitrate:   320000,
			}, audioPayload())

			// A write that carries no id at all, as an imported .lrc would not.
			if err := SetLyrics(path, Lyrics{Merged: "[00:01.000]hello\n"}, Credit{}, nil); err != nil {
				t.Fatalf("SetLyrics: %v", err)
			}

			info, err := Inspect(path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if info.MusicID != 4242 {
				t.Errorf("MusicID was lost: %d", info.MusicID)
			}
			if info.AlbumID != 77 || info.Bitrate != 320000 {
				t.Errorf("platform data was lost: album=%d bitrate=%d", info.AlbumID, info.Bitrate)
			}
			if len(info.ArtistIDs) != 2 || info.ArtistIDs[0] != 5 || info.ArtistIDs[1] != 6 {
				t.Errorf("ArtistIDs were lost: %v", info.ArtistIDs)
			}
			// The trailing newline is normalized away on the way back out;
			// the lines themselves are what this test cares about.
			if !strings.Contains(info.Lyrics.Merged, "[00:01.000]hello") {
				t.Errorf("lyrics = %q", info.Lyrics.Merged)
			}
		})
	}
}

// TestReadFLACBlocksRetriesFromTheStart covers a chain too long for the first
// window. The walk retries with a bigger one, and it has to rewind first: a
// parser handed the bytes where the last read stopped sees audio frames, calls
// them a missing FLAC signature, and the whole file becomes unreadable to every
// caller — which is what a cover of a megabyte or two produced.
func TestReadFLACBlocksRetriesFromTheStart(t *testing.T) {
	audio := []byte("not really frames, but the parser never looks")
	// A megabyte and a half of artwork pushes the chain past the 1 MiB first
	// window, so at least one retry happens before the last block is read.
	big := bytes.Repeat([]byte{0xAB}, 1536<<10)
	prefix := BuildFLACMetadata(&FLACSource{StreamInfo: testStreamInfo()}, &Tags{
		Title:     "Ride",
		Cover:     big,
		CoverMIME: "image/jpeg",
		MusicID:   42,
	})
	path := filepath.Join(t.TempDir(), "big-cover.flac")
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(prefix) <= 1<<20 {
		t.Fatalf("fixture prefix is %d bytes; the test needs one over 1 MiB", len(prefix))
	}

	info, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Title != "Ride" {
		t.Errorf("Title = %q, want %q", info.Title, "Ride")
	}
	if info.MusicID != 42 {
		t.Errorf("MusicID = %d, want 42", info.MusicID)
	}
	if !info.HasCover {
		t.Error("the cover block was not seen")
	}

	if _, err := AudioFingerprint(path); err != nil {
		t.Errorf("AudioFingerprint: %v", err)
	}
	if art, err := ReadCover(path); err != nil || art == nil {
		t.Errorf("ReadCover: art=%v err=%v", art, err)
	}
}

// TestStreamedFormsMatchBufferedOnes guards the refactor that lets the pipeline
// stream a tag straight into the output file: the writers that take an
// io.Writer must produce exactly the bytes the in-memory builders always did.
// The cover is the reason for the split, so the tag used here carries one.
func TestStreamedFormsMatchBufferedOnes(t *testing.T) {
	tags := sampleTags()
	streamInfo := testStreamInfo()

	for _, tc := range []struct {
		name      string
		buffered  []byte
		streamErr error
		stream    func(*bytes.Buffer) error
	}{
		{
			name:     "flac",
			buffered: BuildFLACMetadata(&FLACSource{StreamInfo: streamInfo}, tags),
			stream: func(b *bytes.Buffer) error {
				return WriteFLACMetadata(b, &FLACSource{StreamInfo: streamInfo}, tags)
			},
		},
		{
			name:     "id3v2",
			buffered: BuildID3v2(tags),
			stream:   func(b *bytes.Buffer) error { return WriteID3v2(b, tags) },
		},
	} {
		var buf bytes.Buffer
		if err := tc.stream(&buf); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !bytes.Equal(buf.Bytes(), tc.buffered) {
			t.Errorf("%s: the streamed tag differs from the buffered one (%d bytes vs %d)",
				tc.name, buf.Len(), len(tc.buffered))
		}
	}
}

// TestStreamedFormsStayEmptyWhenThereIsNothingToWrite: an empty tag must stay
// empty, since whether the bytes are there decides whether the file gets a
// tag block at all.
func TestStreamedFormsStayEmptyWhenThereIsNothingToWrite(t *testing.T) {
	streamInfo := testStreamInfo()

	var flac bytes.Buffer
	if err := WriteFLACMetadata(&flac, &FLACSource{StreamInfo: streamInfo}, &Tags{}); err != nil {
		t.Fatal(err)
	}
	if flac.Len() == 0 {
		t.Error("a FLAC metadata section was not written even though it always has to be")
	}

	var id3 bytes.Buffer
	if err := WriteID3v2(&id3, &Tags{}); err != nil {
		t.Fatal(err)
	}
	if id3.Len() != 0 {
		t.Errorf("an empty ID3 tag wrote %d bytes", id3.Len())
	}
	if err := WriteID3v2(&id3, nil); err != nil {
		t.Fatal(err)
	}
	if id3.Len() != 0 {
		t.Errorf("a nil ID3 tag wrote %d bytes", id3.Len())
	}
}

// testStreamInfo is a minimal but structurally valid STREAMINFO block: a
// one-byte header and the 34 bytes the format requires.
func testStreamInfo() []byte {
	return append([]byte{0x00, 0x00, 0x00, 34}, make([]byte, 34)...)
}
