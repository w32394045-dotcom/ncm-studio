package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ncm-studio/internal/backfill"
	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// catalogue stands in for NetEase so the tests never leave the device.
type catalogue struct {
	search string
	lyric  string

	mu       sync.Mutex
	searched []string
}

func (c *catalogue) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body := "{}"

	c.mu.Lock()
	switch {
	case strings.Contains(path, "search"):
		c.searched = append(c.searched, req.URL.Query().Get("s"))
		if c.search != "" {
			body = c.search
		}
	case strings.Contains(path, "lyric"):
		if c.lyric != "" {
			body = c.lyric
		}
	}
	c.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (c *catalogue) searches() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.searched...)
}

const (
	testSongName = "高野麻里佳; 石原夏織 - ひめごと_クライシスターズ(もみじver.).flac"
	testSongID   = int64(2008994719)
	testTitle    = "ひめごと*クライシスターズ(もみじver.)"
)

const testSearchReply = `{"code":200,"result":{"songs":[
 {"id":2008994719,"name":"ひめごと*クライシスターズ(もみじver.)",
  "artists":[{"name":"高野麻里佳"},{"name":"石原夏織"}],
  "album":{"name":"ひめごと*クライシスターズ"},"duration":214253}]}}`

const testLyricReply = `{"code":200,"lrc":{"lyric":"[00:01.000]きみのこえ"}}`

// lyricsServer is one test's world: a running HTTP server, the directory its
// files live in, and the stub the lyric client talks to.
type lyricsServer struct {
	*httptest.Server
	dir string
	cat *catalogue
	// pool is the backfill pool behind /api/lyrics/*.
	pool *job.Pool
}

func newLyricsServer(t *testing.T, mode store.LyricsMode) lyricsServer {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "music")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(root, "config"), store.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *store.Config) {
		c.Dir, c.Output, c.Workspace, c.Workers = dir, filepath.Join(root, "out"), root, 1
		c.Lyrics = mode
	}); err != nil {
		t.Fatal(err)
	}

	cat := &catalogue{search: testSearchReply, lyric: testLyricReply}
	client := lyric.NewClient(filepath.Join(root, "lyrics"))
	client.HTTP = &http.Client{Transport: cat}
	client.MinInterval = 0
	client.SearchMinInterval = 0

	searcher := backfill.NewSearcher()
	clients := func(string) *lyric.Client { return client }
	pool := job.New(1, backfill.Processor(backfill.Options{
		Store: st, Clients: clients, Searcher: searcher,
	}))

	conv := job.New(1, func(context.Context, *job.Item, job.Reporter) (job.Result, error) {
		return job.Result{}, nil
	})
	lrcPool := job.New(1, backfill.LRCProcessor(backfill.LRCOptions{Store: st}))
	aiPool := job.New(1, backfill.AIProcessor(backfill.AIOptions{Store: st}))
	srv := httptest.NewServer(New(st, Pools{Convert: conv, Lyrics: pool, LRC: lrcPool, AI: aiPool},
		searcher, clients, "test").Handler())
	t.Cleanup(func() {
		pool.Cancel()
		conv.Cancel()
		lrcPool.Cancel()
		aiPool.Cancel()
		srv.Close()
	})
	return lyricsServer{Server: srv, dir: dir, cat: cat, pool: pool}
}

// track writes a FLAC carrying the given tags and returns its path.
func (s lyricsServer) track(t *testing.T, name string, tags *tag.Tags) string {
	t.Helper()
	si := make([]byte, 34)
	rate, samples := 44100, uint64(9448557)
	si[10] = byte(rate >> 12)
	si[11] = byte(rate >> 4 & 0xFF)
	si[12] = byte(rate&0x0F) << 4
	si[13] = 15<<4 | byte(samples>>32&0x0F)
	si[14] = byte(samples >> 24 & 0xFF)
	si[15] = byte(samples >> 16 & 0xFF)
	si[16] = byte(samples >> 8 & 0xFF)
	si[17] = byte(samples & 0xFF)
	prefix := tag.BuildFLACMetadata(&tag.FLACSource{StreamInfo: append([]byte{0, 0, 0, 34}, si...)}, tags)
	audio := make([]byte, 4096)
	for i := range audio {
		audio[i] = byte(i)
	}
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (s lyricsServer) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func (s lyricsServer) get(t *testing.T, path string) []byte {
	t.Helper()
	resp, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s -> %d: %s", path, resp.StatusCode, out)
	}
	return out
}

// TestAudioScanReportsWhatEachFileHolds is the contract the lyrics and cover
// pages both build their rows on: one request per file, and everything the row
// needs comes back.
func TestAudioScanReportsWhatEachFileHolds(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	s.track(t, testSongName, &tag.Tags{Title: testTitle, Artists: []string{"高野麻里佳"}})
	// A file this program cannot rewrite is still listed, and says so.
	if err := os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "song.m4a"), []byte("not really"), 0o644); err != nil {
		t.Fatal(err)
	}

	var entries []audioEntry
	if err := json.Unmarshal(s.get(t, "/api/audio?dir="+s.dir), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the .txt is not audio)", len(entries))
	}

	byName := map[string]audioEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	flac := byName[testSongName]
	if !flac.Writable || flac.Format != "flac" {
		t.Errorf("flac = %+v", flac)
	}
	if flac.Title != testTitle || len(flac.Artists) != 1 {
		t.Errorf("tags did not come back: %+v", flac)
	}
	// The stream declares 9 448 557 samples at 44 100 Hz, which is 214.253 s;
	// the scan truncates to whole milliseconds.
	if flac.DurationMS != 214252 {
		t.Errorf("duration = %d ms, want 214252", flac.DurationMS)
	}
	if flac.HasLyrics {
		t.Error("a file with no lyrics reported some")
	}
	if flac.Size == 0 || flac.ModTime == "" {
		t.Errorf("stat fields missing: %+v", flac)
	}

	if m4a := byName["song.m4a"]; m4a.Writable {
		t.Errorf("an m4a was reported writable: %+v", m4a)
	}
}

// TestLyricsStartRunsABatchAndStreamsIt walks the path the UI takes: start a
// batch, then read the state it left behind.
func TestLyricsStartRunsABatchAndStreamsIt(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, &tag.Tags{Title: testTitle, Artists: []string{"高野麻里佳"}})

	code, body := s.post(t, "/api/lyrics/start", map[string]any{"paths": []string{path}})
	if code != http.StatusOK {
		t.Fatalf("start -> %d: %s", code, body)
	}

	// The batch runs on its own pool; wait for it to settle rather than
	// sleeping a fixed amount.
	waitIdle(t, s.pool)

	var state poolState
	if err := json.Unmarshal(s.get(t, "/api/lyrics/state"), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 {
		t.Fatalf("state has %d items, want 1", len(state.Items))
	}
	it := state.Items[0]
	if it.State != job.Done {
		t.Fatalf("item state = %q (%s / %s)", it.State, it.Error, it.Reason)
	}
	if it.MusicID != testSongID {
		t.Errorf("music id = %d, want %d", it.MusicID, testSongID)
	}

	// And the file really carries them now.
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasLyrics() || info.MusicID != testSongID {
		t.Errorf("after the batch: hasLyrics=%v id=%d", info.HasLyrics(), info.MusicID)
	}
}

// TestLyricsStartCarriesAPerFileOverride pins the double-click decision
// reaching the writer: the row asks for a sidecar only, so the audio file must
// be left exactly as it was.
func TestLyricsStartCarriesAPerFileOverride(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, &tag.Tags{Title: testTitle, Artists: []string{"高野麻里佳"}})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	code, body := s.post(t, "/api/lyrics/start", map[string]any{
		"paths": []string{path},
		"modes": map[string]int{path: int(store.LyricsFileOnly)},
	})
	if code != http.StatusOK {
		t.Fatalf("start -> %d: %s", code, body)
	}
	waitIdle(t, s.pool)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a file-only override rewrote the audio file")
	}
	if _, err := os.Stat(filepath.Join(s.dir, strings.TrimSuffix(testSongName, ".flac")+".lrc")); err != nil {
		t.Errorf("no sidecar was written: %v", err)
	}
}

// TestLyricsApplyWritesWhatTheUserPicked is the manual half: the user chose a
// candidate, and that choice has to reach the file and the list.
func TestLyricsApplyWritesWhatTheUserPicked(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, &tag.Tags{Title: testTitle, Artists: []string{"高野麻里佳"}})

	code, body := s.post(t, "/api/lyrics/apply", map[string]any{"path": path, "musicId": testSongID})
	if code != http.StatusOK {
		t.Fatalf("apply -> %d: %s", code, body)
	}

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.MusicID != testSongID || !info.HasLyrics() {
		t.Errorf("after apply: id=%d hasLyrics=%v", info.MusicID, info.HasLyrics())
	}
}

// TestLyricsApplyNamesAFileFromTheCandidatesOwnSpelling: a file with no title
// and no artist is one a player will not show lyrics for, and the moment the
// user confirms a candidate is the moment its spelling of the name is known.
// It fills only what the file leaves empty.
func TestLyricsApplyNamesAFileFromTheCandidatesOwnSpelling(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, &tag.Tags{MusicID: testSongID})

	code, body := s.post(t, "/api/lyrics/apply", map[string]any{
		"path": path, "musicId": testSongID,
		"name": testTitle, "artists": []string{"高野麻里佳", "石原夏織"}, "album": "ひめごと*クライシスターズ",
	})
	if code != http.StatusOK {
		t.Fatalf("apply -> %d: %s", code, body)
	}

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != testTitle {
		t.Errorf("title = %q, want the candidate's %q", info.Title, testTitle)
	}
	if len(info.Artists) != 2 {
		t.Errorf("artists = %v, want both", info.Artists)
	}
	if info.Album != "ひめごと*クライシスターズ" {
		t.Errorf("album = %q", info.Album)
	}

	// A page cannot rename a file that names itself, however sure it is: the
	// tags in the file are the user's own.
	own := s.track(t, testSongName, &tag.Tags{Title: "My Own Spelling", Artists: []string{"Nobody"}})
	code, body = s.post(t, "/api/lyrics/apply", map[string]any{
		"path": own, "musicId": testSongID, "name": testTitle, "artists": []string{"高野麻里佳"},
	})
	if code != http.StatusOK {
		t.Fatalf("apply -> %d: %s", code, body)
	}
	kept, err := tag.Inspect(own)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Title != "My Own Spelling" {
		t.Errorf("title = %q — the request overwrote the file's own", kept.Title)
	}
	if len(kept.Artists) != 1 || kept.Artists[0] != "Nobody" {
		t.Errorf("artists = %v — the request overwrote the file's own", kept.Artists)
	}
}

// TestLyricsMatchRanksWithoutWriting: searching for a file must never be enough
// to change it. Only a person confirming does that.
func TestLyricsMatchRanksWithoutWriting(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, &tag.Tags{Title: testTitle, Artists: []string{"高野麻里佳"}})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	code, body := s.post(t, "/api/lyrics/match", map[string]any{"path": path})
	if code != http.StatusOK {
		t.Fatalf("match -> %d: %s", code, body)
	}
	var sug backfill.Suggestion
	if err := json.Unmarshal(body, &sug); err != nil {
		t.Fatal(err)
	}
	if !sug.Found || sug.Best != testSongID {
		t.Errorf("suggestion = %+v", sug)
	}
	if len(sug.Candidates) == 0 {
		t.Error("no candidates were offered")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("merely searching rewrote the file")
	}
}

// TestLyricsMatchSharesTheBatchesSearch is the reason the searcher is shared:
// the search endpoint refuses a client after a handful of requests, so asking
// about a song the batch has already resolved must not cost another one.
func TestLyricsMatchSharesTheBatchesSearch(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	path := s.track(t, testSongName, &tag.Tags{Title: testTitle, Artists: []string{"高野麻里佳"}})

	if code, body := s.post(t, "/api/lyrics/start", map[string]any{"paths": []string{path}}); code != http.StatusOK {
		t.Fatalf("start -> %d: %s", code, body)
	}
	waitIdle(t, s.pool)
	afterBatch := len(s.cat.searches())

	if code, body := s.post(t, "/api/lyrics/match", map[string]any{"path": path}); code != http.StatusOK {
		t.Fatalf("match -> %d: %s", code, body)
	}
	if got := len(s.cat.searches()); got != afterBatch {
		t.Errorf("a manual search re-asked for a song the batch had resolved: %d -> %d requests", afterBatch, got)
	}
}

// TestLyricsStartRejectsAnEmptySelection keeps a stray request from cancelling
// whatever is already running: Pool.Start cancels first, so an empty batch must
// not reach it.
func TestLyricsStartRejectsAnEmptySelection(t *testing.T) {
	s := newLyricsServer(t, store.LyricsEmbed)
	code, _ := s.post(t, "/api/lyrics/start", map[string]any{"paths": []string{}})
	if code != http.StatusBadRequest {
		t.Errorf("an empty selection -> %d, want 400", code)
	}
}

func waitIdle(t *testing.T, p *job.Pool) {
	t.Helper()
	done := make(chan struct{})
	p.OnIdle(func() { close(done) })
	if !p.Running() {
		// The batch may already have finished, in which case no idle hook is
		// coming; the pool's own state is the answer.
		return
	}
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("the batch never finished")
	}
}
