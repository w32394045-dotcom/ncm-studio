package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ncm-studio/internal/backfill"
	"ncm-studio/internal/job"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// recorder is a processor that remembers what it was asked to do.
type recorder struct {
	mu    sync.Mutex
	items []job.Item
	block chan struct{}
}

func (r *recorder) proc(ctx context.Context, it *job.Item, report job.Reporter) (job.Result, error) {
	r.mu.Lock()
	r.items = append(r.items, *it)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return job.Result{}, nil
}

func (r *recorder) seen() []job.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]job.Item(nil), r.items...)
}

// toolsServer is a server with the .lrc and translation pools wired to
// recorders, so a test can see what a batch was queued with.
type toolsServer struct {
	*httptest.Server
	store *store.Store
	lrc   *recorder
	ai    *recorder
}

func newToolsServer(t *testing.T) *toolsServer {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cfg"), store.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *store.Config) {
		c.Dir, c.Output, c.Workspace, c.Workers = root, filepath.Join(root, "out"), root, 1
	}); err != nil {
		t.Fatal(err)
	}

	idle := func(context.Context, *job.Item, job.Reporter) (job.Result, error) {
		return job.Result{}, nil
	}
	rec := &recorder{block: make(chan struct{})}
	ts := &toolsServer{
		store: st,
		lrc:   rec,
		ai:    &recorder{block: make(chan struct{})},
	}
	conv := job.New(1, idle)
	lrcPool := job.New(1, ts.lrc.proc)
	aiPool := job.New(1, ts.ai.proc)
	ts.Server = httptest.NewServer(New(st, Pools{
		Convert: conv, Lyrics: job.New(1, idle), LRC: lrcPool, AI: aiPool,
	}, backfill.NewSearcher(), nil, "test").Handler())
	t.Cleanup(func() {
		close(rec.block)
		close(ts.ai.block)
		lrcPool.Cancel()
		aiPool.Cancel()
		conv.Cancel()
		ts.Close()
	})
	return ts
}

// track writes a FLAC carrying the given tags.
func (ts *toolsServer) track(t *testing.T, name string, tags *tag.Tags) string {
	t.Helper()
	si := make([]byte, 34)
	si[10], si[11], si[12] = 0x0A, 0xC4, 0x40
	prefix := tag.BuildFLACMetadata(&tag.FLACSource{StreamInfo: append([]byte{0, 0, 0, 34}, si...)}, tags)
	path := filepath.Join(ts.store.Config().Dir, name)
	if err := os.WriteFile(path, append(prefix, make([]byte, 2048)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// post sends a request and returns its status and body, for the cases that
// expect a refusal.
func post(t *testing.T, url string, payload any) (int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(out)
}

// postInto sends a request and decodes its answer.
func postInto(t *testing.T, url string, payload, v any) {
	t.Helper()
	code, body := post(t, url, payload)
	if code != http.StatusOK {
		t.Fatalf("POST %s: %d %s", url, code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
}

// awaitItem waits for a queued item to reach the recorder.
func (ts *toolsServer) awaitItem(t *testing.T, rec *recorder) job.Item {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if items := rec.seen(); len(items) > 0 {
			return items[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing was queued")
	return job.Item{}
}

func TestLRCStartQueuesTheDirection(t *testing.T) {
	ts := newToolsServer(t)
	a := ts.track(t, "a.flac", &tag.Tags{Title: "A"})
	b := ts.track(t, "b.flac", &tag.Tags{Title: "B"})

	if code, body := post(t, ts.URL+"/api/lrc/start", map[string]any{
		"paths": []string{a, b}, "op": "import",
	}); code != http.StatusOK {
		t.Fatalf("start: %d %s", code, body)
	}

	it := ts.awaitItem(t, ts.lrc)
	if it.Op != backfill.LRCImport {
		t.Errorf("op = %q, want %q", it.Op, backfill.LRCImport)
	}
	if it.ID != a {
		t.Errorf("id = %q, want %q", it.ID, a)
	}
}

func TestLRCStartRefusesAnUnknownDirection(t *testing.T) {
	ts := newToolsServer(t)
	path := ts.track(t, "a.flac", &tag.Tags{Title: "A"})

	code, body := post(t, ts.URL+"/api/lrc/start", map[string]any{
		"paths": []string{path}, "op": "sideways",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(body, "op must be") {
		t.Errorf("body = %s", body)
	}
	if len(ts.lrc.seen()) != 0 {
		t.Error("a batch was queued anyway")
	}
}

func TestLRCStartRefusesAnEmptySelection(t *testing.T) {
	ts := newToolsServer(t)
	if code, _ := post(t, ts.URL+"/api/lrc/start", map[string]any{"paths": []string{}, "op": "export"}); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestLRCCancelStopsTheBatch(t *testing.T) {
	ts := newToolsServer(t)
	path := ts.track(t, "a.flac", &tag.Tags{Title: "A"})
	post(t, ts.URL+"/api/lrc/start", map[string]any{"paths": []string{path}, "op": "export"})
	ts.awaitItem(t, ts.lrc)

	postJSON(t, ts.URL+"/api/lrc/cancel", map[string]any{})
	var state poolState
	getJSON(t, ts.URL+"/api/lrc/state", &state)
	if state.Running {
		t.Error("the batch is still running after a cancel")
	}
}

func TestLyricsViewReadsTagsAndSidecar(t *testing.T) {
	ts := newToolsServer(t)
	path := ts.track(t, "a.flac", &tag.Tags{
		Title: "T", Lyrics: "[00:01.000]words\n", LyricsTranslation: "[00:01.000]translated\n",
	})
	if err := os.WriteFile(strings.TrimSuffix(path, ".flac")+".lrc", []byte("[00:01.000]sidecar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var view lyricsView
	getJSON(t, ts.URL+"/api/lyrics/view?path="+path, &view)
	if !strings.Contains(view.Merged, "words") {
		t.Errorf("merged = %q", view.Merged)
	}
	if !strings.Contains(view.Translation, "translated") {
		t.Errorf("translation = %q", view.Translation)
	}
	if !strings.Contains(view.LRC, "sidecar") {
		t.Errorf("lrc = %q", view.LRC)
	}
	if view.LRCPath != strings.TrimSuffix(path, ".flac")+".lrc" {
		t.Errorf("lrcPath = %q", view.LRCPath)
	}
}

func TestLyricsViewNeedsAPath(t *testing.T) {
	ts := newToolsServer(t)
	resp, err := http.Get(ts.URL + "/api/lyrics/view")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The key must never travel to the page: the settings endpoint is what the
// browser loads on every visit, and anything it carries is readable by whatever
// can reach the UI.
func TestAIProviderListNeverCarriesAKey(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.SetAIKey("openai", "sk-secret-value"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/api/ai/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "sk-secret-value") {
		t.Fatalf("the key was sent to the page:\n%s", raw)
	}

	var view aiSettingsView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range view.Providers {
		if p.ID == "openai" {
			found = true
			if !p.HasKey {
				t.Error("the stored key was not reported")
			}
		}
	}
	if !found {
		t.Error("openai is missing from the provider list")
	}
}

func TestAIKeyStoresAndClears(t *testing.T) {
	ts := newToolsServer(t)

	var view aiSettingsView
	code, body := post(t, ts.URL+"/api/ai/key", map[string]string{"provider": "deepseek", "key": "  sk-x  "})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &view)
	if !ts.store.HasAIKey("deepseek") {
		t.Fatal("the key was not stored")
	}
	if got := ts.store.AIKey("deepseek"); got != "sk-x" {
		t.Errorf("stored key = %q, want it trimmed", got)
	}

	post(t, ts.URL+"/api/ai/key", map[string]string{"provider": "deepseek", "key": ""})
	if ts.store.HasAIKey("deepseek") {
		t.Error("an empty key did not clear the stored one")
	}
}

func TestAIKeyRejectsAnUnknownProvider(t *testing.T) {
	ts := newToolsServer(t)
	if code, _ := post(t, ts.URL+"/api/ai/key", map[string]string{"provider": "acme", "key": "x"}); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

// A batch that would do nothing says so instead of filling the list with skips.
func TestAIStartRefusesWithoutALanguage(t *testing.T) {
	ts := newToolsServer(t)
	path := ts.track(t, "a.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n"})

	code, body := post(t, ts.URL+"/api/ai/start", map[string]any{"paths": []string{path}})
	if code != http.StatusBadRequest || !strings.Contains(body, "target language") {
		t.Fatalf("status = %d body = %s", code, body)
	}
}

func TestAIStartRefusesWithoutAKey(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.Update(func(c *store.Config) { c.AITarget = "English"; c.AIProvider = "gemini" }); err != nil {
		t.Fatal(err)
	}
	path := ts.track(t, "a.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n"})

	code, body := post(t, ts.URL+"/api/ai/start", map[string]any{"paths": []string{path}})
	if code != http.StatusBadRequest || !strings.Contains(body, "gemini") {
		t.Fatalf("status = %d body = %s", code, body)
	}
}

func TestAIStartCarriesTheForceFlag(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.Update(func(c *store.Config) { c.AITarget = "English"; c.AIProvider = "openai" }); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetAIKey("openai", "sk-x"); err != nil {
		t.Fatal(err)
	}
	path := ts.track(t, "a.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n"})

	post(t, ts.URL+"/api/ai/start", map[string]any{"paths": []string{path}, "force": true})
	it := ts.awaitItem(t, ts.ai)
	if !it.Force {
		t.Error("the force flag did not reach the item")
	}
}

// A row ticked by hand reaches the processor forced, and its neighbours do not:
// the standing setting can say "leave translated files alone", and the tick is
// one file's answer to that. Without this half the tick would silently do
// nothing, because the batch-level flag is off.
func TestAIStartCarriesTheRowsOwnForce(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.Update(func(c *store.Config) { c.AITarget = "English"; c.AIProvider = "openai" }); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetAIKey("openai", "sk-x"); err != nil {
		t.Fatal(err)
	}
	a := ts.track(t, "a.flac", &tag.Tags{Title: "A", Lyrics: "[00:01.000]x\n"})
	b := ts.track(t, "b.flac", &tag.Tags{Title: "B", Lyrics: "[00:01.000]y\n"})

	post(t, ts.URL+"/api/ai/start", map[string]any{
		"paths":      []string{a, b},
		"force":      false,
		"forcePaths": []string{b},
	})

	// Read from the pool rather than from the recorder: the recorder blocks
	// until cleanup, so with one worker the second item would never arrive.
	var state struct {
		Items []struct {
			Path  string `json:"path"`
			Force bool   `json:"force"`
		} `json:"items"`
	}
	resp, err := http.Get(ts.URL + "/api/ai/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range state.Items {
		got[it.Path] = it.Force
	}
	if len(got) != 2 {
		t.Fatalf("the batch queued %d item(s), want 2", len(got))
	}
	if got[a] {
		t.Error("a path that was not ticked was forced anyway")
	}
	if !got[b] {
		t.Error("the ticked row did not reach the pool forced")
	}
}

func TestAIPreviewSaysWhyAFileWouldBeSkipped(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.Update(func(c *store.Config) { c.AITarget = "English"; c.AIProvider = "openai" }); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetAIKey("openai", "sk-x"); err != nil {
		t.Fatal(err)
	}

	ready := ts.track(t, "ready.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n[00:02.000]y\n"})
	empty := ts.track(t, "empty.flac", &tag.Tags{Title: "T"})
	sourced := ts.track(t, "sidecar.flac", &tag.Tags{Title: "T"})
	if err := os.WriteFile(strings.TrimSuffix(sourced, ".flac")+".lrc", []byte("[00:03.000]z\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got aiPreview
	postInto(t, ts.URL+"/api/ai/preview", map[string]string{"path": ready}, &got)
	if !got.Ready || got.Lines != 2 {
		t.Errorf("ready file: %+v", got)
	}
	if got.Provider != "openai" || got.Target != "English" {
		t.Errorf("provider/target = %s/%s", got.Provider, got.Target)
	}

	postInto(t, ts.URL+"/api/ai/preview", map[string]string{"path": empty}, &got)
	if got.Ready || !strings.Contains(got.Reason, "no lyrics") {
		t.Errorf("empty file: %+v", got)
	}

	postInto(t, ts.URL+"/api/ai/preview", map[string]string{"path": sourced}, &got)
	if !got.Ready || got.From != "lrc" || !strings.Contains(got.Source, "z") {
		t.Errorf("sidecar file: %+v", got)
	}
}

// The preview describes what a run would do with this file, so it has to agree
// with the run: under "never skip" a translated file is ready, and under the
// two settings that leave it to a person it is not.
func TestAIPreviewFollowsTheTranslatedPolicy(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.Update(func(c *store.Config) { c.AITarget = "English"; c.AIProvider = "openai" }); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetAIKey("openai", "sk-x"); err != nil {
		t.Fatal(err)
	}
	path := ts.track(t, "done.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n", LyricsOriginal: "[00:01.000]x\n"})
	if err := tag.SetLyrics(path, tag.Lyrics{
		Merged: "[00:01.000]x\n", Original: "[00:01.000]x\n",
		Translation: "[00:01.000]y\n", TranslationSource: "openai",
	}, tag.Credit{}, nil); err != nil {
		t.Fatal(err)
	}

	var got aiPreview
	postInto(t, ts.URL+"/api/ai/preview", map[string]string{"path": path}, &got)
	if got.Ready || !strings.Contains(got.Reason, "already carries") {
		t.Errorf("under the skip policy: %+v", got)
	}

	if err := ts.store.Update(func(c *store.Config) { c.AITranslated = store.AITranslatedRedo }); err != nil {
		t.Fatal(err)
	}
	postInto(t, ts.URL+"/api/ai/preview", map[string]string{"path": path}, &got)
	if !got.Ready {
		t.Errorf("under the redo policy the file was still refused: %+v", got)
	}
}

// The page has to be able to say "the model wrote this": the marking is the
// difference between a translation and a translation somebody checked.
func TestAudioScanReportsWhoTranslated(t *testing.T) {
	ts := newToolsServer(t)
	path := ts.track(t, "a.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n"})
	if err := tag.SetLyrics(path, tag.Lyrics{
		Merged: "[00:01.000]x\n", Original: "[00:01.000]x\n",
		Translation: "[00:01.000]y\n", TranslationSource: "deepseek", TranslationModel: "deepseek-chat",
	}, tag.Credit{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(path, ".flac")+".lrc", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var entries []audioEntry
	getJSON(t, ts.URL+"/api/audio?dir="+url.QueryEscape(ts.store.Config().Dir), &entries)
	if len(entries) != 1 {
		t.Fatalf("scanned %d entries", len(entries))
	}
	e := entries[0]
	if !e.HasTranslation || e.TranslationSource != "deepseek" || e.TranslationModel != "deepseek-chat" {
		t.Errorf("translation fields: %+v", e)
	}
	if !e.HasLRC {
		t.Error("the sidecar was not reported")
	}
	if !e.HasLyrics || !e.Writable {
		t.Errorf("basic fields: %+v", e)
	}
}

func TestConfigPatchCarriesTheToolSettings(t *testing.T) {
	ts := newToolsServer(t)

	postJSON(t, ts.URL+"/api/config", map[string]any{
		"lrcOutput":   "/tmp/lyrics",
		"lrcConflict": int(store.ConflictMerge),
		"aiTarget":    "日本語",
		"aiProvider":  "claude",
		"aiModel":     "claude-sonnet-5-5",
	})
	cfg := ts.store.Config()
	if cfg.LCROutput != "/tmp/lyrics" || cfg.LRCConflict != store.ConflictMerge {
		t.Errorf("lrc settings: %+v", cfg)
	}
	if cfg.AITarget != "日本語" || cfg.AIProvider != "claude" || cfg.AIModel != "claude-sonnet-5-5" {
		t.Errorf("ai settings: %+v", cfg)
	}

	// An unknown provider is ignored rather than stored: the run would refuse
	// it anyway, and a setting that cannot run is worse than the old one.
	postJSON(t, ts.URL+"/api/config", map[string]any{"aiProvider": "acme"})
	if got := ts.store.Config().AIProvider; got != "claude" {
		t.Errorf("provider = %q, want the previous one", got)
	}

	// An out-of-range conflict policy likewise.
	postJSON(t, ts.URL+"/api/config", map[string]any{"lrcConflict": 99})
	if got := ts.store.Config().LRCConflict; got != store.ConflictMerge {
		t.Errorf("conflict = %v, want the previous one", got)
	}

	// And the export folder can be cleared, which is how a user goes back to
	// sidecars beside the audio.
	postJSON(t, ts.URL+"/api/config", map[string]any{"lrcOutput": ""})
	if got := ts.store.Config().LCROutput; got != "" {
		t.Errorf("lrcOutput = %q, want it cleared", got)
	}
}

// The model follows the provider both ways: switching rewrites the field to
// whatever that provider was last used with, so the panel and the next batch
// cannot disagree about what will be called.
func TestConfigPatchRemembersTheModelPerProvider(t *testing.T) {
	ts := newToolsServer(t)

	postJSON(t, ts.URL+"/api/config", map[string]any{"aiProvider": "deepseek", "aiModel": "deepseek-reasoner"})
	if got := ts.store.Config().AIModel; got != "deepseek-reasoner" {
		t.Fatalf("model = %q after choosing it", got)
	}

	postJSON(t, ts.URL+"/api/config", map[string]any{"aiProvider": "claude", "aiModel": "claude-opus-5-5"})
	if got := ts.store.Config().AIModel; got != "claude-opus-5-5" {
		t.Fatalf("model = %q after switching", got)
	}

	// Back to DeepSeek: not its default, the model that was working there.
	postJSON(t, ts.URL+"/api/config", map[string]any{"aiProvider": "deepseek"})
	cfg := ts.store.Config()
	if cfg.AIModel != "deepseek-reasoner" {
		t.Errorf("model = %q, want the remembered deepseek-reasoner", cfg.AIModel)
	}
	if cfg.AIModels["claude"] != "claude-opus-5-5" {
		t.Errorf("claude's model was lost: %+v", cfg.AIModels)
	}

	// A provider never used has no memory, which is what leaves it on the
	// vendor's own default.
	postJSON(t, ts.URL+"/api/config", map[string]any{"aiProvider": "gemini"})
	if got := ts.store.Config().AIModel; got != "" {
		t.Errorf("model = %q, want empty for a provider never used", got)
	}

	// A model that is typed in and then cleared puts the provider back on its
	// default rather than pinning the old name.
	postJSON(t, ts.URL+"/api/config", map[string]any{"aiModel": "gemini-2.5-pro", "aiProvider": "gemini"})
	postJSON(t, ts.URL+"/api/config", map[string]any{"aiModel": ""})
	if got := ts.store.Config().AIModels["gemini"]; got != "" {
		t.Errorf("gemini still remembers %q", got)
	}
}

func TestConfigPatchCarriesThinkingAndDeleteMode(t *testing.T) {
	ts := newToolsServer(t)

	postJSON(t, ts.URL+"/api/config", map[string]any{"aiThinking": "max", "deleteSource": int(store.DeleteAuto)})
	cfg := ts.store.Config()
	if cfg.AIThinking != "max" || cfg.DeleteSource != store.DeleteAuto {
		t.Fatalf("settings: %+v", cfg)
	}

	// A level the vendor does not document is dropped rather than stored: a
	// request carrying it would be refused, and the batch would fail on every
	// file with nothing on the page to explain why.
	postJSON(t, ts.URL+"/api/config", map[string]any{"aiThinking": "ultra"})
	if got := ts.store.Config().AIThinking; got != "max" {
		t.Errorf("thinking = %q, want the previous level", got)
	}

	postJSON(t, ts.URL+"/api/config", map[string]any{"deleteSource": 99})
	if got := ts.store.Config().DeleteSource; got != store.DeleteAuto {
		t.Errorf("deleteSource = %v, want the previous one", got)
	}

	// And it can be turned back off, which is the whole point of the setting
	// existing as a choice rather than a default.
	postJSON(t, ts.URL+"/api/config", map[string]any{"deleteSource": int(store.DeleteNever)})
	if got := ts.store.Config().DeleteSource; got != store.DeleteNever {
		t.Errorf("deleteSource = %v, want it cleared", got)
	}
}

// Starting a batch records the model that ran, including the one nobody chose
// — an empty field means the vendor's default, and that is what comes back.
func TestAIStartRemembersTheModelItUsed(t *testing.T) {
	ts := newToolsServer(t)
	if err := ts.store.Update(func(c *store.Config) { c.AITarget = "English"; c.AIProvider = "deepseek" }); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetAIKey("deepseek", "sk-x"); err != nil {
		t.Fatal(err)
	}
	path := ts.track(t, "a.flac", &tag.Tags{Title: "T", Lyrics: "[00:01.000]x\n"})

	post(t, ts.URL+"/api/ai/start", map[string]any{"paths": []string{path}})
	if got := ts.store.Config().AIModels["deepseek"]; got != "deepseek-chat" {
		t.Errorf("remembered %q, want the provider's default", got)
	}

	postJSON(t, ts.URL+"/api/config", map[string]any{"aiModel": "deepseek-reasoner"})
	post(t, ts.URL+"/api/ai/start", map[string]any{"paths": []string{path}})
	if got := ts.store.Config().AIModels["deepseek"]; got != "deepseek-reasoner" {
		t.Errorf("remembered %q, want the model that was chosen", got)
	}
}

func TestAIModelsNeedsAStoredKey(t *testing.T) {
	ts := newToolsServer(t)
	code, body := post(t, ts.URL+"/api/ai/models", map[string]any{"provider": "deepseek"})
	if code != http.StatusBadRequest || !strings.Contains(body, "deepseek") {
		t.Fatalf("status = %d body = %s", code, body)
	}

	// An unknown provider is refused before any request is made.
	code, _ = post(t, ts.URL+"/api/ai/models", map[string]any{"provider": "acme"})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown provider", code)
	}
}

// The settings view has to say which providers take the thinking controls, or
// the panel offers a level to a vendor that would reject it.
func TestAIProvidersReportWhichTakeThinking(t *testing.T) {
	ts := newToolsServer(t)
	var view aiSettingsView
	getJSON(t, ts.URL+"/api/ai/providers", &view)

	byID := map[string]bool{}
	for _, p := range view.Providers {
		byID[p.ID] = p.Thinking
	}
	if !byID["deepseek"] {
		t.Error("deepseek was not marked as taking thinking")
	}
	for _, id := range []string{"openai", "claude", "gemini"} {
		if byID[id] {
			t.Errorf("%s was marked as taking thinking", id)
		}
	}
	if len(view.ThinkingLevels) == 0 {
		t.Error("no thinking levels were offered")
	}
}
