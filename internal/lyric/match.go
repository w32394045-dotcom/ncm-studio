package lyric

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Query is what a file claims to be, from its tags where it has them and from
// its name where it does not.
type Query struct {
	Artists []string
	Title   string
	Album   string
}

// ParseName splits a library filename into its parts.
//
// The convention on this device is "<artist>; <artist> - <title>.<ext>", which
// is also how NetEase spells a multi-artist credit. The split is on the first
// separator, because a title may contain one but a leading artist list will not.
func ParseName(name string) Query {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	artists, title, ok := strings.Cut(name, " - ")
	if !ok {
		return Query{Title: strings.TrimSpace(name)}
	}
	return Query{
		Artists: splitNames(artists),
		Title:   strings.TrimSpace(title),
	}
}

func splitNames(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == '/' || r == 0
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Queries returns the search strings to try, most specific first.
//
// The artist list is kept in the first one because NetEase's own index uses the
// same credit, and dropping it is what makes a search return every cover of a
// song instead of the recording. The bare title is the fallback for a file
// whose artist tag is wrong or absent.
func (q Query) Queries() []string {
	var out []string
	title := strings.TrimSpace(q.Title)
	if title == "" {
		return nil
	}
	if len(q.Artists) > 0 {
		out = append(out, strings.Join(q.Artists, " ")+" "+title)
	}
	if len(q.Artists) > 1 {
		// A long credit can over-constrain the search; the lead artist usually
		// still finds it.
		out = append(out, q.Artists[0]+" "+title)
	}
	return append(out, title)
}

// Stem returns the query with its version marker removed, so that every
// recording of one song reduces to the same query.
//
// This is what makes a batch cheap: "(もみじver.)", "(TV size ver.)" and
// "(Instrumental)" all name the same song, and the catalogue answers for every
// one of them with a single result set. Searching per file instead asks the
// same question once per recording, which is both wasteful and — on this
// endpoint — what earns a refusal.
func (q Query) Stem() Query {
	if base, _ := splitVersion(q.Title); strings.TrimSpace(base) != "" {
		q.Title = strings.TrimSpace(base)
	}
	return q
}

// GroupKey identifies the song a file is a recording of. Two files sharing a
// key can be matched against one search result set.
//
// The lead artist is part of the key but the rest of a long credit is not: a
// four-artist credit and a one-artist credit name the same recording, and
// keying on the full list would split one song into two searches.
func GroupKey(q Query) string {
	lead := ""
	if len(q.Artists) > 0 {
		lead = q.Artists[0]
	}
	return normalize(q.Stem().Title) + "\x00" + normalize(lead)
}

// GroupQueries returns the search strings for a whole song, most specific
// first.
//
// It is narrower than Queries on purpose. A group search happens once for every
// recording of a song rather than once per file, so only the two forms that
// carry their weight are offered: the lead artist with the title, and the title
// alone for a file whose artist tag is wrong.
func (q Query) GroupQueries() []string {
	title := strings.TrimSpace(q.Stem().Title)
	if title == "" {
		return nil
	}
	out := make([]string, 0, 2)
	if len(q.Artists) > 0 {
		if lead := strings.TrimSpace(q.Artists[0]); lead != "" {
			out = append(out, lead+" "+title)
		}
	}
	return append(out, title)
}

// Scored is a candidate together with the verdict for one file.
type Scored struct {
	Candidate
	Score        int
	ArtistScore  int
	TitleScore   int
	SuffixScore  int
	DurationDiff time.Duration
	// DurationKnown is false when either side's length is unknown, which is
	// the normal case for MP3. An unknown length is not evidence of a
	// mismatch, so it scores nothing rather than costing anything.
	DurationKnown bool
	Reasons       []string
}

// Score rates one candidate against a query.
//
// The weights are small integers rather than a percentage because the decision
// they feed is a threshold, not a ranking: what matters is that a track which
// agrees on title, artist and length clears it, and that a disagreement about
// which version of a song this is cannot.
func Score(q Query, c Candidate, fileDuration time.Duration) Scored {
	s := Scored{Candidate: c}

	s.TitleScore = titleScore(q.Title, c.Name)
	if s.TitleScore == 0 {
		// Without a shared title there is nothing to confirm: the candidate is
		// not a worse match, it is a different song. Callers filter on
		// TitleScore rather than on the total, which the remaining terms would
		// otherwise pull around zero.
		s.Reasons = append(s.Reasons, "title does not match")
		return s
	}

	s.ArtistScore = artistScore(q.Artists, c.Artists)
	qs, cs := versionSuffix(q.Title), versionSuffix(c.Name)
	switch {
	case qs == cs:
		s.SuffixScore = 2
		if qs != "" {
			s.Reasons = append(s.Reasons, "same version")
		}
	case qs != "" || cs != "":
		// One names a version and the other does not, or they name different
		// ones. Either way this is the case that must not be written blind:
		// picking between "(もみじver.)" and its base recording from the
		// metadata alone is a guess.
		s.SuffixScore = -3
		s.Reasons = append(s.Reasons, "version differs")
	}

	s.DurationDiff, s.DurationKnown = durationDiff(fileDuration, c.Duration())
	durScore := 0
	if s.DurationKnown {
		switch {
		case s.DurationDiff <= 2*time.Second:
			durScore = 2
		case s.DurationDiff <= 5*time.Second:
			durScore = 1
		case s.DurationDiff > 15*time.Second:
			durScore = -3
			s.Reasons = append(s.Reasons, "length differs")
		}
	}

	s.Score = s.TitleScore + s.ArtistScore + s.SuffixScore + durScore
	return s
}

// titleScore compares two titles after normalisation.
func titleScore(a, b string) int {
	na, nb := normalize(a), normalize(b)
	if na == "" || nb == "" {
		return 0
	}
	switch {
	case na == nb:
		return 3
	case strings.Contains(na, nb) || strings.Contains(nb, na):
		return 2
	}
	// The same song under a different version marker is still the same song.
	// It belongs in front of the user to confirm rather than in the bin, which
	// is the difference between "no match" and "is this the one you meant?".
	baseA, _ := splitVersion(a)
	baseB, _ := splitVersion(b)
	if x, y := normalize(baseA), normalize(baseB); x != "" && x == y {
		return 2
	}
	return 0
}

// artistScore compares two artist credits, best-effort in both directions.
//
// Comparing the joined strings handles a credit that is spelled differently but
// names the same people; comparing name by name handles one that is split
// differently. Either is evidence, so the better of the two wins.
func artistScore(query, cand []string) int {
	if len(query) == 0 || len(cand) == 0 {
		return 0
	}
	qa, ca := normalize(strings.Join(query, ";")), normalize(strings.Join(cand, ";"))
	switch {
	case qa != "" && qa == ca:
		return 3
	case qa != "" && ca != "" && (strings.Contains(qa, ca) || strings.Contains(ca, qa)):
		return 2
	}
	for _, a := range query {
		na := normalize(a)
		if na == "" {
			continue
		}
		for _, b := range cand {
			if na == normalize(b) {
				return 1
			}
		}
	}
	return 0
}

func durationDiff(a, b time.Duration) (time.Duration, bool) {
	if a <= 0 || b <= 0 {
		return 0, false
	}
	if d := a - b; d < 0 {
		return -d, true
	} else {
		return d, true
	}
}

// splitVersion separates a title from the last bracketed group in it: the
// "(もみじver.)", "(Live)" or "[TV size]" that distinguishes one recording of a
// song from another. The last group wins, because a title like
// "Song (Live) (Remastered)" is named by the trailing one.
//
// The base is returned as well as the suffix so a caller can compare the two
// recordings' shared title without the part that differs.
func splitVersion(title string) (base, suffix string) {
	pairs := [][2]string{{"(", ")"}, {"[", "]"}, {"（", "）"}, {"【", "】"}, {"〔", "〕"}}
	at, end := -1, -1
	for _, p := range pairs {
		i := strings.LastIndex(title, p[0])
		if i <= at {
			continue
		}
		j := strings.Index(title[i:], p[1])
		if j <= 0 {
			continue
		}
		at, end = i, i+j+len(p[1])
		suffix = title[i+len(p[0]) : i+j]
	}
	if at < 0 {
		return title, ""
	}
	return title[:at] + title[end:], suffix
}

// versionSuffix returns the normalised version marker of a title, or "" when
// the title names no version.
func versionSuffix(title string) string {
	_, suffix := splitVersion(title)
	return normalize(suffix)
}

// normalize folds the differences that are spelling rather than substance:
// case, full-width forms, and every separator or bracket a tagger might have
// chosen. What survives is letters and digits, which is why "(もみじver.)" and
// "（もみじver.）" compare equal and "ひめごと_クライシスターズ" matches
// "ひめごと*クライシスターズ".
//
// Bracketed text is *not* dropped. It is the part that says which recording
// this is, so it has to survive to be compared.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			r -= 0xFEE0 // full-width ASCII
		case r == 0x3000:
			r = ' ' // ideographic space
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Decision is what a caller needs to act: the best candidate, and whether
// writing it without asking is justified.
type Decision struct {
	Best Scored
	// Found is false when no candidate shared even a title.
	Found bool
	// Auto is true only when the match is strong enough to write unasked.
	Auto bool
	// Reason explains a refusal in terms a user can act on.
	Reason string
	// Instrumental records that the file looks like a backing track. It forces
	// a confirmation not because the match is bad but because the lyrics it
	// would fetch are, for that file, wrong.
	Instrumental bool
}

// autoScore is the bar for writing without asking. Title and artist together
// are worth six; the seventh has to come from something else agreeing — the
// length, or the version suffix.
const autoScore = 7

// Decide picks the best candidate and says whether it may be applied unasked.
//
// Anything short of certainty returns the best candidate anyway, for the user
// to confirm with one tap. Refusing to guess is the point: lyrics written into
// a file are the ones its owner will see, and a plausible wrong match is worse
// than a question.
func Decide(q Query, cands []Candidate, fileDuration time.Duration, instrumental bool) Decision {
	d := Decision{Instrumental: instrumental}

	for _, c := range cands {
		s := Score(q, c, fileDuration)
		if s.TitleScore == 0 {
			continue // a different song, not a worse match
		}
		if !d.Found || s.Score > d.Best.Score {
			d.Best, d.Found = s, true
		}
	}
	if !d.Found {
		d.Reason = "no candidate shares a title with this file"
		return d
	}

	// Two candidates scored alike is a coin flip between two different
	// recordings, which is exactly the case a threshold cannot settle.
	ties := 0
	for _, c := range cands {
		if s := Score(q, c, fileDuration); s.Score == d.Best.Score && s.MusicID != d.Best.MusicID {
			ties++
		}
	}

	switch {
	case instrumental:
		d.Reason = "looks like a backing track"
	case d.Best.SuffixScore < 0:
		d.Reason = "version suffix does not match"
	case d.Best.TitleScore < 3:
		d.Reason = "title only partially matches"
	case d.Best.ArtistScore < 2:
		d.Reason = "artists do not match"
	case d.Best.Score < autoScore:
		d.Reason = "not enough agrees to be sure"
	case ties > 0:
		d.Reason = "another version scored the same"
	default:
		d.Auto = true
	}
	return d
}

// instrumentalMarkers are the strings that name a backing track.
//
// They are matched against the file's name and its title, never its album: a
// real search response on this device returned an album called
// "…(Instrumental)" for an unrelated song, which would have marked the vocal
// version of it as a backing track.
var instrumentalMarkers = []string{
	"instrumental", "off vocal", "offvocal", "off voice", "no vocal",
	"karaoke", "カラオケ", "インスト", "オフボーカル", "伴奏", "纯音乐", "純音樂",
	"(inst)", "inst.",
}

// LooksInstrumental reports whether a name or title names a backing track.
func LooksInstrumental(parts ...string) bool {
	for _, p := range parts {
		lower := strings.ToLower(p)
		for _, m := range instrumentalMarkers {
			if strings.Contains(lower, m) {
				return true
			}
		}
	}
	return false
}
