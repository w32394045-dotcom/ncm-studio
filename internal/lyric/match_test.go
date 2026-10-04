package lyric

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The candidates below are shaped like real search responses for the file this
// feature was built for: 高野麻里佳; 石原夏織; 金元寿子; 津田美波 -
// ひめごと_クライシスターズ(もみじver.).flac.
//
// Two details of that response are what the scoring has to survive. The base
// recording is returned *first*, ahead of the version the file actually is, so
// taking the top hit would be wrong. And the versions differ only inside
// brackets, which is why brackets survive normalisation.
func himegotoCandidates() []Candidate {
	artists := []string{"高野麻里佳", "石原夏織", "金元寿子", "津田美波"}
	return []Candidate{
		{
			MusicID:    2008993850,
			Name:       "ひめごと*クライシスターズ",
			Artists:    artists,
			Album:      "ひめごと*クライシスターズ",
			DurationMS: 214253,
		},
		{
			MusicID:    2008994719,
			Name:       "ひめごと*クライシスターズ(もみじver.)",
			Artists:    artists,
			Album:      "ひめごと*クライシスターズ",
			DurationMS: 214253,
		},
		{
			MusicID:    2008994720,
			Name:       "ひめごと*クライシスターズ(まひろver.)",
			Artists:    artists,
			Album:      "ひめごと*クライシスターズ",
			DurationMS: 213000,
		},
	}
}

const himegotoFile = "高野麻里佳; 石原夏織; 金元寿子; 津田美波 - ひめごと_クライシスターズ(もみじver.).flac"

// himegotoDuration is what the real file's STREAMINFO says: 41,136,640 samples
// at 192 kHz.
var himegotoDuration = 214253 * time.Millisecond

func TestParseName(t *testing.T) {
	q := ParseName(himegotoFile)
	if len(q.Artists) != 4 {
		t.Fatalf("artists = %v, want four", q.Artists)
	}
	if q.Artists[0] != "高野麻里佳" || q.Artists[3] != "津田美波" {
		t.Errorf("artists = %v", q.Artists)
	}
	if q.Title != "ひめごと_クライシスターズ(もみじver.)" {
		t.Errorf("title = %q", q.Title)
	}
	// The extension must not survive into the query.
	if strings.Contains(q.Title, ".flac") {
		t.Error("the extension leaked into the title")
	}

	// A name with no separator is all title, not a broken artist list.
	bare := ParseName("just-a-title.mp3")
	if len(bare.Artists) != 0 || bare.Title != "just-a-title" {
		t.Errorf("bare name parsed as %+v", bare)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ a, b string }{
		{"ひめごと_クライシスターズ", "ひめごと*クライシスターズ"},
		{"ひめごと*クライシスターズ(もみじver.)", "ひめごと*クライシスターズ（もみじver.）"},
		{"Danza Kuduro", "danza  kuduro"},
		{"ＡＢＣ", "abc"},       // full-width letters
		{"ＳＯＮＧ　２", "song 2"}, // full-width space
	}
	for _, c := range cases {
		if normalize(c.a) != normalize(c.b) {
			t.Errorf("normalize(%q) = %q, normalize(%q) = %q; want equal",
				c.a, normalize(c.a), c.b, normalize(c.b))
		}
	}

	// Bracketed text is the version marker and must not be folded away.
	if normalize("Song (Live)") == normalize("Song (Studio)") {
		t.Error("normalisation erased the difference between two versions")
	}
}

func TestVersionSuffix(t *testing.T) {
	cases := map[string]string{
		"ひめごと*クライシスターズ(もみじver.)":   "もみじver",
		"ひめごと*クライシスターズ":            "",
		"Song [TV size]":           "tvsize",
		"Song （まひろver.）":           "まひろver",
		"Song":                     "",
		"Song (Live) (Remastered)": "remastered", // the last group is the version
	}
	for title, want := range cases {
		if got := versionSuffix(title); got != want {
			t.Errorf("versionSuffix(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestDecidePicksTheRightVersion is the case the whole scoring table exists
// for: the base recording comes back first, and writing its lyrics into the
// もみじ version's file would be silently wrong.
func TestDecidePicksTheRightVersion(t *testing.T) {
	q := ParseName(himegotoFile)
	d := Decide(q, himegotoCandidates(), himegotoDuration, false)

	if !d.Found {
		t.Fatal("no match found")
	}
	if d.Best.MusicID != 2008994719 {
		t.Errorf("picked %d (%q), want 2008994719 (the もみじ version)",
			d.Best.MusicID, d.Best.Name)
	}
	if !d.Auto {
		t.Errorf("a confident match was not auto-applied: %s", d.Reason)
	}

	// The base recording must score strictly lower, and must not be auto.
	base := Score(q, himegotoCandidates()[0], himegotoDuration)
	if base.Score >= d.Best.Score {
		t.Errorf("the base version scored %d, no lower than the correct one's %d",
			base.Score, d.Best.Score)
	}
	if base.SuffixScore >= 0 {
		t.Errorf("the base version's suffix scored %d, want a penalty", base.SuffixScore)
	}
}

// TestDecideRefusesWhatItCannotConfirm walks the cases that must reach the user
// rather than the file.
func TestDecideRefusesWhatItCannotConfirm(t *testing.T) {
	q := ParseName(himegotoFile)
	cands := himegotoCandidates()

	cases := []struct {
		name         string
		cands        []Candidate
		duration     time.Duration
		instrumental bool
		wantAuto     bool
		wantFound    bool
	}{
		{
			name:      "exact match is applied",
			cands:     cands[1:2],
			duration:  himegotoDuration,
			wantAuto:  true,
			wantFound: true,
		},
		{
			name:      "unknown length still applies, since it is no evidence",
			cands:     cands[1:2],
			duration:  0,
			wantAuto:  true,
			wantFound: true,
		},
		{
			name:      "a length that disagrees blocks it",
			cands:     cands[1:2],
			duration:  himegotoDuration + 40*time.Second,
			wantAuto:  false,
			wantFound: true,
		},
		{
			name:         "a backing track is never auto",
			cands:        cands[1:2],
			duration:     himegotoDuration,
			instrumental: true,
			wantAuto:     false,
			wantFound:    true,
		},
		{
			name:      "only the wrong version is available",
			cands:     cands[2:3],
			duration:  himegotoDuration,
			wantAuto:  false,
			wantFound: true,
		},
		{
			name:      "an unrelated song is not a match at all",
			cands:     []Candidate{{MusicID: 1, Name: "Totally Different", Artists: []string{"Someone"}}},
			duration:  himegotoDuration,
			wantAuto:  false,
			wantFound: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(q, c.cands, c.duration, c.instrumental)
			if d.Found != c.wantFound {
				t.Fatalf("Found = %v, want %v (reason: %s)", d.Found, c.wantFound, d.Reason)
			}
			if d.Auto != c.wantAuto {
				t.Errorf("Auto = %v, want %v (reason: %s)", d.Auto, c.wantAuto, d.Reason)
			}
			if !d.Auto && d.Reason == "" {
				t.Error("a refusal came with no reason to show the user")
			}
		})
	}
}

// TestDecideRefusesATie covers the one thing a threshold cannot settle: two
// different recordings that agree equally well. Writing either is a coin flip.
func TestDecideRefusesATie(t *testing.T) {
	q := Query{Artists: []string{"A"}, Title: "Song"}
	cands := []Candidate{
		{MusicID: 1, Name: "Song", Artists: []string{"A"}, DurationMS: 200000},
		{MusicID: 2, Name: "Song", Artists: []string{"A"}, DurationMS: 200000},
	}
	d := Decide(q, cands, 200*time.Second, false)
	if !d.Found {
		t.Fatal("no match found")
	}
	if d.Auto {
		t.Error("a tie between two songs was applied without asking")
	}
	if !strings.Contains(d.Reason, "same") {
		t.Errorf("reason = %q, want it to mention the tie", d.Reason)
	}
}

func TestLooksInstrumental(t *testing.T) {
	yes := []string{
		"Song (Instrumental)", "Song (Off Vocal)", "Song - offvocal",
		"Song (カラオケ)", "Song (インスト)", "Song (伴奏)", "Song 纯音乐",
	}
	for _, s := range yes {
		if !LooksInstrumental(s) {
			t.Errorf("LooksInstrumental(%q) = false, want true", s)
		}
	}
	no := []string{"Song", "Instinct", "Instrument", "Song (Live)"}
	for _, s := range no {
		if LooksInstrumental(s) {
			t.Errorf("LooksInstrumental(%q) = true, want false", s)
		}
	}
	// "inst" alone must not match; it is a substring of ordinary words. Only
	// the punctuated forms count.
	if LooksInstrumental("Instinct") {
		t.Error("the bare substring \"inst\" matched an ordinary word")
	}
}

func TestQueriesOrder(t *testing.T) {
	q := Query{Artists: []string{"A", "B"}, Title: "Song"}
	qs := q.Queries()
	if len(qs) < 2 {
		t.Fatalf("Queries() = %v, want the full credit first and a fallback", qs)
	}
	if qs[0] != "A B Song" {
		t.Errorf("first query = %q, want the whole credit", qs[0])
	}
	if qs[len(qs)-1] != "Song" {
		t.Errorf("last query = %q, want the bare title as the fallback", qs[len(qs)-1])
	}

	// An artist-less query still has to produce something searchable.
	if got := (Query{Title: "Song"}).Queries(); len(got) != 1 || got[0] != "Song" {
		t.Errorf("Queries() with no artists = %v", got)
	}
	if got := (Query{}).Queries(); len(got) != 0 {
		t.Errorf("Queries() with no title = %v, want none", got)
	}
}

func TestStemStripsTheVersionMarker(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ひめごと*クライシスターズ(もみじver.)", "ひめごと*クライシスターズ"},
		{"Song [TV size]", "Song"},
		{"Song（かえでver.）", "Song"},
		// The last group wins: "Song (Live)" is the song's name and the
		// remaster is what distinguishes this recording of it.
		{"Song (Live) (Remastered)", "Song (Live)"},
		{"No Marker At All", "No Marker At All"},
		// Stripping would leave nothing to search for, so it is left alone.
		{"(Instrumental)", "(Instrumental)"},
	}
	for _, c := range cases {
		if got := (Query{Title: c.in}).Stem().Title; got != c.want {
			t.Errorf("Stem(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGroupKeyFoldsRecordingsButNotSongs(t *testing.T) {
	momiji := Query{Artists: []string{"高野麻里佳", "石原夏織"}, Title: "ひめごと*クライシスターズ(もみじver.)"}
	kaede := Query{Artists: []string{"高野麻里佳", "石原夏織", "金元寿子"}, Title: "ひめごと*クライシスターズ(かえでver.)"}
	other := Query{Artists: []string{"高野麻里佳"}, Title: "べつの曲"}

	if GroupKey(momiji) != GroupKey(kaede) {
		t.Error("two recordings of one song got different keys")
	}
	if GroupKey(momiji) == GroupKey(other) {
		t.Error("two different songs share a key")
	}
	// The underscore the old tool wrote where the catalogue has an asterisk is
	// spelling, not identity.
	under := Query{Artists: []string{"高野麻里佳"}, Title: "ひめごと_クライシスターズ(もみじver.)"}
	if GroupKey(under) != GroupKey(momiji) {
		t.Error("one song spelled two ways got different keys")
	}
}

func TestGroupQueriesAskForTheSongNotTheRecording(t *testing.T) {
	q := Query{Artists: []string{"高野麻里佳", "石原夏織"}, Title: "ひめごと*クライシスターズ(もみじver.)"}
	got := q.GroupQueries()
	want := []string{"高野麻里佳 ひめごと*クライシスターズ", "ひめごと*クライシスターズ"}
	if !slices.Equal(got, want) {
		t.Errorf("GroupQueries() = %q, want %q", got, want)
	}
	// A file with no artist still has its title to search for.
	if got := (Query{Title: "Song (Live)"}).GroupQueries(); !slices.Equal(got, []string{"Song"}) {
		t.Errorf("GroupQueries() with no artist = %q", got)
	}
	if got := (Query{Title: "  "}).GroupQueries(); got != nil {
		t.Errorf("GroupQueries() with no title = %q", got)
	}
}
