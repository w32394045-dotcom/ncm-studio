package lyric

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fetchStub answers lyric requests and counts them. Counting is the whole
// point: "was the catalogue asked again" is exactly the difference the
// refresh setting makes, and it is not visible in the words that come back.
type fetchStub struct {
	mu    sync.Mutex
	lyric string
	n     int
}

func (s *fetchStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.n++
	body := `{"code":200,"lrc":{"lyric":"` + s.lyric + `"}}`
	s.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (s *fetchStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *fetchStub) says(lyric string) {
	s.mu.Lock()
	s.lyric = lyric
	s.mu.Unlock()
}

func fetchClient(t *testing.T, stub *fetchStub) *Client {
	t.Helper()
	c := NewClient(t.TempDir())
	c.HTTP = &http.Client{Transport: stub}
	c.MinInterval = 0
	c.SearchMinInterval = 0
	return c
}

// A lyric already on disk is what a second pass over a library reads — that is
// the setting's default and the reason a re-run costs no requests. SetRefresh
// is the one thing that changes it, and only for as long as it is on.
func TestFetchReadsTheCacheUnlessRefreshIsSet(t *testing.T) {
	stub := &fetchStub{lyric: "[00:01.000]first"}
	c := fetchClient(t, stub)
	ctx := context.Background()

	l, err := c.Fetch(ctx, 42)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(l.Original, "first") || stub.calls() != 1 {
		t.Fatalf("first fetch = %q after %d request(s)", l.Original, stub.calls())
	}

	// The catalogue now says something else. Without the setting, the stored
	// copy answers and no request is made.
	stub.says("[00:01.000]changed")
	l, err = c.Fetch(ctx, 42)
	if err != nil {
		t.Fatalf("cached Fetch: %v", err)
	}
	if !strings.Contains(l.Original, "first") {
		t.Errorf("the stored copy was not used: %q", l.Original)
	}
	if got := stub.calls(); got != 1 {
		t.Errorf("a cached fetch made %d requests in total, want 1", got)
	}

	// With it on, the catalogue is asked and its answer wins.
	c.SetRefresh(true)
	l, err = c.Fetch(ctx, 42)
	if err != nil {
		t.Fatalf("refreshing Fetch: %v", err)
	}
	if !strings.Contains(l.Original, "changed") {
		t.Errorf("refreshing returned the stored copy: %q", l.Original)
	}
	if got := stub.calls(); got != 2 {
		t.Errorf("refreshing made %d requests in total, want 2", got)
	}

	// What came back is written over the old copy, so turning the setting off
	// again costs nothing: one request per refresh, not one per file forever.
	c.SetRefresh(false)
	l, err = c.Fetch(ctx, 42)
	if err != nil {
		t.Fatalf("Fetch after refresh: %v", err)
	}
	if !strings.Contains(l.Original, "changed") {
		t.Errorf("the refreshed copy was not stored: %q", l.Original)
	}
	if got := stub.calls(); got != 2 {
		t.Errorf("a cached fetch after a refresh made %d requests in total, want 2", got)
	}
}

// The search cache answers the same question — which recordings of this song
// exist — and follows the same setting, for the same reason: a lyric that has
// changed online may have moved to a different recording.
func TestSearchRefreshIgnoresTheStoredPool(t *testing.T) {
	stub := &searchStub{body: searchBody}
	c := searchClient(t, stub)
	ctx := context.Background()

	if _, err := c.Search(ctx, "same keyword", 10); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls(); got != 1 {
		t.Fatalf("first search made %d request(s), want 1", got)
	}

	c.SetRefresh(true)
	if _, err := c.Search(ctx, "same keyword", 10); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls(); got != 2 {
		t.Errorf("a refreshing search made %d requests in total, want 2", got)
	}

	// The fresh pool replaced the stored one, so the next cached search is
	// free again.
	c.SetRefresh(false)
	if _, err := c.Search(ctx, "same keyword", 10); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls(); got != 2 {
		t.Errorf("a cached search after a refresh made %d requests in total, want 2", got)
	}
}
