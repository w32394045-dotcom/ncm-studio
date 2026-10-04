package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ncm-studio/internal/lyric"
	"ncm-studio/internal/ncm"
)

const samplePath = "/storage/emulated/0/Download/netease/cloudmusic/Music/Lucenzo Don Omar - Danza Kuduro.ncm"

func requireSample(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(samplePath); err != nil {
		t.Skipf("sample not available: %v", err)
	}
	return samplePath
}

// TestProcessEmbedsLyrics runs the whole pipeline against a real container and
// checks the result with an independent decoder.
func TestProcessEmbedsLyrics(t *testing.T) {
	src := requireSample(t)
	out := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := lyric.NewClient(t.TempDir())
	res, err := Process(ctx, src, Options{
		OutputDir: out,
		Lyrics:    lyricModeForTest(),
		Client:    client,
	}, nil)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	for _, w := range res.Warnings {
		t.Logf("warning: %s", w)
	}
	if res.Format != ncm.FormatFLAC {
		t.Fatalf("format = %q, want flac", res.Format)
	}
	if filepath.Base(res.OutputPath) != "Lucenzo, Don Omar - Danza Kuduro.flac" {
		t.Errorf("output name = %q", filepath.Base(res.OutputPath))
	}
	if fi, err := os.Stat(res.OutputPath); err != nil || fi.Size() < 100<<20 {
		t.Errorf("output looks wrong: %v %d", err, res.Bytes)
	}

	// No stray temporary file may survive a successful run.
	if _, err := os.Stat(res.OutputPath + ".part"); !os.IsNotExist(err) {
		t.Error("temporary .part file was left behind")
	}

	if res.Lyrics == nil {
		t.Fatal("no lyrics attached to the result")
	}
	if !res.Lyrics.HasTranslation() {
		t.Error("expected a translation for this track")
	}
	assertSidecar(t, res)
	assertTagsWithFFprobe(t, res.OutputPath)
}

func lyricModeForTest() LyricsMode { return LyricsEmbedAndFile }

// assertSidecar checks the .lrc beside the audio carries bilingual lines.
func assertSidecar(t *testing.T, res *Result) {
	t.Helper()
	if res.LRCPath == "" {
		t.Fatal("no .lrc sidecar written")
	}
	if filepath.Ext(res.LRCPath) != ".lrc" {
		t.Errorf("sidecar extension = %q", filepath.Ext(res.LRCPath))
	}
	if strings.TrimSuffix(res.LRCPath, ".lrc") != strings.TrimSuffix(res.OutputPath, ".flac") {
		t.Error("sidecar name does not match the audio file")
	}
	data, err := os.ReadFile(res.LRCPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "[00:") {
		t.Error("sidecar has no timestamps")
	}
	// A translated line must sit directly under its original.
	if !strings.Contains(body, "国王啊！") {
		t.Error("sidecar is missing translated lines")
	}
}

// assertTagsWithFFprobe decodes the file with an independent implementation,
// which is the only way to know the tag layout is actually correct rather than
// merely self-consistent.
func assertTagsWithFFprobe(t *testing.T, path string) {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not available")
	}

	out, err := exec.Command(ffprobe,
		"-v", "error",
		"-show_entries", "format_tags",
		"-of", "default=noprint_wrappers=1",
		path,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	tags := string(out)

	for _, want := range []string{
		"TITLE=Danza Kuduro",
		"ALBUM=Ritmo Do Brasil",
		"LYRICS=",
	} {
		if !strings.Contains(strings.ToLower(tags), strings.ToLower(want)) {
			t.Errorf("ffprobe output is missing %q:\n%s", want, firstLines(tags, 40))
		}
	}
	// ffprobe renders repeated ARTIST entries as one semicolon-joined value,
	// so both names have to be looked for inside that single line.
	artistLine := ""
	for _, line := range strings.Split(tags, "\n") {
		if strings.HasPrefix(strings.ToUpper(line), "TAG:ARTIST=") {
			artistLine = line
			break
		}
	}
	for _, name := range []string{"Lucenzo", "Don Omar"} {
		if !strings.Contains(artistLine, name) {
			t.Errorf("artist %q is missing from %q", name, artistLine)
		}
	}
	if !strings.Contains(tags, "国王啊！") {
		t.Errorf("embedded lyrics have no translation:\n%s", firstLines(tags, 40))
	}

	// The audio itself must still decode end to end.
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return
	}
	if out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg decode: %v\n%s", err, out)
	} else if len(out) > 0 {
		t.Errorf("decoder reported errors:\n%s", out)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestProcessWithoutLyrics(t *testing.T) {
	src := requireSample(t)
	out := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := Process(ctx, src, Options{OutputDir: out, Lyrics: LyricsOff}, nil)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Lyrics != nil || res.LRCPath != "" {
		t.Error("lyrics were fetched despite LyricsOff")
	}
	if fi, err := os.Stat(res.OutputPath); err != nil || fi.Size() < 100<<20 {
		t.Errorf("output looks wrong: %v", err)
	}
}

func TestProcessRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.ncm")
	if err := os.WriteFile(bad, []byte("this is not an ncm file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Process(context.Background(), bad, Options{OutputDir: dir, Lyrics: LyricsOff}, nil)
	if err == nil {
		t.Fatal("expected an error for a non-NCM file")
	}
	if !strings.Contains(err.Error(), "magic") {
		t.Errorf("error should mention the magic check, got: %v", err)
	}
}

func TestProcessHonoursCancellation(t *testing.T) {
	src := requireSample(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the copy starts

	_, err := Process(ctx, src, Options{OutputDir: t.TempDir(), Lyrics: LyricsOff}, nil)
	if err == nil {
		t.Fatal("expected the cancelled context to abort the run")
	}
}

// TestLyricsModePredicates pins the three questions a mode answers. They are
// separate because a mode can need the fetch without needing either write, and
// conflating them is how lyrics end up embedded in a file the user asked to
// leave alone.
func TestLyricsModePredicates(t *testing.T) {
	cases := []struct {
		mode               LyricsMode
		fetch, embed, file bool
	}{
		{LyricsOff, false, false, false},
		{LyricsEmbed, true, true, false},
		{LyricsEmbedAndFile, true, true, true},
		{LyricsFileOnly, true, false, true},
	}
	for _, c := range cases {
		if got := c.mode.WantsLyrics(); got != c.fetch {
			t.Errorf("mode %d: WantsLyrics = %v, want %v", c.mode, got, c.fetch)
		}
		if got := c.mode.WantsEmbed(); got != c.embed {
			t.Errorf("mode %d: WantsEmbed = %v, want %v", c.mode, got, c.embed)
		}
		if got := c.mode.WantsFile(); got != c.file {
			t.Errorf("mode %d: WantsFile = %v, want %v", c.mode, got, c.file)
		}
	}
}

// TestProcessFileOnlyLeavesTagsAlone runs the mode that exists to avoid
// rewriting a large file: the lyrics must reach the sidecar and nowhere else,
// and the tags the file would otherwise carry must be untouched.
func TestProcessFileOnlyLeavesTagsAlone(t *testing.T) {
	src := requireSample(t)
	out := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := Process(ctx, src, Options{
		OutputDir: out,
		Lyrics:    LyricsFileOnly,
		Client:    lyric.NewClient(t.TempDir()),
	}, nil)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	assertSidecar(t, res)

	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not available")
	}
	raw, err := exec.Command(ffprobe,
		"-v", "error",
		"-show_entries", "format_tags",
		"-of", "default=noprint_wrappers=1",
		res.OutputPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, raw)
	}
	tags := string(raw)

	if strings.Contains(strings.ToLower(tags), "lyrics=") {
		t.Errorf("sidecar-only mode embedded lyrics anyway:\n%s", firstLines(tags, 30))
	}
	// The ordinary tags still have to be there — not embedding lyrics must not
	// turn into not tagging the file at all.
	for _, want := range []string{"TITLE=Danza Kuduro", "ALBUM=Ritmo Do Brasil"} {
		if !strings.Contains(tags, want) {
			t.Errorf("ffprobe output is missing %q:\n%s", want, firstLines(tags, 30))
		}
	}
}
