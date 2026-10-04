package lyric

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultSearchEndpoint is the web player's own search call. It is
// undocumented and needs no login, which is exactly why it is isolated behind a
// field: it can start requiring one at any time, and that has to be a change to
// this file rather than a rewrite of everything that matches tracks.
const defaultSearchEndpoint = "https://music.163.com/api/search/get/web"

// searchCacheTTL is how long a keyword's results are reused. Song ids are
// stable, so the only thing that ages out is a track being added to the
// catalogue under a keyword somebody already searched.
const searchCacheTTL = 30 * 24 * time.Hour

// ErrSearchUnavailable reports that the search endpoint answered with
// something other than a song list — a login wall, a rate limit, or an API
// that has changed shape. It is deliberately distinct from "searched, nothing
// matched": the first is worth retrying or working around by typing an id, the
// second is an answer.
var ErrSearchUnavailable = errors.New("lyric: search is unavailable")

// searchRefusal is a search failure together with whether asking again could
// plausibly work.
//
// The endpoint refuses in two different ways and they mean opposite things. A
// well-formed JSON error code — 406 is the one measured on this device — is
// throttling: it arrived after a run of rapid searches, and the same request
// succeeds again a moment later. A login page or a captcha served in place of
// JSON is a decision about the caller, and repeating it only wastes time.
type searchRefusal struct {
	err       error
	retryable bool
}

// Error leads with the sentinel's own words so that whatever prints it — the
// UI's error line — names the thing that failed rather than only the shape of
// the failure. The two kinds of refusal are spelled differently because they
// call for different things from the reader: a rate limit is worth re-running
// in a few minutes, a wall is not worth re-running at all.
func (e *searchRefusal) Error() string {
	what := ErrSearchUnavailable.Error()
	if e.retryable {
		what = "lyric: search is rate-limited"
	}
	return what + ": " + e.err.Error()
}

// Unwrap reports the refusal as the one sentinel a caller has to know about;
// only the retry decision needs the finer distinction.
func (e *searchRefusal) Unwrap() error { return ErrSearchUnavailable }

func refuse(retryable bool, format string, args ...any) error {
	return &searchRefusal{err: fmt.Errorf(format, args...), retryable: retryable}
}

// Candidate is one song the search returned.
type Candidate struct {
	MusicID    int64    `json:"musicId"`
	Name       string   `json:"name"`
	Artists    []string `json:"artists,omitempty"`
	Album      string   `json:"album,omitempty"`
	DurationMS int64    `json:"durationMs,omitempty"`
}

// Duration is the track length the search reported, or zero when it did not.
func (c Candidate) Duration() time.Duration {
	return time.Duration(c.DurationMS) * time.Millisecond
}

type searchResponse struct {
	Code   int `json:"code"`
	Result struct {
		SongCount int `json:"songCount"`
		Songs     []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Artists []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name string `json:"name"`
			} `json:"album"`
			Duration int64 `json:"duration"`
		} `json:"songs"`
	} `json:"result"`
}

// Search looks up songs by keyword.
//
// Results are cached per keyword, negative results included, so re-running a
// backfill over a directory that did not match costs a file read rather than a
// request. A keyword the endpoint could not answer is not cached: that is a
// failure, not an answer.
func (c *Client) Search(ctx context.Context, keyword string, limit int) ([]Candidate, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	// The cache answers during a cooldown too: a keyword resolved once must
	// never cost a request again, however long the client is backing off for.
	if cached, ok := c.loadSearchCache(keyword); ok {
		return truncate(cached, limit), nil
	}
	if wait, blocked := c.searchBlocked(); blocked {
		return nil, refuse(true, "not asking again for another %v", wait.Round(time.Second))
	}

	cands, err := c.searchRemote(ctx, keyword, limit)
	if err != nil {
		var refusal *searchRefusal
		if errors.As(err, &refusal) {
			// Any refusal, not just a retryable one, stops the client asking
			// for a while: a login wall will not clear on its own either, and
			// the cost of being wrong is a batch that waits rather than a
			// batch that hammers.
			c.penaliseSearch()
		}
		return nil, err
	}
	c.forgiveSearch()
	c.saveSearchCache(keyword, cands)
	return cands, nil
}

// searchRemote asks the endpoint once.
//
// A refusal is returned as it arrived rather than retried: the endpoint
// answering "too frequent" is objecting to being asked, so asking again is the
// one thing guaranteed not to help. Only transport failures — which say nothing
// about the caller — are repeated.
func (c *Client) searchRemote(ctx context.Context, keyword string, limit int) ([]Candidate, error) {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 800 * time.Millisecond):
			}
		}
		cands, err := c.doSearch(ctx, keyword, limit)
		if err == nil {
			return cands, nil
		}
		lastErr = err
		var refusal *searchRefusal
		if errors.As(err, &refusal) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: %v", ErrSearchUnavailable, lastErr)
}

// penaliseSearch stops this client searching for a while, doubling the wait
// each time the endpoint refuses again.
//
// Doubling rather than a fixed pause because the window being enforced is
// undocumented and was measured to grow: a first refusal clears in minutes, and
// a client that keeps testing it stays blocked for longer than one that goes
// quiet. Doubling finds the current window without a human tuning a constant,
// and a success resets it so an unlucky refusal does not slow the next batch.
func (c *Client) penaliseSearch() {
	c.mu.Lock()
	defer c.mu.Unlock()

	base, cap := c.SearchCooldown, c.MaxSearchCooldown
	if base <= 0 {
		base = defaultSearchCooldown
	}
	if cap <= 0 {
		cap = defaultMaxSearchCooldown
	}
	switch {
	case c.searchPenalty <= 0:
		c.searchPenalty = base
	case c.searchPenalty < cap:
		c.searchPenalty *= 2
		if c.searchPenalty > cap {
			c.searchPenalty = cap
		}
	}
	c.searchBlockedUntil = time.Now().Add(c.searchPenalty)
}

// forgiveSearch clears the cooldown after the endpoint answers again.
func (c *Client) forgiveSearch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.searchPenalty = 0
	c.searchBlockedUntil = time.Time{}
}

// searchBlocked reports how much longer the client must leave the endpoint
// alone.
func (c *Client) searchBlocked() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := time.Until(c.searchBlockedUntil); wait > 0 {
		return wait, true
	}
	return 0, false
}

func (c *Client) doSearch(ctx context.Context, keyword string, limit int) ([]Candidate, error) {
	if err := c.pace(ctx, c.SearchMinInterval); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("s", keyword)
	q.Set("type", "1") // songs
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", "0")

	endpoint := c.SearchEndpoint
	if endpoint == "" {
		endpoint = defaultSearchEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		// 429 and the 5xx range are the server asking for another try; anything
		// else is it turning this caller away. Both are reported as the search
		// being unavailable, because that is what a caller can act on, but only
		// the first is worth repeating.
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, refuse(retry, "http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var api searchResponse
	if err := json.Unmarshal(body, &api); err != nil {
		// A login wall or a captcha is HTML, not JSON, and an API that has
		// moved answers with something else entirely. Both mean the same thing
		// to a caller, and neither is "no results" — but repeating the request
		// will not change either one.
		return nil, refuse(false, "not a song list: %v", err)
	}
	if api.Code != 0 && api.Code != 200 {
		// An error in the endpoint's own protocol. Measured on this device:
		// 405 and 406 arrive after a handful of searches, refusing even a
		// repeat of a request that just succeeded, and clear in minutes.
		return nil, refuse(true, "api code %d", api.Code)
	}

	out := make([]Candidate, 0, len(api.Result.Songs))
	for _, s := range api.Result.Songs {
		if s.ID == 0 {
			continue
		}
		cand := Candidate{
			MusicID:    s.ID,
			Name:       strings.TrimSpace(s.Name),
			Album:      strings.TrimSpace(s.Album.Name),
			DurationMS: s.Duration,
		}
		for _, a := range s.Artists {
			if name := strings.TrimSpace(a.Name); name != "" {
				cand.Artists = append(cand.Artists, name)
			}
		}
		out = append(out, cand)
	}
	return out, nil
}

func truncate(cands []Candidate, limit int) []Candidate {
	if len(cands) > limit {
		return cands[:limit]
	}
	return cands
}

// searchCacheEntry is one keyword's results. An empty Songs slice is cached
// too — "this keyword finds nothing" is an answer, and re-asking costs a
// request from a budget that is deliberately small.
type searchCacheEntry struct {
	Keyword string      `json:"keyword"`
	At      time.Time   `json:"at"`
	Songs   []Candidate `json:"songs"`
}

func (c *Client) searchCachePath(keyword string) string {
	if c.NoCache || c.CacheDir == "" {
		return ""
	}
	// The keyword is hashed rather than used as a filename: it can contain
	// slashes, colons and characters no filesystem here accepts.
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(keyword))))
	return filepath.Join(c.CacheDir, "search", hex.EncodeToString(sum[:])+".json")
}

func (c *Client) loadSearchCache(keyword string) ([]Candidate, bool) {
	// A user who asked for a fresh answer asked for one about the catalogue as
	// it stands now; a candidate list remembered from last month is the thing
	// they said not to use. The fresh list is still written back below.
	if c.refresh.Load() {
		return nil, false
	}
	path := c.searchCachePath(keyword)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var e searchCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, false
	}
	if time.Since(e.At) > searchCacheTTL {
		return nil, false
	}
	return e.Songs, true
}

func (c *Client) saveSearchCache(keyword string, cands []Candidate) {
	path := c.searchCachePath(keyword)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(searchCacheEntry{
		Keyword: keyword,
		At:      time.Now(),
		Songs:   cands,
	})
	if err != nil {
		return
	}
	// A cache that cannot be written is not a failure of the search that just
	// succeeded, so the error stops here.
	_ = os.WriteFile(path, data, 0o644)
}
