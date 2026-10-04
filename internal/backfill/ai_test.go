package backfill

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ncm-studio/internal/ai"
	"ncm-studio/internal/job"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// translator is a stand-in provider: it records what it was asked and replies
// with a fixed answer.
type translator struct {
	*httptest.Server
	asked []string
	reply string
}

func newTranslator(t *testing.T, reply string) *translator {
	t.Helper()
	tr := &translator{reply: reply}
	tr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		for _, m := range body.Messages {
			tr.asked = append(tr.asked, m.Content)
		}
		io.WriteString(w, tr.reply)
	}))
	t.Cleanup(tr.Close)
	return tr
}

// aiHarness is a settings store, a provider and a processor.
type aiHarness struct {
	dir       string
	store     *store.Store
	tr        *translator
	processor job.Processor
}

func newAIHarness(t *testing.T, mode store.LyricsMode, target, provider string) *aiHarness {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "config"), store.DefaultConfig(home))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *store.Config) {
		c.Lyrics = mode
		c.AITarget = target
		c.AIProvider = provider
		c.AIModel = "test-model"
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAIKey(provider, "test-key"); err != nil {
		t.Fatal(err)
	}

	h := &aiHarness{dir: home, store: st}
	h.processor = AIProcessor(AIOptions{
		Store: st,
		NewClient: func(o ai.Options) (*ai.Client, error) {
			o.Endpoint = h.tr.URL
			return ai.New(o)
		},
	})
	return h
}

func (h *aiHarness) withTranslator(t *testing.T, reply string) {
	t.Helper()
	h.tr = newTranslator(t, reply)
}

func (h *aiHarness) write(t *testing.T, name string, tags *tag.Tags) string {
	t.Helper()
	si := make([]byte, 34)
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40
	prefix := tag.BuildFLACMetadata(&tag.FLACSource{StreamInfo: append([]byte{0, 0, 0, 34}, si...)}, tags)
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

func (h *aiHarness) run(t *testing.T, it *job.Item) job.Result {
	t.Helper()
	res, err := h.processor(context.Background(), it, func(string, int64, int64) {})
	if err != nil {
		t.Fatalf("processor: %v", err)
	}
	return res
}

const originalLRC = "[00:01.000]きみのこえ\n[00:05.000]ひめごと\n"

func TestAITranslatesAndMarksTheFile(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "简体中文", "deepseek")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"你的声音\",\"秘密\"]}"}}]}`)

	path := h.write(t, "song.flac", &tag.Tags{
		Title: "ひめごと", Artists: []string{"高野麻里佳"},
		Lyrics: originalLRC, LyricsOriginal: originalLRC,
	})

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s)", res.State, res.Reason)
	}

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Lyrics.Translation, "你的声音") {
		t.Errorf("translation = %q", info.Lyrics.Translation)
	}
	// The marking is the requirement: a reader must be able to tell a
	// machine's work from a person's.
	if info.Lyrics.TranslationSource != "deepseek" || info.Lyrics.TranslationModel != "test-model" {
		t.Errorf("marking = %q/%q", info.Lyrics.TranslationSource, info.Lyrics.TranslationModel)
	}
	if !info.Lyrics.MachineTranslated() {
		t.Error("MachineTranslated is false")
	}
	// Both languages in the merged timeline, so any player renders them.
	if !strings.Contains(info.Lyrics.Merged, "きみのこえ") || !strings.Contains(info.Lyrics.Merged, "你的声音") {
		t.Errorf("merged = %q", info.Lyrics.Merged)
	}
	// The name was already there and must not have been replaced.
	if info.Title != "ひめごと" || info.Artists[0] != "高野麻里佳" {
		t.Errorf("identity changed: %q %v", info.Title, info.Artists)
	}
	if len(h.tr.asked) == 0 || !strings.Contains(strings.Join(h.tr.asked, " "), "简体中文") {
		t.Error("the target language was not asked for")
	}
}

// The .lrc is a source of words too, which is what makes the page usable on a
// file that was never tagged.
func TestAITranslatesFromASidecar(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)

	path := h.write(t, "song.flac", &tag.Tags{Title: "T"})
	if err := os.WriteFile(filepath.Join(h.dir, "song.lrc"), []byte(originalLRC), 0o644); err != nil {
		t.Fatal(err)
	}

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s)", res.State, res.Reason)
	}

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Lyrics.Translation, "Your voice") {
		t.Errorf("translation = %q", info.Lyrics.Translation)
	}
	// The words it translated are written alongside, so the file is not a
	// translation of a text it does not carry.
	if !strings.Contains(info.Lyrics.Original, "きみのこえ") {
		t.Errorf("original = %q", info.Lyrics.Original)
	}
}

// A file with a machine translation is left alone unless a re-run is asked for:
// translating a translation is how a lyric degrades.
func TestAISkipsWhatItAlreadyTranslated(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)

	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	if err := tag.SetLyrics(path, tag.Lyrics{
		Merged: originalLRC + "[00:01.000]Your voice\n", Original: originalLRC,
		Translation: "[00:01.000]Your voice\n", TranslationSource: "openai", TranslationModel: "gpt-4o-mini",
	}, tag.Credit{}, nil); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, path)

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the file was rewritten")
	}
	if len(h.tr.asked) != 0 {
		t.Error("a request was made for a file that needed none")
	}
}

// The standing setting can say that a translated file is redone. It is the
// whole reason the setting exists: without it the only way to redo anything is
// the per-batch force box, which redoes everything.
func TestAIRedoesWhatItAlreadyTranslatedWhenTheSettingSaysSo(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)
	if err := h.store.Update(func(c *store.Config) { c.AITranslated = store.AITranslatedRedo }); err != nil {
		t.Fatal(err)
	}

	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	if err := tag.SetLyrics(path, tag.Lyrics{
		Merged: originalLRC, Original: originalLRC,
		Translation: "[00:01.000]stale\n", TranslationSource: "openai",
	}, tag.Credit{}, nil); err != nil {
		t.Fatal(err)
	}

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.State == job.Skipped {
		t.Fatalf("the setting said to redo, but the file was skipped: %s", res.Reason)
	}
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Lyrics.Translation, "stale") {
		t.Errorf("the old translation survived: %q", info.Lyrics.Translation)
	}
}

// Under the "choose one by one" setting a translated file still waits for a
// decision — and the decision is what the row's own Force carries. This is the
// half the web layer has to send; without it the tick would do nothing.
func TestAIPickPolicyWaitsForTheRowsOwnForce(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)
	if err := h.store.Update(func(c *store.Config) { c.AITranslated = store.AITranslatedPick }); err != nil {
		t.Fatal(err)
	}

	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	if err := tag.SetLyrics(path, tag.Lyrics{
		Merged: originalLRC, Original: originalLRC,
		Translation: "[00:01.000]stale\n", TranslationSource: "openai",
	}, tag.Credit{}, nil); err != nil {
		t.Fatal(err)
	}

	if res := h.run(t, &job.Item{ID: path, Path: path}); res.State != job.Skipped {
		t.Errorf("an unticked row ran anyway: %q", res.State)
	}
	if res := h.run(t, &job.Item{ID: path, Path: path, Force: true}); res.State == job.Skipped {
		t.Errorf("a ticked row was skipped: %s", res.Reason)
	}
}

func TestAIForceRetranslates(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)

	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	if err := tag.SetLyrics(path, tag.Lyrics{
		Merged: originalLRC, Original: originalLRC,
		Translation: "[00:01.000]stale\n", TranslationSource: "openai",
	}, tag.Credit{}, nil); err != nil {
		t.Fatal(err)
	}

	res := h.run(t, &job.Item{ID: path, Path: path, Force: true})
	if res.State == job.Skipped {
		t.Fatalf("a forced run was skipped: %s", res.Reason)
	}
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Lyrics.Translation, "stale") {
		t.Errorf("the old translation survived: %q", info.Lyrics.Translation)
	}
}

func TestAISkipsAFileWithNothingToTranslate(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{}`)
	path := h.write(t, "song.flac", &tag.Tags{Title: "T"})

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
}

func TestAISkipsWithoutAKey(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{}`)
	if err := h.store.SetAIKey("openai", ""); err != nil {
		t.Fatal(err)
	}
	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.State != job.Skipped || !strings.Contains(res.Reason, "key") {
		t.Errorf("state = %q reason = %q", res.State, res.Reason)
	}
}

func TestAISkipsWhenNoTargetLanguageIsSet(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "", "openai")
	h.withTranslator(t, `{}`)
	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})

	if res := h.run(t, &job.Item{ID: path, Path: path}); res.State != job.Skipped {
		t.Errorf("state = %q, want skipped", res.State)
	}
}

// The file-only mode writes a sidecar and does not copy the audio; the whole
// point of that mode is that a translation of a hundred-megabyte file costs
// nothing.
func TestAIFileOnlyModeLeavesTheAudioAlone(t *testing.T) {
	h := newAIHarness(t, store.LyricsFileOnly, "English", "openai")
	h.withTranslator(t, `{"choices":[{"message":{"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)
	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	before := fileBytes(t, path)

	res := h.run(t, &job.Item{ID: path, Path: path})
	if res.LRC == "" {
		t.Fatalf("no .lrc was written: %+v", res)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the audio file was rewritten in file-only mode")
	}
	body, err := os.ReadFile(res.LRC)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Your voice") {
		t.Errorf(".lrc holds %q", body)
	}
}

// A provider failure is a failure, not a file written with a broken lyric.
func TestAIFailureLeavesTheFileAlone(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "openai")
	h.withTranslator(t, `{"error":{"message":"rate limited"}}`)
	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	before := fileBytes(t, path)

	if _, err := h.processor(context.Background(), &job.Item{ID: path, Path: path},
		func(string, int64, int64) {}); err == nil {
		t.Fatal("a provider error was swallowed")
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the file was rewritten after a failed translation")
	}
}

// The DeepSeek trap, at the level that matters: the chain of thought must not
// reach the file.
func TestAIReasoningNeverReachesTheFile(t *testing.T) {
	h := newAIHarness(t, store.LyricsEmbed, "English", "deepseek")
	h.withTranslator(t, `{"choices":[{"message":{
		"reasoning_content":"The user wants a translation. First I will consider each line...",
		"content":"{\"lines\":[\"Your voice\",\"A secret\"]}"}}]}`)

	path := h.write(t, "song.flac", &tag.Tags{Title: "T", Lyrics: originalLRC, LyricsOriginal: originalLRC})
	h.run(t, &job.Item{ID: path, Path: path})

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	whole := info.Lyrics.Merged + info.Lyrics.Translation + info.Lyrics.Original
	if strings.Contains(whole, "First I will consider") {
		t.Fatalf("the reasoning was written into the file:\n%s", whole)
	}
	if !strings.Contains(info.Lyrics.Translation, "Your voice") {
		t.Errorf("translation = %q", info.Lyrics.Translation)
	}
}
