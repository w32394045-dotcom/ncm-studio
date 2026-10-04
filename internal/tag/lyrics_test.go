package tag

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The timelines below are shaped like the ones the lyric API returns, including
// a translation and a romanisation, because the write path has to keep all
// three separate as well as write the merged form.
func lyricFixture() Lyrics {
	return Lyrics{
		Merged:       "[00:01.000]Hello\n[00:01.000]你好\n[00:02.000]World\n[00:02.000]世界\n",
		Original:     "[00:01.000]Hello\n[00:02.000]World\n",
		Translation:  "[00:01.000]你好\n[00:02.000]世界\n",
		Romanization: "[00:01.000]Halo\n[00:02.000]Wārudo\n",
	}
}

// TestSetLyricsPreservesEverythingElse is the promise the whole in-place design
// exists for: adding lyrics to a file must not cost its owner a single other
// byte. The audio is compared verbatim, and so is every tag this package does
// not itself model.
func TestSetLyricsPreservesEverythingElse(t *testing.T) {
	audio := audioPayload()

	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			tags := sampleTags()
			tags.MusicID = 0 // this file predates the id being written
			path := build(t, tags, audio)

			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := lyricFixture()
			if err := SetLyrics(path, want, Credit{MusicID: 2008994719}, nil); err != nil {
				t.Fatalf("SetLyrics: %v", err)
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, after) {
				t.Fatal("the file was not changed at all")
			}

			// The audio payload has to appear in the new file unchanged, at
			// whatever offset the metadata now ends.
			if !bytes.Contains(after, audio) {
				t.Error("the audio payload is no longer intact in the rewritten file")
			}

			info, err := Inspect(path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if info.Title != "Danza Kuduro" {
				t.Errorf("title = %q", info.Title)
			}
			// FLAC repeats the ARTIST key once per value; ID3 has a single TPE1,
			// which this package writes comma-joined so that a name containing a
			// slash is not torn in half. Either way no artist is lost, which is
			// the part a lyrics rewrite has to preserve.
			if got := strings.Join(info.Artists, ", "); got != "Lucenzo, Don Omar" {
				t.Errorf("artists = %v", info.Artists)
			}
			if info.Album != "Ritmo Do Brasil" {
				t.Errorf("album = %q", info.Album)
			}
			if !info.HasCover {
				t.Error("the cover was dropped")
			}
			if info.MusicID != 2008994719 {
				t.Errorf("music id = %d, want the one that was written", info.MusicID)
			}
		})
	}
}

// TestSetLyricsRoundTripsExactly is what makes a second pass over a finished
// library cheap: after a write, what the file reports has to compare equal to
// what was written, or the next run would rewrite every file to change nothing.
//
// It is also the regression test for the USLT descriptor. Decoding a lyrics
// frame as though it were a plain text frame leaves three language bytes and a
// terminator stuck to the front, which never compares equal — so every run
// rewrote every MP3 and never converged.
func TestSetLyricsRoundTripsExactly(t *testing.T) {
	audio := audioPayload()[:4096]

	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			path := build(t, sampleTags(), audio)
			want := lyricFixture()

			if err := SetLyrics(path, want, Credit{MusicID: 12345}, nil); err != nil {
				t.Fatalf("SetLyrics: %v", err)
			}
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			info, err := Inspect(path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !info.HasLyrics() {
				t.Fatal("the file does not report the lyrics that were just written")
			}
			if !info.Lyrics.Equal(want) {
				t.Errorf("read back %+v, want %+v", info.Lyrics, want)
			}
			if info.MusicID != 12345 {
				t.Errorf("music id = %d", info.MusicID)
			}

			// Writing the same thing again must produce the same bytes: that is
			// what lets a re-run skip the copy instead of repeating it.
			if err := SetLyrics(path, want, Credit{MusicID: 12345}, nil); err != nil {
				t.Fatalf("second SetLyrics: %v", err)
			}
			second, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Error("writing the same lyrics twice produced a different file")
			}
		})
	}
}

// TestSetLyricsKeepsUnknownVorbisKeys guards the reason this package rewrites
// blocks by hand rather than rebuilding the tag from a Tags value: anything it
// does not model has to survive, because it belongs to the user.
func TestSetLyricsKeepsUnknownVorbisKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.flac")

	var buf bytes.Buffer
	buf.WriteString("fLaC")
	si := make([]byte, 34)
	if err := writeBlock(&buf, blockStreamInfo, si, false); err != nil {
		t.Fatal(err)
	}
	c := &vorbisComments{vendor: "reference libFLAC 1.3.2"}
	c.set("TITLE", "Danza Kuduro")
	c.set("REPLAYGAIN_TRACK_GAIN", "-6.20 dB")
	c.set("REPLAYGAIN_TRACK_PEAK", "0.988861")
	c.set("COMPOSER", "Lucenzo")
	c.set("SOMETHING_ELSE", "keep me")
	if err := writeBlock(&buf, blockVorbisComment, c.marshal(), true); err != nil {
		t.Fatal(err)
	}
	buf.Write(audioPayload()[:2048])
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetLyrics(path, lyricFixture(), Credit{MusicID: 42}, nil); err != nil {
		t.Fatalf("SetLyrics: %v", err)
	}

	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"TITLE":                 "Danza Kuduro",
		"REPLAYGAIN_TRACK_GAIN": "-6.20 dB",
		"REPLAYGAIN_TRACK_PEAK": "0.988861",
		"COMPOSER":              "Lucenzo",
		"SOMETHING_ELSE":        "keep me",
		"NETEASE_MUSIC_ID":      "42",
		// A write trims the ends of each timeline. Transports disagree about a
		// trailing newline, and keeping one would make an unchanged file look
		// different on every pass.
		"LYRICS":              strings.TrimSpace(lyricFixture().Merged),
		"UNSYNCEDLYRICS":      strings.TrimSpace(lyricFixture().Merged),
		"LYRICS_ORIGINAL":     strings.TrimSpace(lyricFixture().Original),
		"LYRICS_TRANSLATION":  strings.TrimSpace(lyricFixture().Translation),
		"LYRICS_ROMANIZATION": strings.TrimSpace(lyricFixture().Romanization),
	} {
		got, ok := vorbisValueForTest(t, file, key)
		if !ok {
			t.Errorf("%s was not written", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// And the count has to be right, or the next reader stops early.
	blocks, _, _, err := parseFLACBlocks(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if b.Type != blockVorbisComment {
			continue
		}
		_, malformed, err := parseVorbisComments(b.Data)
		if err != nil {
			t.Fatal(err)
		}
		if malformed {
			t.Error("the comment block that was written does not parse cleanly")
		}
	}
}

// rawComments renders a Vorbis comment block with a declared entry count that
// the caller controls, which is how the older decryptor's block is reproduced:
// it wrote len(bytes)/8 rather than the number of entries, so a file with three
// comments claims to have nine.
func rawComments(vendor string, declared uint32, entries ...string) []byte {
	var b bytes.Buffer
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(vendor)))
	b.Write(n[:])
	b.WriteString(vendor)
	binary.LittleEndian.PutUint32(n[:], declared)
	b.Write(n[:])
	for _, e := range entries {
		binary.LittleEndian.PutUint32(n[:], uint32(len(e)))
		b.Write(n[:])
		b.WriteString(e)
	}
	return b.Bytes()
}

// TestBrokenCommentCountIsToleratedAndFixed covers the files this tool most
// often has to repair: a comment block whose vendor count field is wrong. The
// parser must not trust it — the block's own length is the only honest bound —
// and a rewrite has to leave a correct count behind.
func TestBrokenCommentCountIsToleratedAndFixed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-tool.flac")

	block := rawComments("ncm-decrypt", 9,
		"TITLE=ひめごと*クライシスターズ(もみじver.)",
		"ARTIST=高野麻里佳; 石原夏織; 金元寿子; 津田美波",
		"ALBUM=ひめごと*クライシスターズ",
	)

	var buf bytes.Buffer
	buf.WriteString("fLaC")
	if err := writeBlock(&buf, blockStreamInfo, make([]byte, 34), false); err != nil {
		t.Fatal(err)
	}
	if err := writeBlock(&buf, blockVorbisComment, block, true); err != nil {
		t.Fatal(err)
	}
	buf.Write(audioPayload()[:1024])
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// The declared count is a lie; all three entries must still be found.
	info, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Title != "ひめごと*クライシスターズ(もみじver.)" {
		t.Errorf("title = %q — the wrong count hid the entries", info.Title)
	}
	if len(info.Artists) != 4 {
		t.Errorf("artists = %v, want four", info.Artists)
	}

	if err := SetLyrics(path, lyricFixture(), Credit{MusicID: 7}, nil); err != nil {
		t.Fatalf("SetLyrics: %v", err)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vorbisValueForTest(t, file, "TITLE"); !ok {
		t.Error("the title was lost repairing the block")
	}

	blocks, _, _, err := parseFLACBlocks(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if b.Type != blockVorbisComment {
			continue
		}
		declared := int(binary.LittleEndian.Uint32(b.Data[4+int(binary.LittleEndian.Uint32(b.Data[:4])):]))
		if _, malformed, _ := parseVorbisComments(b.Data); malformed {
			t.Errorf("the repaired block still declares %d entries for the ones it holds", declared)
		}
	}
}

// TestParseUSLTIsNotAPlainTextFrame pins the frame layout: encoding, a
// three-byte language, a terminated descriptor, then the lyrics.
func TestParseUSLTIsNotAPlainTextFrame(t *testing.T) {
	const lyrics = "[00:01.000]Hello\n[00:02.000]World\n"
	if got := parseUSLT(usltFrame(lyrics)); got != lyrics {
		t.Errorf("parseUSLT(usltFrame(x)) = %q, want %q", got, lyrics)
	}

	// A Latin-1 frame with a real descriptor, as another tagger would write it.
	latin1 := append([]byte{0}, []byte("eng")...)
	latin1 = append(latin1, []byte("Lyrics by X")...)
	latin1 = append(latin1, 0)
	latin1 = append(latin1, []byte("line one\nline two")...)
	if got := parseUSLT(latin1); got != "line one\nline two" {
		t.Errorf("parseUSLT with a descriptor = %q", got)
	}

	if got := parseUSLT(nil); got != "" {
		t.Errorf("parseUSLT(nil) = %q", got)
	}
	if got := parseUSLT([]byte{1, 'e', 'n'}); got != "" {
		t.Errorf("parseUSLT(truncated) = %q", got)
	}
}

// TestSetLyricsRefusesWhatItCannotWrite: an unreadable file must be left exactly
// as it was, with no half-written temporary beside it.
func TestSetLyricsRefusesWhatItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	original := []byte("this is not audio at all, but it is somebody's file")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetLyrics(path, lyricFixture(), Credit{MusicID: 1}, nil); err == nil {
		t.Fatal("SetLyrics accepted a file that is not audio")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Error("the refused file was modified anyway")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("directory holds %d entries; a temporary file was left behind", len(entries))
	}
}

// TestSetLyricsRefusesAnEmptyTimeline: a caller with nothing to write has made a
// mistake upstream, and silently rewriting the file without lyrics would look
// like it worked.
func TestSetLyricsRefusesAnEmptyTimeline(t *testing.T) {
	path := flacFixture(t, sampleTags(), audioPayload()[:512])
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetLyrics(path, Lyrics{}, Credit{MusicID: 1}, nil); err == nil {
		t.Fatal("SetLyrics accepted an empty timeline")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the file changed despite there being nothing to write")
	}
}

// namelessFLAC builds the file this fix exists for: one that already carries
// the lyrics and the track id, and nothing that says what the track is called.
// It is what some other decoder leaves behind, and what a player needs a name
// for before it will look the lyrics up at all.
func namelessFLAC(t *testing.T, withName bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nameless.flac")

	var buf bytes.Buffer
	buf.WriteString("fLaC")
	if err := writeBlock(&buf, blockStreamInfo, make([]byte, 34), false); err != nil {
		t.Fatal(err)
	}
	c := &vorbisComments{vendor: "Lavf58.45.100"}
	c.set("encoder", "Lavf58.45.100")
	c.set("LYRICS", "[00:01.000]On a dark desert highway\n")
	if withName {
		c.set("TITLE", "A name of its own")
		c.set("ARTIST", "Some Other Artist")
		c.set("ALBUM", "Some Other Album")
	}
	c.set("NETEASE_MUSIC_ID", "4049399")
	if err := writeBlock(&buf, blockVorbisComment, c.marshal(), true); err != nil {
		t.Fatal(err)
	}
	buf.Write(audioPayload()[:1024])
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// namelessMP3 is the same file in the other container: an ID3 tag holding the
// lyrics and the id, with no TIT2, TPE1 or TALB frames at all.
func namelessMP3(t *testing.T, withName bool) string {
	t.Helper()
	tags := &Tags{Lyrics: "[00:01.000]On a dark desert highway\n", MusicID: 4049399}
	if withName {
		tags.Title = "A name of its own"
		tags.Artists = []string{"Some Other Artist"}
		tags.Album = "Some Other Album"
	}
	return mp3Fixture(t, tags, audioPayload()[:1024])
}

// TestSetLyricsFillsInAMissingName is the fix for the file a third-party player
// would not show lyrics for: the timelines were all there and correct, and the
// only thing the file lacked was a title and an artist to attach them to.
//
// The fill is one-way. A field the file already carries is its owner's and is
// never replaced, however confident the catalogue is — so the second half of
// this test is the more important half.
func TestSetLyricsFillsInAMissingName(t *testing.T) {
	credit := Credit{
		MusicID: 4049399,
		Title:   "Hotel California (Live on MTV, 1994)",
		Artists: []string{"Eagles"},
		Album:   "Hell Freezes Over",
	}

	for name, build := range map[string]func(*testing.T, bool) string{
		"flac": namelessFLAC,
		"mp3":  namelessMP3,
	} {
		t.Run(name, func(t *testing.T) {
			path := build(t, false)
			before, err := Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			if before.Title != "" || len(before.Artists) != 0 || before.Album != "" {
				t.Fatalf("the fixture is not nameless: %+v", before)
			}
			if !before.HasLyrics() || before.MusicID != 4049399 {
				t.Fatalf("the fixture is missing what it should already have: %+v", before)
			}
			if !credit.Fills(before) {
				t.Fatal("a nameless file was not recognised as one the credit could fill")
			}

			if err := SetLyrics(path, before.Lyrics, credit, nil); err != nil {
				t.Fatalf("SetLyrics: %v", err)
			}

			after, err := Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Title != credit.Title {
				t.Errorf("title = %q, want %q", after.Title, credit.Title)
			}
			if strings.Join(after.Artists, ", ") != "Eagles" {
				t.Errorf("artists = %v, want the credit's", after.Artists)
			}
			if after.Album != credit.Album {
				t.Errorf("album = %q, want %q", after.Album, credit.Album)
			}
			// Filling a name is not licence to touch anything else.
			if !after.Lyrics.Equal(before.Lyrics) {
				t.Errorf("the lyrics changed: %+v -> %+v", before.Lyrics, after.Lyrics)
			}
			if after.MusicID != 4049399 {
				t.Errorf("music id = %d", after.MusicID)
			}
			// Nothing left to fill means the next pass can skip the rewrite.
			if credit.Fills(after) {
				t.Error("the credit still claims to fill something after the write")
			}

			// Now the one-way half: the same credit against a file that already
			// names itself must leave every one of those fields as it was.
			named := build(t, true)
			own, err := Inspect(named)
			if err != nil {
				t.Fatal(err)
			}
			if !own.HasLyrics() {
				t.Fatal("the named fixture lost its lyrics")
			}
			if err := SetLyrics(named, own.Lyrics, credit, nil); err != nil {
				t.Fatalf("SetLyrics: %v", err)
			}
			kept, err := Inspect(named)
			if err != nil {
				t.Fatal(err)
			}
			if kept.Title != "A name of its own" {
				t.Errorf("title = %q — the file's own was overwritten", kept.Title)
			}
			if strings.Join(kept.Artists, ", ") != "Some Other Artist" {
				t.Errorf("artists = %v — the file's own were overwritten", kept.Artists)
			}
			if kept.Album != "Some Other Album" {
				t.Errorf("album = %q — the file's own was overwritten", kept.Album)
			}
			if credit.Fills(kept) {
				t.Error("a file that names itself was still seen as missing a name")
			}
		})
	}
}

// TestCreditFillsOnlyWhatIsMissing pins the predicate itself, since it decides
// both whether a write happens at all and whether the next pass can skip it.
func TestCreditFillsOnlyWhatIsMissing(t *testing.T) {
	full := Credit{MusicID: 1, Title: "T", Artists: []string{"A"}, Album: "L"}

	if (&Credit{MusicID: 1}).Fills(&AudioInfo{}) {
		t.Error("an id alone was treated as a name to write")
	}
	// An album is not identity: the one that makes a player show lyrics is the
	// title or the artist.
	if full.Fills(&AudioInfo{Title: "T", Artists: []string{"A"}, Album: "L"}) {
		t.Error("a fully named file was seen as missing a name")
	}
	if !full.Fills(&AudioInfo{Artists: []string{"A"}, Album: "L"}) {
		t.Error("a file with no title was seen as named")
	}
	if !full.Fills(&AudioInfo{Title: "T", Album: "L"}) {
		t.Error("a file with no artist was seen as named")
	}
	// A blank entry is not a name, and a player will not read it as one.
	if !full.Fills(&AudioInfo{Title: "   ", Artists: []string{"A"}, Album: "L"}) {
		t.Error("a blank title was treated as the file's own")
	}
	// Something the credit cannot supply is not a gap it can fill, or every
	// file without an album would be rewritten on every pass.
	if (&Credit{Title: "T"}).Fills(&AudioInfo{Title: "T"}) {
		t.Error("a credit missing an album claimed a file lacked one")
	}
}

// TestLyricsEqualFoldsTheMergedFallback keeps the comparison honest: a caller
// that supplies only an original timeline is asking for the same file as one
// that spells out both, so it must not look like a change.
func TestLyricsEqualFoldsTheMergedFallback(t *testing.T) {
	a := Lyrics{Original: "[00:01.000]x\n"}
	b := Lyrics{Merged: "[00:01.000]x\n", Original: "[00:01.000]x\n"}
	if !a.Equal(b) || !b.Equal(a) {
		t.Error("a merged fallback compared as a change")
	}
	if a.Equal(Lyrics{Original: "[00:02.000]y\n"}) {
		t.Error("different timelines compared equal")
	}
	// Whitespace is the transport's business, not the file's.
	if !a.Equal(Lyrics{Original: "  [00:01.000]x\n  "}) {
		t.Error("surrounding whitespace compared as a change")
	}
}

// TestInspectReadsEveryArtistEntry is the regression test for a four-artist
// credit being read as only its first name.
//
// Vorbis comments repeat a key once per value rather than joining, so reading a
// single ARTIST entry drops everyone after the lead. That is not cosmetic: the
// artist credit is what a backfill matches a track on, and a file that appears
// to have one artist where it really has four scores below the bar that lets
// its lyrics be written without asking.
func TestInspectReadsEveryArtistEntry(t *testing.T) {
	const credit = "高野麻里佳; 石原夏織; 金元寿子; 津田美波"
	artists := strings.Split(credit, "; ")

	path := filepath.Join(t.TempDir(), "track.flac")
	var buf bytes.Buffer
	buf.WriteString("fLaC")
	if err := writeBlock(&buf, blockStreamInfo, make([]byte, 34), false); err != nil {
		t.Fatal(err)
	}
	c := &vorbisComments{vendor: "ncm-decrypt"}
	c.set("TITLE", "ひめごと*クライシスターズ(もみじver.)")
	// Appended rather than set: set replaces the first entry with that key,
	// which is the opposite of what a repeated Vorbis key means.
	for _, a := range artists {
		c.entries = append(c.entries, vorbisEntry{key: "ARTIST", value: a})
	}
	if err := writeBlock(&buf, blockVorbisComment, c.marshal(), true); err != nil {
		t.Fatal(err)
	}
	buf.Write(audioPayload()[:256])
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(info.Artists) != len(artists) {
		t.Fatalf("artists = %v, want all %d", info.Artists, len(artists))
	}
	for i, want := range artists {
		if info.Artists[i] != want {
			t.Errorf("artist %d = %q, want %q", i, info.Artists[i], want)
		}
	}

	// A single entry holding a semicolon-joined credit — what an older tool
	// wrote — has to come apart the same way.
	var single bytes.Buffer
	single.WriteString("fLaC")
	if err := writeBlock(&single, blockStreamInfo, make([]byte, 34), false); err != nil {
		t.Fatal(err)
	}
	one := &vorbisComments{vendor: "ncm-decrypt"}
	one.set("ARTIST", credit)
	if err := writeBlock(&single, blockVorbisComment, one.marshal(), true); err != nil {
		t.Fatal(err)
	}
	single.Write(audioPayload()[:256])
	joined := filepath.Join(t.TempDir(), "joined.flac")
	if err := os.WriteFile(joined, single.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = Inspect(joined)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Artists) != len(artists) {
		t.Errorf("a joined credit read back as %v", info.Artists)
	}
}
