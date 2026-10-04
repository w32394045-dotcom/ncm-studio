package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"ncm-studio/internal/backfill"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"ncm-studio/internal/job"
	"ncm-studio/internal/store"
)

// newTestServer stands up the real handler chain behind a real HTTP server.
//
// A recorder is not enough here: httptest.ResponseRecorder implements
// http.Flusher itself, so it would happily satisfy the event stream even when
// a middleware wrapper has hidden the underlying writer's Flush and broken
// streaming for every actual client.
func newTestServer(t *testing.T, proc job.Processor) (*httptest.Server, *store.Store, *job.Pool) {
	t.Helper()

	root := t.TempDir()
	src := filepath.Join(root, "src")
	out := filepath.Join(root, "out")
	for _, d := range []string{src, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(filepath.Join(root, "cfg"), store.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	// The workspace is pinned inside the temp tree: DefaultWorkspace prefers
	// the directory beside the executable, which for a test binary would be
	// somewhere shared rather than this test's own.
	if err := st.Update(func(c *store.Config) {
		c.Dir, c.Output, c.Workspace, c.Workers = src, out, root, 1
	}); err != nil {
		t.Fatal(err)
	}

	if proc == nil {
		proc = func(ctx context.Context, it *job.Item, report job.Reporter) (job.Result, error) {
			return job.Result{Output: filepath.Join(out, it.Name)}, nil
		}
	}
	pool := job.New(1, proc)

	srv := httptest.NewServer(newServerWith(st, pool, nil).Handler())
	t.Cleanup(func() {
		pool.Cancel()
		srv.Close()
	})
	return srv, st, pool
}

// TestEventStreamReachesClient is the regression test for the Flusher that
// logRequests used to swallow: without it /api/events answered 500 with
// "streaming is not supported" and the UI showed live progress for nothing.
func TestEventStreamReachesClient(t *testing.T) {
	srv, _, pool := newTestServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("event stream refused: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}

	// An event emitted after the client connected must actually arrive, which
	// only happens if each event is flushed rather than buffered.
	pool.Start([]*job.Item{{ID: "a", Path: "/a.ncm", Name: "a.ncm"}})

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("event stream closed before delivering an event")
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var evt job.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
				t.Fatalf("bad event payload %q: %v", line, err)
			}
			if evt.Kind == "batch" && evt.Run {
				return // the batch-start event made it through
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for an event")
		}
	}
}

func TestIndexIsServedNotAPlaceholder(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{"<title>NCM Studio</title>", `id="fileList"`, `id="browseList"`, "/api/events"} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}

// TestTranslationsCoverEveryKey keeps the five dictionaries from drifting: a
// key present in one language but missing in another renders as a raw
// identifier in the UI.
func TestTranslationsCoverEveryKey(t *testing.T) {
	data, err := staticFS.ReadFile("static/i18n.json")
	if err != nil {
		t.Fatal(err)
	}
	var dict map[string]map[string]string
	if err := json.Unmarshal(data, &dict); err != nil {
		t.Fatalf("i18n.json is not valid JSON: %v", err)
	}

	const base = "zh-CN"
	if len(dict[base]) == 0 {
		t.Fatalf("no %s dictionary", base)
	}
	for lang, table := range dict {
		if lang == base {
			continue
		}
		for key := range dict[base] {
			if _, ok := table[key]; !ok {
				t.Errorf("%s is missing key %q", lang, key)
			}
		}
		for key := range table {
			if _, ok := dict[base][key]; !ok {
				t.Errorf("%s has extra key %q", lang, key)
			}
		}
	}
}

// TestEveryKeyTheUIRefersToExists catches the failure the parity test cannot:
// a key that no dictionary has. t() falls back to the key itself, so a typo
// renders as the literal text "settings.workersHint" in the middle of the UI
// and nothing else complains.
func TestEveryKeyTheUIRefersToExists(t *testing.T) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	data, err := staticFS.ReadFile("static/i18n.json")
	if err != nil {
		t.Fatal(err)
	}
	var dict map[string]map[string]string
	if err := json.Unmarshal(data, &dict); err != nil {
		t.Fatal(err)
	}
	have := dict["zh-CN"]

	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-i18n="([^"]+)"`).FindAllStringSubmatch(string(page), -1) {
		keys[m[1]] = true
	}
	// Literal t("a.b") calls. Keys built by concatenation are covered by the
	// families below, since no regex can know which suffix a variable holds.
	for _, m := range regexp.MustCompile(`t\("([a-zA-Z]+\.[a-zA-Z]+)"`).FindAllStringSubmatch(string(page), -1) {
		keys[m[1]] = true
	}
	// data-i18n-attr is written "attr:key,attr:key", and a key misspelled there
	// leaves an attribute empty rather than printing the key, which is the one
	// failure mode the rest of this test cannot see.
	for _, m := range regexp.MustCompile(`data-i18n-attr="([^"]+)"`).FindAllStringSubmatch(string(page), -1) {
		for _, pair := range strings.Split(m[1], ",") {
			if _, key, ok := strings.Cut(strings.TrimSpace(pair), ":"); ok {
				keys[strings.TrimSpace(key)] = true
			}
		}
	}

	// Every value the dynamic keys can take, taken from the code that sets them:
	// item states, pipeline stages, lyric outcomes, connection states, the
	// browse root names in server.go, and the folder cards in index.html.
	for _, family := range []struct {
		prefix string
		values []string
	}{
		{"state.", []string{"pending", "running", "done", "skipped", "failed", "exists", "review"}},
		// The matcher's refusal reasons, which the page turns into "reason." +
		// the suffix it looked up; backfill and lyric hold the other end.
		{"reason.", []string{
			"nothing", "noCandidate", "instrumental", "suffix", "partialTitle",
			"artists", "unsure", "tie", "off", "noLyrics",
			// The .lrc directions and the translation batch refuse for reasons
			// of their own; backfill, ai and web/ai.go hold the other end.
			"noLyricsOut", "lrcExists", "noLrc", "lrcNoLines", "lyricsKept",
			"lyricsOff", "noTarget", "nothingToTranslate", "alreadyTranslated", "noKey",
		}},
		{"stage.", []string{"start", "decrypt", "lyrics", "tag", "done"}},
		{"lyrics.", []string{"ok", "none", "failed"}},
		// What a confirmed match actually wrote, chosen after the write from what
		// the server reported rather than from what was asked for.
		{"lyrics.applied", []string{"Embed", "File", "Both", "Current"}},
		{"status.", []string{"connecting", "online", "offline", "reconnecting"}},
		{"files.root.", []string{"home", "storage", "sdcard", "downloads", "music", "root"}},
		// The DIR_CARDS table builds these as "dirs." + key.
		{"dirs.", []string{"scan", "output", "workspace"}},
		// The landing-mode chips and the thinking control keep their keys in a
		// JS array and a map, indexed by a mode number or a level name — no
		// pattern above reaches either, and a typo in one is a blank chip where
		// the four choices should be.
		{"mode.", []string{"music", "musicLyrics", "musicLyricsLrc", "musicLrc"}},
		{"ai.think", []string{"Default", "Off", "Low", "High", "Max"}},
	} {
		for _, v := range family.values {
			keys[family.prefix+v] = true
		}
	}

	if len(keys) < 60 {
		t.Fatalf("only %d keys collected; the page or these patterns changed", len(keys))
	}
	for key := range keys {
		if _, ok := have[key]; !ok {
			t.Errorf("index.html refers to %q, which zh-CN does not define", key)
		}
	}
}

func TestStateReportsSettings(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)

	var got stateResponse
	getJSON(t, srv.URL+"/api/state", &got)

	cfg := st.Config()
	if got.Config.Dir != cfg.Dir || got.Config.Output != cfg.Output {
		t.Errorf("state config = %+v, want dir %q output %q", got.Config, cfg.Dir, cfg.Output)
	}
	if got.Version != "test" {
		t.Errorf("version = %q, want test", got.Version)
	}
	if got.ConfigDir != st.Dir() {
		t.Errorf("configDir = %q, want %q", got.ConfigDir, st.Dir())
	}
}

func TestConfigPatchPersists(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)

	resp, err := http.Post(srv.URL+"/api/config", "application/json",
		strings.NewReader(`{"workers":5,"lyrics":0,"lang":"ja"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cfg := st.Config()
	if cfg.Workers != 5 || cfg.Lyrics != store.LyricsOff || cfg.Lang != "ja" {
		t.Errorf("config = %+v, want workers 5, lyrics off, lang ja", cfg)
	}
}

func TestFilesFindsNCMAndSkipsEverythingElse(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)
	cfg := st.Config()

	write(t, filepath.Join(cfg.Dir, "a.ncm"), "one")
	write(t, filepath.Join(cfg.Dir, "b.NCM"), "two")
	write(t, filepath.Join(cfg.Dir, "notes.txt"), "three")
	sub := filepath.Join(cfg.Dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "c.ncm"), "four")

	var flat []fileEntry
	getJSON(t, srv.URL+"/api/files", &flat)
	if len(flat) != 2 {
		t.Fatalf("non-recursive scan found %d files, want 2: %+v", len(flat), flat)
	}

	var deep []fileEntry
	getJSON(t, srv.URL+"/api/files?recursive=1", &deep)
	if len(deep) != 3 {
		t.Fatalf("recursive scan found %d files, want 3", len(deep))
	}
	for _, f := range deep {
		if f.State != "new" {
			t.Errorf("%s state = %q, want new", f.Name, f.State)
		}
		if f.Size == 0 {
			t.Errorf("%s has no size", f.Name)
		}
	}
}

func TestBrowseMkdirRenameDelete(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)
	cfg := st.Config()

	write(t, filepath.Join(cfg.Output, "song.flac"), "audio")

	var listing struct {
		Path    string     `json:"path"`
		Parent  string     `json:"parent"`
		Entries []dirEntry `json:"entries"`
		Roots   []map[string]string
	}
	getJSON(t, srv.URL+"/api/browse?path="+cfg.Output, &listing)
	if listing.Path != cfg.Output {
		t.Errorf("path = %q, want %q", listing.Path, cfg.Output)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "song.flac" {
		t.Fatalf("entries = %+v, want just song.flac", listing.Entries)
	}
	if len(listing.Roots) == 0 {
		t.Error("no browse roots offered")
	}

	newDir := filepath.Join(cfg.Output, "album")
	postJSON(t, srv.URL+"/api/mkdir", map[string]string{"path": newDir})
	if fi, err := os.Stat(newDir); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir did not create %s: %v", newDir, err)
	}

	postJSON(t, srv.URL+"/api/rename", map[string]string{
		"from": filepath.Join(cfg.Output, "song.flac"),
		"to":   filepath.Join(cfg.Output, "renamed.flac"),
	})
	if _, err := os.Stat(filepath.Join(cfg.Output, "renamed.flac")); err != nil {
		t.Fatalf("rename did not take effect: %v", err)
	}

	// Renaming onto an existing file must be refused, not silently clobbered.
	write(t, filepath.Join(cfg.Output, "taken.flac"), "x")
	resp, err := http.Post(srv.URL+"/api/rename", "application/json",
		strings.NewReader(`{"from":"`+filepath.Join(cfg.Output, "renamed.flac")+`","to":"`+filepath.Join(cfg.Output, "taken.flac")+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("rename over an existing file: status = %d, want 409", resp.StatusCode)
	}

	postJSON(t, srv.URL+"/api/delete", map[string]any{"paths": []string{filepath.Join(cfg.Output, "renamed.flac")}})
	if _, err := os.Stat(filepath.Join(cfg.Output, "renamed.flac")); !os.IsNotExist(err) {
		t.Error("delete did not remove the file")
	}
}

func TestUploadAcceptsNCMOnly(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)
	cfg := st.Config()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, body := range map[string]string{"keep.ncm": "audio", "drop.txt": "text"} {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	// Snapshot now: bytes.Buffer.Bytes returns only the unread remainder, so
	// re-reading buf after the first POST would send an empty body.
	body := append([]byte(nil), buf.Bytes()...)

	resp, err := http.Post(srv.URL+"/api/upload", mw.FormDataContentType(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}

	if _, err := os.Stat(filepath.Join(cfg.Dir, "keep.ncm")); err != nil {
		t.Error(".ncm upload was not saved")
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, "drop.txt")); !os.IsNotExist(err) {
		t.Error("a non-.ncm upload was saved")
	}

	// A second upload of the same name must not overwrite the first.
	resp2, err := http.Post(srv.URL+"/api/upload", mw.FormDataContentType(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if _, err := os.Stat(filepath.Join(cfg.Dir, "keep (1).ncm")); err != nil {
		t.Error("a duplicate upload did not get a unique name")
	}
}

func TestStartRejectsEmptySelection(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	resp, err := http.Post(srv.URL+"/api/start", "application/json", strings.NewReader(`{"paths":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	// A slice passed in more than once has to be cleared first: encoding/json
	// appends into whatever capacity it is given, so a second fetch of the same
	// listing would decode the new rows onto the stale ones and every later
	// assertion would be reading the first response.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && !rv.IsNil() {
		if el := rv.Elem(); el.Kind() == reflect.Slice && !el.IsNil() {
			el.Set(reflect.MakeSlice(el.Type(), 0, el.Cap()))
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

func postJSON(t *testing.T, url string, payload any) {
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
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("POST %s: %d %s", url, resp.StatusCode, msg)
	}
}

// TestWorkersSettingIsClampedAndPersisted: the stored value has to be one the
// pool will actually run. A config that says 20 while the pool runs 16 leaves
// the UI showing a setting it cannot represent, and the next start disagrees
// with the last one.
func TestWorkersSettingIsClampedAndPersisted(t *testing.T) {
	srv, st, pool := newTestServer(t, nil)

	for _, tc := range []struct{ sent, want int }{
		{sent: 0, want: job.MinWorkers},
		{sent: -3, want: job.MinWorkers},
		{sent: 4, want: 4},
		{sent: 99, want: job.MaxWorkers},
	} {
		postJSON(t, srv.URL+"/api/config", map[string]int{"workers": tc.sent})
		if got := st.Config().Workers; got != tc.want {
			t.Errorf("stored workers for %d = %d, want %d", tc.sent, got, tc.want)
		}
		if got := pool.Workers(); got != tc.want {
			t.Errorf("pool workers for %d = %d, want %d", tc.sent, got, tc.want)
		}
	}
}

// newTestServer builds a server for tests that are not about lyrics. The
// backfill pool exists so the handler can be routed, and does nothing if a test
// starts it; clients may be nil for the same reason.
func newServerWith(st *store.Store, pool *job.Pool, clients backfill.ClientFor) *Server {
	idle := func(context.Context, *job.Item, job.Reporter) (job.Result, error) {
		return job.Result{}, nil
	}
	return New(st, Pools{
		Convert: pool,
		Lyrics:  job.New(1, idle),
		LRC:     job.New(1, idle),
		AI:      job.New(1, idle),
	}, backfill.NewSearcher(), clients, "test")
}

// TestCrossSiteRequestIsRefused: the API answers the page this program serves,
// and a POST from anywhere else is not that page. A body of JSON sent as
// text/plain reaches a handler without a preflight, so nothing in the request
// itself marks it as foreign but these headers — which makes them the thing to
// check, and makes this the test that keeps a drive-by page from reaching
// /api/delete.
func TestCrossSiteRequestIsRefused(t *testing.T) {
	srv, st, _ := newTestServer(t, nil)

	send := func(t *testing.T, headers map[string]string, path string) int {
		t.Helper()
		body := bytes.NewReader([]byte(`{"paths":["` + path + `"]}`))
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/delete", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/plain")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	foreign := []map[string]string{
		{"Origin": "https://example.com"},
		{"Origin": "http://127.0.0.1:9"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
	}
	for i, h := range foreign {
		path := filepath.Join(st.Config().Dir, "keep-"+strconv.Itoa(i)+".ncm")
		write(t, path, "x")
		if code := send(t, h, path); code != http.StatusForbidden {
			t.Errorf("POST /api/delete with %v = %d, want 403", h, code)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("POST /api/delete with %v deleted the file anyway: %v", h, err)
		}
	}

	// The page's own request still goes through, whichever way it was reached:
	// the LAN address a phone uses, and a client that sends no such headers at
	// all — curl, or the user's own script.
	own := filepath.Join(st.Config().Dir, "own.ncm")
	write(t, own, "x")
	if code := send(t, map[string]string{"Origin": srv.URL, "Sec-Fetch-Site": "same-origin"}, own); code != http.StatusOK {
		t.Errorf("POST /api/delete from the page itself = %d, want 200", code)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("the page's own delete did not remove the file: %v", err)
	}
	plain := filepath.Join(st.Config().Dir, "plain.ncm")
	write(t, plain, "x")
	if code := send(t, nil, plain); code != http.StatusOK {
		t.Errorf("POST /api/delete with no headers = %d, want 200", code)
	}
}
