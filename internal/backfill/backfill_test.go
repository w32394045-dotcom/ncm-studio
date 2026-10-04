package backfill

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ncm-studio/internal/job"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/store"
	"ncm-studio/internal/tag"
)

// The file this feature was built for, in the two states it arrives in: one
// decoded before this tool wrote ids, and one that already names its track.
const (
	himegotoName  = "高野麻里佳; 石原夏織; 金元寿子; 津田美波 - ひめごと_クライシスターズ(もみじver.).flac"
	himegotoTitle = "ひめごと*クライシスターズ(もみじver.)"
	himegotoID    = int64(2008994719)
)

// A second recording of the same song, and the credit both files carry.
const (
	kaedeName    = "高野麻里佳; 石原夏織; 金元寿子; 津田美波 - ひめごと_クライシスターズ(かえでver.).flac"
	kaedeTitle   = "ひめごと*クライシスターズ(かえでver.)"
	kaedeID      = int64(2008994720)
	himegotoCast = "高野麻里佳; 石原夏織; 金元寿子; 津田美波"
)

const himegotoLyrics = "[00:01.000]きみのこえ\n[00:05.000]ひめごと\n"

// searchReply is the real response shape, with the base recording listed ahead
// of the version the file actually is — taking the top hit is wrong here, which
// is the whole reason the scoring table exists.
const searchReply = `{"code":200,"result":{"songCount":3,"songs":[
 {"id":2008993850,"name":"ひめごと*クライシスターズ",
  "artists":[{"name":"高野麻里佳"},{"name":"石原夏織"},{"name":"金元寿子"},{"name":"津田美波"}],
  "album":{"name":"ひめごと*クライシスターズ"},"duration":214253},
 {"id":2008994719,"name":"ひめごと*クライシスターズ(もみじver.)",
  "artists":[{"name":"高野麻里佳"},{"name":"石原夏織"},{"name":"金元寿子"},{"name":"津田美波"}],
  "album":{"name":"ひめごと*クライシスターズ"},"duration":214253},
 {"id":2008994720,"name":"ひめごと*クライシスターズ(かえでver.)",
  "artists":[{"name":"高野麻里佳"},{"name":"石原夏織"},{"name":"金元寿子"},{"name":"津田美波"}],
  "album":{"name":"ひめごと*クライシスターズ"},"duration":214253}]}}`

const lyricReply = `{"code":200,"lrc":{"lyric":"[00:01.000]きみのこえ\n[00:05.000]ひめごと"},
 "tlyric":{"lyric":"[00:01.000]你的声音\n[00:05.000]秘密"}}`

// catalogue stands in for NetEase. It answers both endpoints this feature uses
// and records which were asked, because several tests assert that a search was
// *not* made.
type catalogue struct {
	search string
	lyric  string
	status int

	mu       sync.Mutex
	searched []string
	fetched  []string
}

func (c *catalogue) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body := ""

	c.mu.Lock()
	switch {
	case strings.Contains(path, "search"):
		c.searched = append(c.searched, req.URL.Query().Get("s"))
		body = c.search
	case strings.Contains(path, "lyric"):
		c.fetched = append(c.fetched, req.URL.Query().Get("id"))
		body = c.lyric
	}
	c.mu.Unlock()

	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = "{}"
	}
	return &http.Response{
		StatusCode: status,
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

func (c *catalogue) fetches() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.fetched...)
}

// harness is one test's world: a settings store, a catalogue, and a processor
// built the same way main builds it.
type harness struct {
	dir       string
	cat       *catalogue
	client    *lyric.Client
	store     *store.Store
	processor job.Processor
}

func newHarness(t *testing.T, mode store.LyricsMode) *harness {
	t.Helper()
	home := t.TempDir()

	st, err := store.Open(filepath.Join(home, "config"), store.DefaultConfig(home))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(c *store.Config) {
		c.Workspace = filepath.Join(home, "workspace")
		c.Lyrics = mode
	}); err != nil {
		t.Fatal(err)
	}

	cat := &catalogue{search: searchReply, lyric: lyricReply}
	client := lyric.NewClient(filepath.Join(home, "workspace", "lyrics"))
	client.HTTP = &http.Client{Transport: cat}
	client.MinInterval = 0
	client.SearchMinInterval = 0

	h := &harness{dir: home, cat: cat, client: client, store: st}
	h.processor = Processor(Options{
		Store:   st,
		Clients: func(string) *lyric.Client { return client },
	})
	return h
}

// flac writes a FLAC fixture carrying the given tags, returning its path.
func (h *harness) flac(t *testing.T, name string, tags *tag.Tags) string {
	t.Helper()
	// 214.253 s at 44.1 kHz — the length the catalogue also reports — so the
	// duration term in the scoring is real rather than absent.
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
	streamInfo := append([]byte{0, 0, 0, 34}, si...)

	prefix := tag.BuildFLACMetadata(&tag.FLACSource{StreamInfo: streamInfo}, tags)
	audio := make([]byte, 8192)
	for i := range audio {
		audio[i] = byte(i)
	}
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, append(prefix, audio...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, h *harness, it *job.Item) job.Result {
	t.Helper()
	res, err := h.processor(context.Background(), it, func(string, int64, int64) {})
	if err != nil {
		t.Fatalf("processor: %v", err)
	}
	return res
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBackfillWritesWithoutSearchingWhenTheIdIsKnown: an id in the file is an
// answer this tool itself wrote after confirming a match, so searching again
// could only replace it with a guess.
func TestBackfillWritesWithoutSearchingWhenTheIdIsKnown(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	path := h.flac(t, himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織", "金元寿子", "津田美波"},
		MusicID: himegotoID,
	})
	before := fileBytes(t, path)

	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s), want done", res.State, res.Reason)
	}
	if res.Lyrics != "ok" || res.MusicID != himegotoID {
		t.Fatalf("result = %+v", res)
	}
	if got := h.cat.searches(); len(got) != 0 {
		t.Errorf("searched anyway, for %v", got)
	}

	// The timelines land in the file as they arrived: the original under its own
	// key, the translation under its own, and the two interleaved under the
	// plain lyrics key that every player reads.
	want := tag.Lyrics{
		Original:    strings.TrimSpace(himegotoLyrics),
		Translation: "[00:01.000]你的声音\n[00:05.000]秘密",
	}
	got, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lyrics.Original != want.Original || got.Lyrics.Translation != want.Translation {
		t.Errorf("written lyrics = %+v, want %+v", got.Lyrics, want)
	}
	if !strings.Contains(got.Lyrics.Merged, "きみのこえ") || !strings.Contains(got.Lyrics.Merged, "你的声音") {
		t.Errorf("the merged timeline is not bilingual: %q", got.Lyrics.Merged)
	}

	// The audio payload must be intact — the same bytes, somewhere in the file.
	audio := before[len(before)-8192:]
	if !strings.Contains(string(fileBytes(t, path)), string(audio)) {
		t.Error("the audio payload was damaged by the lyrics write")
	}
}

// TestBackfillNamesAFileThatCarriesOnlyAnId is the other half of the id-is-an-
// answer rule: the id says which track this is, but a file with no title and no
// artist is one a player cannot name, and a player that cannot name a track
// will not show its lyrics however well they are embedded.
//
// The name is borrowed from the id's own entry in the search pool, never
// guessed: the id was only ever written after a match was confirmed.
func TestBackfillNamesAFileThatCarriesOnlyAnId(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	path := h.flac(t, himegotoName, &tag.Tags{MusicID: himegotoID})

	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s), want done", res.State, res.Reason)
	}

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != himegotoTitle {
		t.Errorf("title = %q, want the catalogue's %q", info.Title, himegotoTitle)
	}
	if len(info.Artists) != 4 {
		t.Errorf("artists = %v, want all four", info.Artists)
	}
	if info.Album != "ひめごと*クライシスターズ" {
		t.Errorf("album = %q", info.Album)
	}
	if info.MusicID != himegotoID {
		t.Errorf("music id = %d — the id it already carried was changed", info.MusicID)
	}
	if !info.HasLyrics() {
		t.Error("the lyrics were not written")
	}

	// The fill is what makes the next pass free rather than another search.
	if again := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName}); again.State != "" && again.State != job.Done {
		t.Fatalf("second pass: state = %q (%s)", again.State, again.Reason)
	}
	if got := h.cat.searches(); len(got) != 1 {
		t.Errorf("made %d searches, want one for the nameless file and none after", len(got))
	}
}

// TestBackfillSearchesAndPicksTheRightVersion is the case the whole feature was
// built for: no id in the file, and a search that returns the base recording
// first. The もみじ version has to be chosen over it.
func TestBackfillSearchesAndPicksTheRightVersion(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	path := h.flac(t, himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織", "金元寿子", "津田美波"},
	})

	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s), want done", res.State, res.Reason)
	}
	if res.MusicID != himegotoID {
		t.Fatalf("matched %d, want the もみじ recording (%d)", res.MusicID, himegotoID)
	}
	if len(h.cat.searches()) == 0 {
		t.Fatal("no search was made")
	}
	if got := h.cat.fetches(); len(got) != 1 || got[0] != "2008994719" {
		t.Errorf("fetched %v, want only the matched track", got)
	}

	// And the id it settled on is written, so the next pass needs no search.
	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.MusicID != himegotoID {
		t.Errorf("music id = %d, want it recorded in the file", info.MusicID)
	}
}

// TestBackfillStopsAtWhatItCannotConfirm covers the cases that must reach a
// person. Writing a plausible wrong match is worse than asking: the lyrics end
// up in a file whose owner will read them.
func TestBackfillStopsAtWhatItCannotConfirm(t *testing.T) {
	cases := []struct {
		name     string
		title    string
		fileName string
		reply    string
		wantWhy  string
		// wantNoCandidates marks the case where nothing shared a title, so the
		// review row has nothing to offer and has to say so.
		wantNoCandidates bool
	}{
		{
			name:     "only a different version is in the catalogue",
			title:    himegotoTitle,
			fileName: himegotoName,
			reply: `{"code":200,"result":{"songs":[
			 {"id":2008994720,"name":"ひめごと*クライシスターズ(かえでver.)",
			  "artists":[{"name":"高野麻里佳"},{"name":"石原夏織"},{"name":"金元寿子"},{"name":"津田美波"}],
			  "album":{"name":"ひめごと*クライシスターズ"},"duration":214253}]}}`,
			wantWhy: "version suffix does not match",
		},
		{
			name:     "nothing resembling the file",
			title:    himegotoTitle,
			fileName: himegotoName,
			reply:    `{"code":200,"result":{"songs":[{"id":5,"name":"Something Else","artists":[{"name":"Nobody"}]}]}}`,
			wantWhy:  "no search result looked like this file",
			// Nothing shared a title, so there is nothing worth offering: the
			// list would be a page of unrelated songs. The reason says so and
			// the user searches by hand.
			wantNoCandidates: true,
		},
		{
			name:     "a backing track is never written unasked",
			title:    himegotoTitle,
			fileName: "高野麻里佳; 石原夏織; 金元寿子; 津田美波 - ひめごと_クライシスターズ(もみじver.) (Instrumental).flac",
			reply:    searchReply,
			wantWhy:  "looks like a backing track",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, store.LyricsEmbed)
			h.cat.search = c.reply
			path := h.flac(t, c.fileName, &tag.Tags{
				Title:   c.title,
				Artists: []string{"高野麻里佳", "石原夏織", "金元寿子", "津田美波"},
			})
			before := fileBytes(t, path)

			res := run(t, h, &job.Item{ID: "a", Path: path, Name: c.fileName})
			if res.State != job.Review {
				t.Fatalf("state = %q, want review (%s)", res.State, res.Reason)
			}
			if res.Reason != c.wantWhy {
				t.Errorf("reason = %q, want %q", res.Reason, c.wantWhy)
			}
			if c.wantNoCandidates {
				if len(res.Candidates) != 0 {
					t.Errorf("offered %d unrelated songs", len(res.Candidates))
				}
			} else if len(res.Candidates) == 0 {
				t.Error("the user was given nothing to choose from")
			}
			if !equalBytes(before, fileBytes(t, path)) {
				t.Error("a file waiting on confirmation was written anyway")
			}
			if got := h.cat.fetches(); len(got) != 0 {
				t.Errorf("lyrics were fetched for an unconfirmed match: %v", got)
			}
		})
	}
}

// TestBackfillSkipsAFileThatAlreadySaysThis is what makes a second pass over a
// finished library cheap: the lyrics are already there, so the file must not be
// copied again to change nothing.
func TestBackfillSkipsAFileThatAlreadySaysThis(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	path := h.flac(t, himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織"},
		MusicID: himegotoID,
	})

	run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	settled := fileBytes(t, path)

	// Clear the fetch cache so the second run has to go to the network for the
	// lyrics: the point is that the *write* is skipped, not the request.
	h.cat.mu.Lock()
	h.cat.fetched = nil
	h.cat.mu.Unlock()

	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s)", res.State, res.Reason)
	}
	if !equalBytes(settled, fileBytes(t, path)) {
		t.Error("a file that already had these lyrics was rewritten")
	}
}

// TestBackfillFileOnlyModeLeavesTheAudioAlone: the whole point of the mode is
// that it is instant, because it does not copy a hundred megabytes to write a
// text file beside them.
func TestBackfillFileOnlyModeLeavesTheAudioAlone(t *testing.T) {
	h := newHarness(t, store.LyricsFileOnly)
	path := h.flac(t, himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織"},
		MusicID: himegotoID,
	})
	before := fileBytes(t, path)

	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s)", res.State, res.Reason)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the audio file was rewritten in sidecar-only mode")
	}

	sidecar := SidecarPath(path)
	body, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("the .lrc was not written: %v", err)
	}
	if !strings.Contains(string(body), "きみのこえ") {
		t.Errorf(".lrc = %q", body)
	}
	// The sidecar carries the merged timeline, exactly as the decryptor writes
	// it: the platform's own exported .lrc files are bilingual, so a player
	// shown one of these looks the same as one shown the other.
	if !strings.Contains(string(body), "你的声音") {
		t.Error("the sidecar is missing the translation the catalogue returned")
	}
	if res.LRC != sidecar {
		t.Errorf("result lrc = %q, want %q", res.LRC, sidecar)
	}
}

// TestPerItemModeOverridesTheDefault is the double-tap in the list: one row can
// be asked for a sidecar while the batch as a whole embeds.
func TestPerItemModeOverridesTheDefault(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	path := h.flac(t, himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織"},
		MusicID: himegotoID,
	})
	before := fileBytes(t, path)

	fileOnly := int(store.LyricsFileOnly)
	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName, LyricsMode: &fileOnly})
	if res.State != "" && res.State != job.Done {
		t.Fatalf("state = %q (%s)", res.State, res.Reason)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the per-file override to sidecar-only did not stop the embed")
	}
	if _, err := os.Stat(SidecarPath(path)); err != nil {
		t.Errorf("no sidecar was written: %v", err)
	}

	// A nonsense mode falls back to the default rather than to "off": an
	// unknown integer from the API must not silently disable a batch.
	bogus := 99
	path2 := h.flac(t, "second - "+himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織"},
		MusicID: himegotoID,
	})
	res = run(t, h, &job.Item{ID: "b", Path: path2, Name: himegotoName, LyricsMode: &bogus})
	if res.Lyrics != "ok" {
		t.Errorf("an out-of-range mode was obeyed: %+v", res)
	}
}

// TestBackfillReportsATrackWithNoLyricsAsAnAnswer keeps "the catalogue has
// nothing for this track" from looking like a failure the user should retry.
func TestBackfillReportsATrackWithNoLyricsAsAnAnswer(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	h.cat.lyric = `{"code":200,"lrc":{"lyric":""},"uncollected":true}`
	path := h.flac(t, himegotoName, &tag.Tags{
		Title:   himegotoTitle,
		Artists: []string{"高野麻里佳", "石原夏織"},
		MusicID: himegotoID,
	})
	before := fileBytes(t, path)

	res := run(t, h, &job.Item{ID: "a", Path: path, Name: himegotoName})
	if res.State != job.Done {
		t.Fatalf("state = %q (%s), want done", res.State, res.Reason)
	}
	if res.Lyrics != "none" {
		t.Errorf("lyrics = %q, want none", res.Lyrics)
	}
	if !equalBytes(before, fileBytes(t, path)) {
		t.Error("the file was rewritten despite there being no lyrics")
	}
}

// TestSearchUnavailableFailsTheItem: an endpoint that has stopped answering is
// not "no such song", and the difference has to reach the user.
func TestSearchUnavailableFailsTheItem(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	h.cat.search = "<html>please log in</html>"
	path := h.flac(t, himegotoName, &tag.Tags{Title: himegotoTitle, Artists: []string{"A"}})

	_, err := h.processor(context.Background(), &job.Item{ID: "a", Path: path, Name: himegotoName},
		func(string, int64, int64) {})
	if err == nil {
		t.Fatal("a search wall was reported as success")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error = %v, want it to say the search is unavailable", err)
	}
}

// TestApplyWritesAConfirmedMatch is the second half of the review flow: the
// candidate the user tapped is written with the same rules the batch uses.
func TestApplyWritesAConfirmedMatch(t *testing.T) {
	h := newHarness(t, store.LyricsFileOnly)
	path := h.flac(t, himegotoName, &tag.Tags{Title: himegotoTitle, Artists: []string{"高野麻里佳"}})

	info, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	credit := tag.Credit{MusicID: himegotoID}
	out, err := Apply(context.Background(), h.client, info, credit, store.LyricsEmbed, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Embedded || out.MusicID != himegotoID {
		t.Fatalf("outcome = %+v", out)
	}

	after, err := tag.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.HasLyrics() || after.MusicID != himegotoID {
		t.Errorf("the confirmed match was not written: %+v", after)
	}

	// Confirming the same thing again is a no-op, not a second copy.
	out, err = Apply(context.Background(), h.client, after, credit, store.LyricsEmbed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Embedded {
		t.Error("applying an already-applied match rewrote the file")
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOneSearchAnswersForEveryVersionOfASong is the reason searches are grouped
// rather than issued per file. The library this was built for holds eight
// recordings of one single; the search endpoint refuses a client after a
// handful of requests, and the catalogue answers for all eight with a single
// result set anyway.
func TestOneSearchAnswersForEveryVersionOfASong(t *testing.T) {
	h := newHarness(t, store.LyricsEmbed)
	cast := strings.Split(himegotoCast, "; ")
	first := h.flac(t, himegotoName, &tag.Tags{Title: himegotoTitle, Artists: cast})
	second := h.flac(t, kaedeName, &tag.Tags{Title: kaedeTitle, Artists: cast})

	res1 := run(t, h, &job.Item{ID: "a", Path: first, Name: himegotoName})
	res2 := run(t, h, &job.Item{ID: "b", Path: second, Name: kaedeName})

	if res1.MusicID != himegotoID {
		t.Errorf("もみじver. matched id %d (%s), want %d", res1.MusicID, res1.Reason, himegotoID)
	}
	if res2.MusicID != kaedeID {
		t.Errorf("かえでver. matched id %d (%s), want %d", res2.MusicID, res2.Reason, kaedeID)
	}

	got := h.cat.searches()
	if len(got) != 1 {
		t.Fatalf("two recordings of one song made %d searches (%q), want 1", len(got), got)
	}
	if !strings.Contains(got[0], "ひめごと") || strings.Contains(got[0], "ver.") {
		t.Errorf("keyword = %q, want the song without its version marker", got[0])
	}
}

// TestDifferentSongsKeepDifferentPools is the other half of grouping: a file
// must never be scored against another song's candidates.
func TestDifferentSongsKeepDifferentPools(t *testing.T) {
	const otherName = "Someone Else - A Totally Different Song.flac"
	h := newHarness(t, store.LyricsEmbed)
	a := h.flac(t, himegotoName, &tag.Tags{Title: himegotoTitle, Artists: strings.Split(himegotoCast, "; ")})
	b := h.flac(t, otherName, &tag.Tags{Title: "A Totally Different Song", Artists: []string{"Someone Else"}})

	run(t, h, &job.Item{ID: "a", Path: a, Name: himegotoName})
	run(t, h, &job.Item{ID: "b", Path: b, Name: otherName})

	got := h.cat.searches()
	if len(got) != 2 {
		t.Fatalf("two different songs made %d searches (%q), want 2", len(got), got)
	}
	if got[0] == got[1] {
		t.Errorf("both songs searched for %q", got[0])
	}
}
