package lyric

import (
	"context"
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
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound means the server answered, but holds no lyrics for this track
// (an instrumental, or one whose lyrics it does not have). It is a normal
// outcome rather than a failure.
var ErrNotFound = errors.New("lyric: no lyrics available for this track")

const (
	defaultEndpoint = "https://music.163.com/api/song/lyric"
	// NetEase throttles aggressively and the endpoint is unauthenticated, so
	// requests are issued one at a time with a floor between them.
	defaultMinInterval = 400 * time.Millisecond
	// defaultSearchMinInterval is the search endpoint's own, larger floor; see
	// Client.SearchMinInterval for the measurement behind it.
	defaultSearchMinInterval = 1200 * time.Millisecond
	// defaultSearchCooldown is how long searching stops after the endpoint
	// refuses one. See Client.SearchCooldown.
	defaultSearchCooldown = 2 * time.Minute
	// defaultMaxSearchCooldown caps the doubling that follows further refusals.
	defaultMaxSearchCooldown = 30 * time.Minute
	requestTimeout           = 15 * time.Second
	maxAttempts              = 3
)

// Lyrics is one track's lyrics in every form the API returned.
type Lyrics struct {
	MusicID int64 `json:"musicId"`
	// Original is the plain LRC timeline.
	Original string `json:"original,omitempty"`
	// Translation is the translated LRC timeline, if the track has one.
	Translation string `json:"translation,omitempty"`
	// Romanization is the transliterated timeline (roman or pinyin), if any.
	Romanization string    `json:"romanization,omitempty"`
	FetchedAt    time.Time `json:"fetchedAt"`
}

// HasTranslation reports whether a translation timeline was returned.
func (l *Lyrics) HasTranslation() bool {
	return l != nil && !IsEmpty(l.Translation)
}

// HasRomanization reports whether a romanized timeline was returned.
func (l *Lyrics) HasRomanization() bool {
	return l != nil && !IsEmpty(l.Romanization)
}

// Bilingual returns the merged original+translation timeline, falling back to
// the original alone when there is nothing to merge.
func (l *Lyrics) Bilingual() string {
	if l == nil {
		return ""
	}
	if !l.HasTranslation() {
		return l.Original
	}
	return Merge(l.Original, l.Translation)
}

// Client fetches lyrics, caching each track on disk and pacing its requests.
type Client struct {
	HTTP     *http.Client
	Endpoint string
	// SearchEndpoint is separate from Endpoint because the two are different
	// APIs, and because the search one is undocumented and unauthenticated —
	// keeping it addressable means a change to it, or a test standing in for
	// it, does not touch the lyric path.
	SearchEndpoint string
	CacheDir       string
	UserAgent      string
	// MinInterval is the floor between two outbound requests.
	MinInterval time.Duration
	// SearchMinInterval is a longer floor for searches alone.
	//
	// The search endpoint is the sensitive one: measured on this device, four
	// searches 400 ms apart earn a 405 "操作频繁，请稍候再试" — too frequent, try
	// later — while the lyric endpoint takes the same rate happily. Searching
	// is also the rare call, since a file that has been through here once
	// carries the id that makes the next pass skip the search entirely, so
	// paying for it in time costs little.
	SearchMinInterval time.Duration
	// SearchCooldown is how long this client stops searching after the endpoint
	// refuses one, doubling with each further refusal up to MaxSearchCooldown
	// and resetting when a search succeeds again.
	//
	// A refusal is not a hiccup to retry through. Measured on this device: the
	// search endpoint answers a handful of requests and then refuses
	// everything — including a request identical to one that just succeeded —
	// while the lyric endpoint, which is a different API, keeps serving
	// throughout. Retrying into that refusal is what turns a short block into a
	// long one, so instead the client stops asking and lets the rest of a batch
	// fail fast. The cooldown doubles because the window it has to outlast is
	// not documented and was measured to grow under load.
	SearchCooldown time.Duration
	// MaxSearchCooldown caps that doubling.
	MaxSearchCooldown time.Duration
	// NoCache disables reading and writing the on-disk cache.
	NoCache bool
	// refresh reads past the cache and asks again, while still storing what
	// comes back. It is the difference between "the copy I have will do" and
	// "the copy I have may be out of date", and it is deliberately not NoCache:
	// asking again is worth nothing if the answer is thrown away, and the point
	// of asking is that the stored copy ends up current.
	//
	// It is atomic because one client is shared by a batch's workers, which
	// read it per file, and by the settings page, which writes it.
	refresh atomic.Bool

	mu       sync.Mutex // held across a request so the pacing is global
	lastCall time.Time

	// searchBlockedUntil and searchPenalty are the cooldown described on
	// SearchCooldown; both are guarded by mu.
	searchBlockedUntil time.Time
	searchPenalty      time.Duration
}

// NewClient returns a client caching under cacheDir.
func NewClient(cacheDir string) *Client {
	return &Client{
		HTTP:              &http.Client{Timeout: requestTimeout},
		Endpoint:          defaultEndpoint,
		SearchEndpoint:    defaultSearchEndpoint,
		CacheDir:          cacheDir,
		UserAgent:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		MinInterval:       defaultMinInterval,
		SearchMinInterval: defaultSearchMinInterval,
		SearchCooldown:    defaultSearchCooldown,
		MaxSearchCooldown: defaultMaxSearchCooldown,
	}
}

type apiResponse struct {
	Code        int  `json:"code"`
	Uncollected bool `json:"uncollected"`
	LRC         struct {
		Lyric string `json:"lyric"`
	} `json:"lrc"`
	TLyric struct {
		Lyric string `json:"lyric"`
	} `json:"tlyric"`
	RomaLRC struct {
		Lyric string `json:"lyric"`
	} `json:"romalrc"`
}

// Fetch returns the lyrics for a track, using the cache when it can.
//
// An instrumental yields ErrNotFound; genuine transport or protocol failures
// yield a wrapped error. Callers can treat the two differently — the first is
// a permanent property of the track, the second is worth retrying later.
// SetRefresh decides whether the next fetch reads what is stored or asks the
// catalogue again. It is set from the settings before each use, so a change
// takes effect on the next file rather than on the next run.
func (c *Client) SetRefresh(on bool) { c.refresh.Store(on) }

func (c *Client) Fetch(ctx context.Context, musicID int64) (*Lyrics, error) {
	if musicID <= 0 {
		return nil, ErrNotFound
	}
	if !c.refresh.Load() {
		if l, ok := c.loadCache(musicID); ok {
			if l == nil {
				return nil, ErrNotFound
			}
			return l, nil
		}
	}

	l, err := c.fetchRemote(ctx, musicID)
	if err != nil {
		return nil, err
	}
	c.saveCache(musicID, l) // nil is cached too: instrumentals stay settled
	if l == nil {
		return nil, ErrNotFound
	}
	return l, nil
}

func (c *Client) fetchRemote(ctx context.Context, musicID int64) (*Lyrics, error) {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			// Back off between attempts without holding the pacing lock.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 800 * time.Millisecond):
			}
		}
		l, err := c.doRequest(ctx, musicID)
		if err == nil {
			return l, nil
		}
		lastErr = err
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("lyric %d: %w", musicID, lastErr)
}

// pace blocks until this client may make another request.
//
// The floor is enforced across every kind of request the client makes, not per
// endpoint: a backfill that searches for one track and then fetches its lyrics
// makes two calls, and pacing them separately would double the rate the server
// sees. A caller with a reason to be slower than the shared floor passes its
// own.
func (c *Client) pace(ctx context.Context, floor time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if floor < c.MinInterval {
		floor = c.MinInterval
	}
	if wait := floor - time.Since(c.lastCall); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	c.lastCall = time.Now()
	return nil
}

func (c *Client) doRequest(ctx context.Context, musicID int64) (*Lyrics, error) {
	if err := c.pace(ctx, 0); err != nil {
		return nil, err
	}

	// lv/tv/rv ask for the original, translated and romanized timelines.
	// lv=-1 is what makes the server return a plain LRC timeline rather than
	// the rich per-word JSON that the v1 endpoint produces.
	q := url.Values{}
	q.Set("id", strconv.FormatInt(musicID, 10))
	q.Set("lv", "-1")
	q.Set("tv", "-1")
	q.Set("rv", "-1")

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
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
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var api apiResponse
	if err := json.Unmarshal(body, &api); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if api.Code != 0 && api.Code != 200 {
		return nil, fmt.Errorf("api code %d", api.Code)
	}
	if api.Uncollected || IsEmpty(api.LRC.Lyric) {
		return nil, ErrNotFound
	}

	out := &Lyrics{
		MusicID:      musicID,
		Original:     strings.TrimSpace(api.LRC.Lyric),
		Translation:  strings.TrimSpace(api.TLyric.Lyric),
		Romanization: strings.TrimSpace(api.RomaLRC.Lyric),
		FetchedAt:    time.Now(),
	}
	// A translation that is only the placeholder text is no translation.
	if IsEmpty(out.Translation) {
		out.Translation = ""
	}
	if IsEmpty(out.Romanization) {
		out.Romanization = ""
	}
	return out, nil
}

// cacheEntry distinguishes "no lyrics" from "never fetched" on disk, so a
// library full of instrumentals is not re-queried on every run.
type cacheEntry struct {
	Lyrics *Lyrics `json:"lyrics,omitempty"`
	None   bool    `json:"none,omitempty"`
}

func (c *Client) cachePath(musicID int64) string {
	if c.CacheDir == "" {
		return ""
	}
	return filepath.Join(c.CacheDir, strconv.FormatInt(musicID, 10)+".json")
}

func (c *Client) loadCache(musicID int64) (*Lyrics, bool) {
	if c.NoCache {
		return nil, false
	}
	path := c.cachePath(musicID)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	if entry.None {
		return nil, true
	}
	if entry.Lyrics == nil {
		return nil, false
	}
	return entry.Lyrics, true
}

func (c *Client) saveCache(musicID int64, l *Lyrics) {
	if c.NoCache {
		return
	}
	path := c.cachePath(musicID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	entry := cacheEntry{Lyrics: l, None: l == nil}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	// Written via a temporary file so an interrupted run cannot leave a
	// half-written entry that would then be trusted on the next run.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}
