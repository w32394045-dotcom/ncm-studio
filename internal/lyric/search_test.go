package lyric

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// searchStub answers search requests without a network and counts them, which
// is what the pacing and caching assertions need.
type searchStub struct {
	body   string
	status int

	mu    sync.Mutex
	asked []string
	n     int
}

func (s *searchStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.asked = append(s.asked, req.URL.String())
	s.n++
	s.mu.Unlock()

	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

func (s *searchStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *searchStub) lastQuery() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.asked) == 0 {
		return ""
	}
	return s.asked[len(s.asked)-1]
}

// searchBody is the shape the endpoint actually returns, trimmed to the fields
// that are read. Note the ordering: the base recording comes back ahead of the
// versioned one, which is the detail the matcher has to survive.
const searchBody = `{
  "code": 200,
  "result": {
    "songCount": 3,
    "songs": [
      {"id": 2008993850, "name": "ひめごと*クライシスターズ",
       "artists": [{"name":"高野麻里佳"},{"name":"石原夏織"},{"name":"金元寿子"},{"name":"津田美波"}],
       "album": {"name":"ひめごと*クライシスターズ"}, "duration": 214253},
      {"id": 2008994719, "name": "ひめごと*クライシスターズ(もみじver.)",
       "artists": [{"name":"高野麻里佳"},{"name":"石原夏織"},{"name":"金元寿子"},{"name":"津田美波"}],
       "album": {"name":"ひめごと*クライシスターズ"}, "duration": 214253},
      {"id": 0, "name": "no id, must be dropped", "artists": [], "album": {}, "duration": 0}
    ]
  }
}`

func searchClient(t *testing.T, stub *searchStub) *Client {
	t.Helper()
	c := NewClient(t.TempDir())
	c.HTTP = &http.Client{Transport: stub}
	// Both floors off by default so a test only pays for the pacing it is
	// actually about.
	c.MinInterval = 0
	c.SearchMinInterval = 0
	return c
}

func TestSearchParsesCandidates(t *testing.T) {
	stub := &searchStub{body: searchBody}
	c := searchClient(t, stub)

	cands, err := c.Search(context.Background(), "ひめごと クライシスターズ", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2 (the id-less entry must be dropped)", len(cands))
	}
	if cands[0].MusicID != 2008993850 {
		t.Errorf("first id = %d", cands[0].MusicID)
	}
	if len(cands[1].Artists) != 4 {
		t.Errorf("artists = %v, want four", cands[1].Artists)
	}
	if got := cands[1].Duration(); got != 214253*time.Millisecond {
		t.Errorf("duration = %v", got)
	}
	if cands[1].Album != "ひめごと*クライシスターズ" {
		t.Errorf("album = %q", cands[1].Album)
	}

	// The keyword has to reach the endpoint as the caller wrote it.
	if q := stub.lastQuery(); !strings.Contains(q, "type=1") || !strings.Contains(q, "s=") {
		t.Errorf("query = %q", q)
	}
}

// TestSearchIsCached pins that a second identical search costs nothing: a
// backfill re-run over a directory that partly matched must not re-ask for
// every keyword.
func TestSearchIsCached(t *testing.T) {
	stub := &searchStub{body: searchBody}
	c := searchClient(t, stub)
	ctx := context.Background()

	if _, err := c.Search(ctx, "same keyword", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(ctx, "same keyword", 10); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls(); got != 1 {
		t.Errorf("made %d requests for one repeated keyword, want 1", got)
	}

	// NoCache must bypass the cache entirely.
	c.NoCache = true
	if _, err := c.Search(ctx, "same keyword", 10); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls(); got != 2 {
		t.Errorf("NoCache made %d requests in total, want 2", got)
	}
}

// TestSearchCachesNegativeResults covers the case that makes a re-run cheap for
// a library full of tracks the catalogue does not have.
func TestSearchCachesNegativeResults(t *testing.T) {
	stub := &searchStub{body: `{"code":200,"result":{"songCount":0,"songs":[]}}`}
	c := searchClient(t, stub)
	ctx := context.Background()

	cands, err := c.Search(ctx, "nothing matches this", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("got %d candidates, want none", len(cands))
	}
	if _, err := c.Search(ctx, "nothing matches this", 10); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls(); got != 1 {
		t.Errorf("an empty result was not cached: %d requests", got)
	}
}

// TestSearchUnavailableIsDistinct checks that a login wall or an API change is
// reported as such rather than as "no results" — the UI has to be able to tell
// the user to type an id instead of silently matching nothing.
func TestSearchUnavailableIsDistinct(t *testing.T) {
	cases := map[string]*searchStub{
		"html login wall": {body: "<html><body>please log in</body></html>"},
		"api error code":  {body: `{"code":-460,"message":"cheating"}`},
		"http failure":    {body: "", status: http.StatusForbidden},
	}
	for name, stub := range cases {
		t.Run(name, func(t *testing.T) {
			c := searchClient(t, stub)
			_, err := c.Search(context.Background(), "anything", 10)
			if !errors.Is(err, ErrSearchUnavailable) {
				t.Fatalf("error = %v, want ErrSearchUnavailable", err)
			}
		})
	}

	// A failure must not be cached as an answer. The cooldown is cleared
	// between the two attempts so this measures the cache and not the pause.
	stub := &searchStub{body: "<html>log in</html>"}
	c := searchClient(t, stub)
	ctx := context.Background()
	for range 2 {
		if _, err := c.Search(ctx, "retry me", 10); !errors.Is(err, ErrSearchUnavailable) {
			t.Fatalf("error = %v, want ErrSearchUnavailable", err)
		}
		c.searchBlockedUntil = time.Time{}
	}
	if got := stub.calls(); got != 2 {
		t.Errorf("a failed search was cached: %d requests for two attempts, want 2", got)
	}
}

// TestSearchSharesThePacingBudget is the reason pace() exists: a backfill
// searches and then fetches for each file, and pacing the two separately would
// double the rate the server sees.
func TestSearchSharesThePacingBudget(t *testing.T) {
	stub := &searchStub{body: searchBody}
	c := searchClient(t, stub)
	c.MinInterval = 120 * time.Millisecond

	start := time.Now()
	if _, err := c.Search(context.Background(), "first", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(context.Background(), "second", 10); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < c.MinInterval {
		t.Errorf("two requests took %v, less than the %v floor", elapsed, c.MinInterval)
	}
}

// TestSearchCanBePacedHarderThanFetches covers the reason SearchMinInterval
// exists: the search endpoint refuses a rate the lyric endpoint accepts.
func TestSearchCanBePacedHarderThanFetches(t *testing.T) {
	stub := &searchStub{body: searchBody}
	c := searchClient(t, stub)
	c.MinInterval = time.Millisecond
	c.SearchMinInterval = 150 * time.Millisecond

	start := time.Now()
	if _, err := c.Search(context.Background(), "first", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(context.Background(), "second", 10); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < c.SearchMinInterval {
		t.Errorf("two searches took %v, less than the %v search floor", elapsed, c.SearchMinInterval)
	}

	// A fresh client must ship with the search floor already in place; a zero
	// here would mean the live 405 returns the moment a library has no ids.
	if d := NewClient(t.TempDir()).SearchMinInterval; d < defaultMinInterval {
		t.Errorf("NewClient search floor = %v, want at least %v", d, defaultMinInterval)
	}
}

func TestSearchEmptyKeywordMakesNoRequest(t *testing.T) {
	stub := &searchStub{body: searchBody}
	c := searchClient(t, stub)
	cands, err := c.Search(context.Background(), "   ", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("got %d candidates for a blank keyword", len(cands))
	}
	if got := stub.calls(); got != 0 {
		t.Errorf("a blank keyword made %d requests, want 0", got)
	}
}

// TestSearchBacksOffInsteadOfRetryingThrottling is the behaviour the live
// endpoint forced. Four searches in a burst earn a 405 that refuses even a
// repeat of a request that just succeeded, and asking again through it was
// measured to hold the block open — so a refusal has to stop the client rather
// than be retried.
func TestSearchBacksOffInsteadOfRetryingThrottling(t *testing.T) {
	stub := &searchStub{body: `{"msg":"操作频繁，请稍候再试","code":405}`}
	c := searchClient(t, stub)
	c.SearchCooldown = time.Minute

	_, err := c.Search(context.Background(), "first", 10)
	if !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("error = %v, want ErrSearchUnavailable", err)
	}
	if got := stub.calls(); got != 1 {
		t.Fatalf("a refusal was retried %d times, want exactly 1 attempt", got)
	}
	if !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("error = %v, want it to read as a rate limit", err)
	}

	// A different keyword must not go near the endpoint while the cooldown
	// lasts — that is the whole point of backing off.
	if _, err := c.Search(context.Background(), "second", 10); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("error = %v, want ErrSearchUnavailable", err)
	}
	if got := stub.calls(); got != 1 {
		t.Errorf("the client searched again during its cooldown: %d requests", got)
	}

	// A keyword already answered from cache still works: a resolved song must
	// never cost a request again, cooldown or not.
	c.saveSearchCache("known", []Candidate{{MusicID: 7, Name: "cached"}})
	got, err := c.Search(context.Background(), "known", 10)
	if err != nil {
		t.Fatalf("a cached keyword failed during a cooldown: %v", err)
	}
	if len(got) != 1 || got[0].MusicID != 7 {
		t.Errorf("cached result = %v", got)
	}
	if n := stub.calls(); n != 1 {
		t.Errorf("serving from cache made a request: %d", n)
	}
}

// TestSearchCooldownEscalatesAndResets covers the size of the pause: the window
// the endpoint enforces is undocumented and grows when it is tested, so the
// wait doubles on each refusal and a success forgives it.
func TestSearchCooldownEscalatesAndResets(t *testing.T) {
	stub := &searchStub{body: `{"code":406}`}
	c := searchClient(t, stub)
	c.SearchCooldown = 100 * time.Millisecond
	c.MaxSearchCooldown = 400 * time.Millisecond

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}
	for i, w := range want {
		if _, err := c.Search(context.Background(), fmt.Sprintf("k%d", i), 10); !errors.Is(err, ErrSearchUnavailable) {
			t.Fatalf("attempt %d: error = %v", i, err)
		}
		if c.searchPenalty != w {
			t.Fatalf("after %d refusals the pause is %v, want %v", i+1, c.searchPenalty, w)
		}
		// Let the pause elapse before the next attempt. A refusal only doubles
		// the wait when it follows a request; a call the cooldown turned away
		// never reached the endpoint and has nothing to escalate for.
		c.searchBlockedUntil = time.Time{}
	}

	// Once the endpoint answers again, the next refusal starts from the bottom.
	stub.body = searchBody
	c.searchBlockedUntil = time.Time{}
	if _, err := c.Search(context.Background(), "works", 10); err != nil {
		t.Fatalf("a healthy search failed: %v", err)
	}
	if c.searchPenalty != 0 {
		t.Errorf("a success left a %v penalty behind", c.searchPenalty)
	}
}

// TestSearchResumesWhenTheCooldownExpires pins that the pause ends by itself.
func TestSearchResumesWhenTheCooldownExpires(t *testing.T) {
	stub := &searchStub{body: `{"code":405}`}
	c := searchClient(t, stub)
	c.SearchCooldown = 20 * time.Millisecond

	if _, err := c.Search(context.Background(), "blocked", 10); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("error = %v", err)
	}
	stub.body = searchBody
	time.Sleep(30 * time.Millisecond)

	cands, err := c.Search(context.Background(), "blocked", 10)
	if err != nil {
		t.Fatalf("Search after the cooldown: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("got %d candidates, want 2", len(cands))
	}
}

// TestSearchUnavailableIsDistinct covers the other refusal: a login page or a
// changed API is not something waiting will fix, so it must not read as one.
func TestSearchWallIsNotRetriedOrCalledRateLimited(t *testing.T) {
	stub := &searchStub{body: "<html><body>captcha</body></html>"}
	c := searchClient(t, stub)
	if _, err := c.Search(context.Background(), "walled", 10); !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("error = %v, want ErrSearchUnavailable", err)
	}
	if got := stub.calls(); got != 1 {
		t.Errorf("a login wall was tried %d times, want 1", got)
	}
}
