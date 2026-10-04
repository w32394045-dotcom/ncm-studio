package backfill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ncm-studio/internal/job"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// lrcHarness is the file half of the test world: a settings store and a
// processor, with no catalogue behind it — the .lrc page never fetches.
type lrcHarness struct {
	dir       string
	store     *store.Store
	processor job.Processor
}

func newLRCHarness(t *testing.T) *lrcHarness {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "config"), store.DefaultConfig(home))
	if err != nil {
		t.Fatal(err)
	}
	return &lrcHarness{
		dir:       home,
		store:     st,
		processor: LRCProcessor(LRCOptions{Store: st}),
	}
}

func (h *lrcHarness) set(t *testing.T, fn func(*store.Config)) {
	t.Helper()
	if err := h.store.Update(fn); err != nil {
		t.Fatal(err)
	}
}

// flac writes a minimal FLAC carrying the given tags, the same fixture the
// backfill tests use.
func (h *lrcHarness) flac(t *testing.T, name string, tags *tag.Tags) string {
	t.Helper()
	if tags == nil {
		tags = &tag.Tags{}
	}
	si := make([]byte, 34)
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40 // 44100 Hz
	streamInfo := append([]byte{0, 0, 0, 34}, si...)
	prefix := tag.BuildFLACMetadata(&tag.FLACSource{StreamInfo: streamInfo}, tags)
	audio := make([]byte, 4096)
	for i := range audio {
		audio[i] = byte(i)
	}
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (h *lrcHarness) run(t *testing.T, op, path string) job.Result {
	t.Helper()
	res, err := h.processor(context.Background(), &job.Item{ID: path, Path: path, Op: op},
		func(string, int64, int64) {})
	if err != nil {
		t.Fatalf("%s %s: %v", op, filepath.Base(path), err)
	}
	return res
}

func fileModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

const sidecarLyrics = "[00:01.000]きみのこえ\n[00:05.000]ひめごと\n"

func TestLRCExportsBesideTheFile(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{
		Title: "ひめごと", Artists: []string{"高野麻里佳"},
		Lyrics: sidecarLyrics,
	})

	res := h.run(t, LRCExport, path)
	want := filepath.Join(h.dir, "song.lrc")
	if res.LRC != want {
		t.Fatalf("exported to %q, want %q", res.LRC, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sidecarLyrics {
		t.Errorf(".lrc holds %q, want %q", got, sidecarLyrics)
	}
}

func TestLRCExportLeavesTheAudioAlone(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: sidecarLyrics})
	before := fileBytes(t, path)

	h.run(t, LRCExport, path)
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("export rewrote the audio file")
	}
}

func TestLRCExportSkipsAFileWithNoLyrics(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T"})

	res := h.run(t, LRCExport, path)
	if res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "song.lrc")); !os.IsNotExist(err) {
		t.Error("an empty .lrc was written")
	}
}

func TestLRCExportDoesNotRewriteAnIdenticalFile(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: sidecarLyrics})
	target := filepath.Join(h.dir, "song.lrc")
	if err := os.WriteFile(target, []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}
	// A timestamp the test can watch: a second write would move it.
	old := fileModTime(t, target)

	h.run(t, LRCExport, path)
	if got := fileModTime(t, target); !got.Equal(old) {
		t.Error("an identical .lrc was rewritten")
	}
}

func TestLRCExportHonoursSkip(t *testing.T) {
	h := newLRCHarness(t)
	h.set(t, func(c *store.Config) { c.LRCConflict = store.ConflictSkip })
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: sidecarLyrics})

	target := filepath.Join(h.dir, "song.lrc")
	if err := os.WriteFile(target, []byte("[00:01.000]手で書いた\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := h.run(t, LRCExport, path)
	if res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
	if got := string(fileBytes(t, target)); !strings.Contains(got, "手で書いた") {
		t.Errorf("the existing .lrc was replaced: %q", got)
	}
}

func TestLRCExportMergesBothDocuments(t *testing.T) {
	h := newLRCHarness(t)
	h.set(t, func(c *store.Config) { c.LRCConflict = store.ConflictMerge })
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: sidecarLyrics})

	target := filepath.Join(h.dir, "song.lrc")
	if err := os.WriteFile(target, []byte("[00:01.000]手で書いた\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.run(t, LRCExport, path)
	got := string(fileBytes(t, target))
	for _, want := range []string{"手で書いた", "きみのこえ", "ひめごと"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged .lrc dropped %q:\n%s", want, got)
		}
	}
}

func TestLRCExportIntoAChosenFolderNamesByTrack(t *testing.T) {
	h := newLRCHarness(t)
	out := filepath.Join(h.dir, "lyrics")
	h.set(t, func(c *store.Config) { c.LCROutput = out })

	path := h.flac(t, "01 - track.flac", &tag.Tags{
		Title: "ひめごと*クライシスターズ", Artists: []string{"高野麻里佳", "石原夏織"},
		Lyrics: sidecarLyrics,
	})

	res := h.run(t, LRCExport, path)
	want := filepath.Join(out, "高野麻里佳, 石原夏織 - ひめごと_クライシスターズ.lrc")
	if res.LRC != want {
		t.Fatalf("exported to %q, want %q", res.LRC, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
	// Two tracks of one album must not collide on "01 - track".
	if strings.Contains(filepath.Base(res.LRC), "01 - ") {
		t.Errorf("named from the file name: %q", res.LRC)
	}
}

func TestLRCPathMakesATitleSafeToWrite(t *testing.T) {
	info := &tag.AudioInfo{
		Path:    "/x/01.flac",
		Title:   "AC/DC: Back?",
		Artists: []string{"A|B"},
	}
	got := LRCPath(info, "/out")
	if want := filepath.Join("/out", "A_B - AC_DC_ Back_.lrc"); got != want {
		t.Errorf("LRCPath = %q, want %q", got, want)
	}
}

func TestLRCImportWritesTheSidecarIntoTags(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T"})
	before := fileBytes(t, path)

	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}
	res := h.run(t, LRCImport, path)
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s)", res.State, res.Reason)
	}

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasLyrics() || !strings.Contains(info.Lyrics.Merged, "きみのこえ") {
		t.Errorf("tags hold %q", info.Lyrics.Merged)
	}
	if equalBytes(before, fileBytes(t, path)) {
		t.Error("the audio file was not rewritten")
	}
}

func TestLRCImportSkipsWithoutASidecar(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T"})
	before := fileBytes(t, path)

	res := h.run(t, LRCImport, path)
	if res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the audio file was rewritten with nothing to write")
	}
}

// A second import over the same folder must cost nothing: the whole point of
// the contains check is that re-running a batch does not copy every file again.
func TestLRCImportTwiceDoesNotRewrite(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T"})
	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}

	h.run(t, LRCImport, path)
	once := fileBytes(t, path)
	h.run(t, LRCImport, path)
	if !equalBytes(once, fileBytes(t, path)) {
		t.Error("the second import rewrote the file")
	}
}

func TestLRCImportOverwriteReplacesTheLyrics(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: "[00:09.000]古い歌詞\n"})
	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}

	h.run(t, LRCImport, path)
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Lyrics.Merged, "古い歌詞") {
		t.Errorf("the old lyrics survived the default overwrite: %q", info.Lyrics.Merged)
	}
	if !strings.Contains(info.Lyrics.Merged, "きみのこえ") {
		t.Errorf("the .lrc did not land: %q", info.Lyrics.Merged)
	}
}

func TestLRCImportSkipLeavesTheFileAlone(t *testing.T) {
	h := newLRCHarness(t)
	h.set(t, func(c *store.Config) { c.LRCConflict = store.ConflictSkip })
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: "[00:09.000]古い歌詞\n"})
	before := fileBytes(t, path)
	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}

	res := h.run(t, LRCImport, path)
	if res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the file was rewritten under ConflictSkip")
	}
}

func TestLRCImportMergeKeepsBoth(t *testing.T) {
	h := newLRCHarness(t)
	h.set(t, func(c *store.Config) { c.LRCConflict = store.ConflictMerge })
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T", Lyrics: "[00:09.000]古い歌詞\n"})
	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}

	h.run(t, LRCImport, path)
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"古い歌詞", "きみのこえ", "ひめごと"} {
		if !strings.Contains(info.Lyrics.Merged, want) {
			t.Errorf("merged tags dropped %q:\n%s", want, info.Lyrics.Merged)
		}
	}
}

// The translation is its own timeline and an import of words must not touch it.
func TestLRCImportOverwriteKeepsTheTranslation(t *testing.T) {
	h := newLRCHarness(t)
	path := h.flac(t, "song.flac", &tag.Tags{Title: "T"})

	// What an earlier AI run left in the file: a translation of the old words,
	// labelled with who wrote it.
	err := tag.SetLyrics(path, tag.Lyrics{
		Merged:            "[00:09.000]古い歌詞\n",
		Original:          "[00:09.000]古い歌詞\n",
		Translation:       "[00:09.000]old words\n",
		TranslationSource: "deepseek",
		TranslationModel:  "deepseek-chat",
	}, tag.Credit{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}
	h.run(t, LRCImport, path)

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Lyrics.Translation, "old words") {
		t.Errorf("the translation was discarded: %q", info.Lyrics.Translation)
	}
	if info.Lyrics.TranslationSource != "deepseek" {
		t.Errorf("the translation source was discarded: %q", info.Lyrics.TranslationSource)
	}
	if !strings.Contains(info.Lyrics.Merged, "きみのこえ") {
		t.Errorf("the .lrc did not land: %q", info.Lyrics.Merged)
	}
}

// The export folder is consulted in both directions, so a library whose lyrics
// live elsewhere still has something to import.
func TestLRCImportReadsFromTheExportFolder(t *testing.T) {
	h := newLRCHarness(t)
	out := filepath.Join(h.dir, "lyrics")
	h.set(t, func(c *store.Config) { c.LCROutput = out })
	path := h.flac(t, "song.flac", &tag.Tags{Title: "ひめごと", Artists: []string{"高野麻里佳"}})
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(out, "高野麻里佳 - ひめごと.lrc")
	if err := os.WriteFile(target, []byte(sidecarLyrics), 0o644); err != nil {
		t.Fatal(err)
	}

	res := h.run(t, LRCImport, path)
	if res.LRC != target {
		t.Fatalf("imported from %q, want %q", res.LRC, target)
	}
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Lyrics.Merged, "きみのこえ") {
		t.Errorf("tags hold %q", info.Lyrics.Merged)
	}
}
