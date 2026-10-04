package backfill

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ncm-studio/internal/lyric"
	"ncm-studio/internal/tag"
)

// flacAudioOffset walks a FLAC file's metadata chain and returns where the
// audio frames begin, so a test can prove a rewrite left every one of them
// alone without depending on the package's own parser.
func flacAudioOffset(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	head := make([]byte, 4)
	if _, err := f.Read(head); err != nil || string(head) != "fLaC" {
		t.Fatalf("%s is not a FLAC file", path)
	}
	var pos int64 = 4
	var hdr [4]byte
	for {
		if _, err := f.ReadAt(hdr[:], pos); err != nil {
			t.Fatal(err)
		}
		size := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		pos += 4 + size
		if hdr[0]&0x80 != 0 {
			return pos
		}
	}
}

// TestRealLibraryMatchesTheRightVersion is the check the synthetic fixtures
// cannot make: eight recordings of one song, all named almost identically, run
// against the live catalogue. Each has to be matched to its own track, and the
// backing track has to be stopped for a person.
func TestRealLibraryMatchesTheRightVersion(t *testing.T) {
	if os.Getenv("NCM_REAL_CHECK") == "" {
		t.Skip("set NCM_REAL_CHECK=1 to run against the live catalogue")
	}
	client := lyric.NewClient(filepath.Join(t.TempDir(), "lyrics"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const dir = "/sdcard/Download"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no test library: %v", err)
	}

	// The group cache is what production uses, so the live check has to go
	// through it: eight recordings of one song must cost one search, and this
	// is the only place that can be observed against the real endpoint.
	searches := NewSearcher()

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.Contains(name, "ひめごと") || !strings.HasSuffix(name, ".flac") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := tag.Inspect(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		q := queryFor(info, name)
		inst := lyric.LooksInstrumental(name, info.Title)

		cands, err := searches.pool(ctx, client, q)
		if err != nil {
			t.Fatalf("%s: search: %v", name, err)
		}
		d := lyric.Decide(q, cands, info.Duration, inst)

		verdict := "REVIEW"
		if d.Auto {
			verdict = "auto"
		}
		t.Logf("%-72s len=%-12v inst=%-5v %s id=%d %q %s",
			name, info.Duration.Truncate(time.Millisecond), inst, verdict, d.Best.MusicID, d.Best.Name, d.Reason)
		checked++

		if inst && d.Auto {
			t.Errorf("%s: a backing track was going to be written without asking", name)
		}
	}
	if checked == 0 {
		t.Skip("the test library holds no versions of this song")
	}
	t.Logf("checked %d recordings", checked)
}

// TestRealFileSurvivesALyricsWrite is the end-to-end proof on a copy of the
// 160 MB file this feature was built for: the lyrics land, and every audio
// frame is byte-for-byte what it was.
func TestRealFileSurvivesALyricsWrite(t *testing.T) {
	if os.Getenv("NCM_REAL_CHECK") == "" {
		t.Skip("set NCM_REAL_CHECK=1 to run against the live catalogue")
	}
	src := "/sdcard/Download/高野麻里佳; 石原夏織; 金元寿子; 津田美波 - ひめごと_クライシスターズ(もみじver.).flac"
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no test file: %v", err)
	}

	dir := t.TempDir()
	work := filepath.Join(dir, filepath.Base(src))
	if err := copyFile(src, work); err != nil {
		t.Fatal(err)
	}

	offset := flacAudioOffset(t, work)
	before, err := hashAudio(work, offset)
	if err != nil {
		t.Fatal(err)
	}

	client := lyric.NewClient(filepath.Join(dir, "lyrics"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := tag.Inspect(work)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before: format=%v len=%d duration=%v artists=%v id=%d hasLyrics=%v",
		info.Format, info.Length, info.Duration, info.Artists, info.MusicID, info.HasLyrics())

	q := queryFor(info, filepath.Base(src))
	cands, err := search(ctx, client, q)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	d := lyric.Decide(q, cands, info.Duration, false)
	if !d.Auto {
		t.Fatalf("the real file was not matched confidently: %s (best id %d)", d.Reason, d.Best.MusicID)
	}
	t.Logf("matched id=%d %q score=%d (%v)", d.Best.MusicID, d.Best.Name, d.Best.Score, d.Best.Reasons)

	// The catalogue's own spelling goes along, the way the batch sends it: this
	// file is one of the tag-less ones, so the write is also what gives it a
	// name to be looked up by.
	credit := tag.Credit{MusicID: d.Best.MusicID, Title: d.Best.Name, Artists: d.Best.Artists, Album: d.Best.Album}
	out, err := Apply(ctx, client, info, credit, 2 /* embed + file */, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	t.Logf("wrote: embedded=%v lrc=%q", out.Embedded, out.LRC)

	after := flacAudioOffset(t, work)
	if after == offset {
		t.Error("the metadata chain did not change; nothing was written")
	}
	got, err := hashAudio(work, after)
	if err != nil {
		t.Fatal(err)
	}
	if got != before {
		t.Fatalf("the audio frames changed:\n before %s\n  after %s", before, got)
	}

	final, err := tag.Inspect(work)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after: id=%d hasLyrics=%v original=%d bytes translation=%d bytes merged=%d bytes",
		final.MusicID, final.HasLyrics(), len(final.Lyrics.Original), len(final.Lyrics.Translation), len(final.Lyrics.Merged))
	if !final.HasLyrics() {
		t.Error("the file does not report lyrics after the write")
	}
	if final.MusicID != d.Best.MusicID {
		t.Errorf("id = %d, want %d", final.MusicID, d.Best.MusicID)
	}
	if s, err := os.Stat(SidecarPath(work)); err != nil {
		t.Errorf("no sidecar: %v", err)
	} else {
		t.Logf("sidecar: %d bytes", s.Size())
	}

	// A second pass must change nothing at all.
	again, err := Apply(ctx, client, final, credit, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Embedded {
		t.Error("a second pass rewrote a file that already had these lyrics")
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return out.Sync()
}

func hashAudio(path string, offset int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return string(h.Sum(nil)), nil
}
