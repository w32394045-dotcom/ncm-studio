package tag

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// audioPayload stands in for the encoded frames. Its content does not matter —
// every operation here copies it verbatim — but its length and bytes are
// compared before and after a rewrite.
func audioPayload() []byte {
	b := make([]byte, 200_000)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func flacFixture(t *testing.T, tags *Tags, audio []byte) string {
	t.Helper()
	streamInfo := append([]byte{blockStreamInfo, 0, 0, 34}, make([]byte, 34)...)
	prefix := BuildFLACMetadata(&FLACSource{StreamInfo: streamInfo}, tags)

	path := filepath.Join(t.TempDir(), "track.flac")
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mp3Fixture(t *testing.T, tags *Tags, audio []byte) string {
	t.Helper()
	prefix := BuildID3v2(tags)
	path := filepath.Join(t.TempDir(), "track.mp3")
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtures runs the same assertions against both containers, since the two
// rewrites share a contract but not a line of code.
func fixtures() map[string]func(*testing.T, *Tags, []byte) string {
	return map[string]func(*testing.T, *Tags, []byte) string{
		"flac": flacFixture,
		"mp3":  mp3Fixture,
	}
}

// TestCoverEditPreservesAudioAndTags is the core promise of the cover editor:
// swapping the artwork must not touch the audio, and must not cost the user the
// lyrics that were embedded when the file was decrypted.
func TestCoverEditPreservesAudioAndTags(t *testing.T) {
	audio := audioPayload()
	old := bytes.Repeat([]byte("OLD-COVER-BYTES"), 100)
	fresh := bytes.Repeat([]byte("NEW-COVER-BYTES"), 250)

	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			path := build(t, &Tags{
				Title:     "Danza Kuduro",
				Artists:   []string{"Lucenzo"},
				Lyrics:    "[00:01.00]La mano arriba",
				Cover:     old,
				CoverMIME: "image/jpeg",
				MusicID:   1234567,
			}, audio)

			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fpBefore, err := AudioFingerprint(path)
			if err != nil {
				t.Fatal(err)
			}

			if err := SetCover(path, fresh, "image/png", nil); err != nil {
				t.Fatalf("SetCover: %v", err)
			}

			art, err := ReadCover(path)
			if err != nil {
				t.Fatal(err)
			}
			if art == nil {
				t.Fatal("cover disappeared after the rewrite")
			}
			if !bytes.Equal(art.Data, fresh) {
				t.Errorf("cover = %d bytes, want the %d bytes that were set", len(art.Data), len(fresh))
			}
			if art.MIME != "image/png" {
				t.Errorf("mime = %q, want image/png", art.MIME)
			}

			// The audio is the part that must not change. The cover grew, so
			// the file did too; the payload appended after the metadata is what
			// has to be byte-identical.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(after, audio) {
				t.Error("the audio payload was not carried across intact")
			}
			if bytes.Equal(before, after) {
				t.Error("the file is unchanged, so the cover was not written")
			}
			fpAfter, err := AudioFingerprint(path)
			if err != nil {
				t.Fatal(err)
			}
			if fpBefore != fpAfter {
				t.Error("the fingerprint moved with the cover")
			}

			// The lyrics were written at decrypt time and live in the same
			// metadata the cover does; a careless rewrite would drop them.
			if name == "flac" {
				if _, ok := vorbisValueForTest(t, after, "LYRICS"); !ok {
					t.Error("LYRICS was lost when the cover was replaced")
				}
			}
			if got := ReadMusicID(path); got != 1234567 {
				t.Errorf("music id = %d, want 1234567 after the rewrite", got)
			}
		})
	}
}

// TestFingerprintSurvivesCoverEdit is the property the cover library is built
// on: the digest identifies the track, not the file, so editing artwork or
// renaming the file must not orphan its cover history.
func TestFingerprintSurvivesCoverEdit(t *testing.T) {
	audio := audioPayload()

	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			path := build(t, &Tags{
				Title:     "Track",
				Cover:     bytes.Repeat([]byte("A"), 500),
				CoverMIME: "image/jpeg",
			}, audio)

			fp1, err := AudioFingerprint(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetCover(path, bytes.Repeat([]byte("B"), 90_000), "image/jpeg", nil); err != nil {
				t.Fatal(err)
			}
			fp2, err := AudioFingerprint(path)
			if err != nil {
				t.Fatal(err)
			}
			if fp1 != fp2 {
				t.Error("the fingerprint changed when only the cover did")
			}

			// A rename is the other thing that must not matter.
			moved := filepath.Join(filepath.Dir(path), "renamed"+filepath.Ext(path))
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			fp3, err := AudioFingerprint(moved)
			if err != nil {
				t.Fatal(err)
			}
			if fp1 != fp3 {
				t.Error("the fingerprint changed when the file was renamed")
			}

			// Real audio changes still have to be caught, or two different
			// tracks could share a cover history.
			other := audioPayload()
			other[0] ^= 0xFF
			changed := build(t, &Tags{Title: "Track"}, other)
			fp4, err := AudioFingerprint(changed)
			if err != nil {
				t.Fatal(err)
			}
			if fp4 == fp1 {
				t.Error("a different audio payload produced the same fingerprint")
			}
		})
	}
}

// TestRemoveCover covers the third state a track can be in: no artwork at all,
// with the tags around it left alone.
func TestRemoveCover(t *testing.T) {
	for name, build := range fixtures() {
		t.Run(name, func(t *testing.T) {
			audio := audioPayload()
			path := build(t, &Tags{
				Title:     "Track",
				Lyrics:    "[00:01.00]words",
				Cover:     bytes.Repeat([]byte("C"), 4000),
				CoverMIME: "image/jpeg",
			}, audio)

			fp1, err := AudioFingerprint(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetCover(path, nil, "", nil); err != nil {
				t.Fatalf("SetCover(nil): %v", err)
			}

			art, err := ReadCover(path)
			if err != nil {
				t.Fatal(err)
			}
			if art != nil {
				t.Errorf("cover = %d bytes, want none", len(art.Data))
			}
			fp2, err := AudioFingerprint(path)
			if err != nil {
				t.Fatal(err)
			}
			if fp1 != fp2 {
				t.Error("removing the cover changed the fingerprint")
			}

			// Removing a picture must not take the lyrics with it.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(after, audio) {
				t.Error("the audio payload did not survive removing the cover")
			}
			if len(after) >= len(audio)+4000 {
				t.Error("the picture bytes are still in the file")
			}
		})
	}
}

// TestSetCoverIsAtomicOnFailure checks that a rewrite which cannot finish
// leaves the original playable rather than a half-written file.
func TestSetCoverIsAtomicOnFailure(t *testing.T) {
	audio := audioPayload()
	path := flacFixture(t, &Tags{Title: "Track", Cover: bytes.Repeat([]byte("D"), 100)}, audio)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetCover(filepath.Join(t.TempDir(), "nope.flac"), []byte("x"), "image/jpeg", nil); err == nil {
		t.Error("rewriting a missing file reported success")
	}
	if err := SetCover(path+".missing", []byte("x"), "image/jpeg", nil); err == nil {
		t.Error("rewriting a missing file reported success")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the original file changed")
	}
}

// TestReadCoverRejectsNonAudio keeps a stray file from being treated as a
// track with no artwork.
func TestReadCoverRejectsNonAudio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("just some text, not audio at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCover(path); err != ErrUnsupportedFormat {
		t.Errorf("err = %v, want ErrUnsupportedFormat", err)
	}
	if _, err := AudioFingerprint(path); err != ErrUnsupportedFormat {
		t.Errorf("fingerprint err = %v, want ErrUnsupportedFormat", err)
	}
}

// TestSetCoverProgress checks that a caller gets a usable progress signal, not
// just a start and an end.
func TestSetCoverProgress(t *testing.T) {
	audio := audioPayload()
	path := flacFixture(t, &Tags{Title: "Track"}, audio)

	var calls int
	var last, total int64
	err := SetCover(path, bytes.Repeat([]byte("E"), 3000), "image/jpeg", func(done, t2 int64) {
		calls++
		last, total = done, t2
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("progress was never reported")
	}
	if total <= 0 || last > total {
		t.Errorf("progress reported %d/%d", last, total)
	}
}

// vorbisValueForTest digs a Vorbis comment out of a finished FLAC file.
func vorbisValueForTest(t *testing.T, file []byte, key string) (string, bool) {
	t.Helper()
	blocks, _, more, err := parseFLACBlocks(file)
	if err != nil || more {
		t.Fatalf("reparsing the written file: %v (more=%v)", err, more)
	}
	for _, b := range blocks {
		if b.Type == blockVorbisComment {
			return vorbisValue(b.Data, key)
		}
	}
	return "", false
}
