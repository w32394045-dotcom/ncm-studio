// Package lyric fetches and combines NetEase Cloud Music lyrics.
//
// NCM containers hold no lyrics at all — the metadata JSON has no lyric, lrc
// or yrc field. Lyrics live only on the server, keyed by the musicId that the
// container does carry, so they are fetched over the public lyric endpoint and
// then written back into the decrypted audio.
package lyric

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Line is a single timed lyric line.
type Line struct {
	Time time.Duration
	Text string
}

// timeTag matches one timestamp such as [01:23.456] or [01:23.45]. A line may
// carry several of them, which means the text repeats at each time.
var timeTag = regexp.MustCompile(`\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

// metaTag matches bracketed headers such as [by:someone] or [offset:500] that
// are not timestamps.
var metaTag = regexp.MustCompile(`^\[[a-zA-Z#][^\]]*\]`)

// noLyricSentinels are the placeholder bodies NetEase returns for tracks that
// genuinely have no lyrics. They arrive with ordinary timestamps and a 200
// response, so they have to be recognised by their text.
var noLyricSentinels = []string{
	"暂无歌词",
	"纯音乐，请欣赏",
	"纯音乐,请欣赏",
	"纯音乐，请您欣赏",
	"此歌曲为没有填词的纯音乐，请您欣赏",
	"该歌曲为纯音乐，请欣赏",
}

// Parse splits an LRC document into its header tags and its timed lines.
// Headers are returned in source order, without their trailing newline.
func Parse(doc string) (headers []string, lines []Line) {
	seen := make(map[string]bool)
	for _, raw := range strings.Split(doc, "\n") {
		raw = strings.TrimRight(raw, "\r")
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		matches := timeTag.FindAllStringSubmatchIndex(line, -1)
		if len(matches) == 0 {
			// A non-timestamped bracket line is a header; anything else is
			// untimed prose, which LRC has no place for.
			if metaTag.MatchString(line) && !seen[line] {
				seen[line] = true
				headers = append(headers, line)
			}
			continue
		}

		// Text is whatever follows the final timestamp.
		text := strings.TrimSpace(line[matches[len(matches)-1][1]:])
		if text == "" {
			continue
		}
		for _, m := range matches {
			if t, ok := parseTime(line[m[2]:m[3]], line[m[4]:m[5]], line[m[6]:m[7]]); ok {
				lines = append(lines, Line{Time: t, Text: text})
			}
		}
	}
	return headers, lines
}

func parseTime(mm, ss, frac string) (time.Duration, bool) {
	minutes, err := strconv.Atoi(mm)
	if err != nil {
		return 0, false
	}
	seconds, err := strconv.Atoi(ss)
	if err != nil {
		return 0, false
	}

	// The fractional part is hundredths when two digits, milliseconds when
	// three, and tenths when one.
	var millis int
	if frac != "" {
		v, err := strconv.Atoi(frac)
		if err != nil {
			return 0, false
		}
		switch len(frac) {
		case 1:
			millis = v * 100
		case 2:
			millis = v * 10
		default:
			millis = v
		}
	}
	return time.Duration(minutes)*time.Minute +
		time.Duration(seconds)*time.Second +
		time.Duration(millis)*time.Millisecond, true
}

// FormatTime renders a duration the way LRC timestamps are written.
func FormatTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := d.Milliseconds()
	return fmt.Sprintf("[%02d:%02d.%03d]",
		total/60000, (total/1000)%60, total%1000)
}

// Body returns the lyric text with timestamps and headers removed. It is what
// the no-lyrics sentinels are matched against.
func Body(doc string) string {
	_, lines := Parse(doc)
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, l.Text)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// IsEmpty reports whether a lyric document carries no actual words, covering
// both a blank response and the placeholder text NetEase uses for
// instrumentals and tracks whose lyrics it does not hold.
func IsEmpty(doc string) bool {
	body := Body(doc)
	if body == "" {
		return true
	}
	for _, s := range noLyricSentinels {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}

// Render writes timed lines back out as an LRC document, with headers first.
func Render(headers []string, lines []Line) string {
	var b strings.Builder
	for _, h := range headers {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	for _, l := range lines {
		b.WriteString(FormatTime(l.Time))
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// Merge combines the original lyrics with their translation into a single
// document in which each translated line sits directly under the line it
// translates, carrying the same timestamp.
//
// This is the layout every mainstream player already renders as bilingual
// lyrics, so it works without the player knowing anything about translations.
// Timestamps present in only one of the two documents are kept rather than
// dropped, so an unmatched line never disappears.
func Merge(original, translation string) string {
	headers, orig := Parse(original)
	if len(orig) == 0 {
		return original
	}
	if IsEmpty(translation) {
		return Render(headers, orig)
	}
	_, trans := Parse(translation)
	if len(trans) == 0 {
		return Render(headers, orig)
	}

	transAt := make(map[time.Duration][]string, len(trans))
	for _, l := range trans {
		transAt[l.Time] = append(transAt[l.Time], l.Text)
	}

	// A translated line that is byte-identical to its original adds nothing
	// and would just double every line on screen.
	merged := make([]Line, 0, len(orig)+len(trans))
	used := make(map[time.Duration]bool, len(trans))
	for _, l := range orig {
		merged = append(merged, l)
		for _, t := range transAt[l.Time] {
			if t != l.Text {
				merged = append(merged, Line{Time: l.Time, Text: t})
			}
		}
		used[l.Time] = true
	}

	// Translation-only timestamps: keep them in time order at the end.
	var orphans []Line
	for t, texts := range transAt {
		if used[t] {
			continue
		}
		for _, text := range texts {
			orphans = append(orphans, Line{Time: t, Text: text})
		}
	}
	if len(orphans) > 0 {
		sort.SliceStable(orphans, func(i, j int) bool { return orphans[i].Time < orphans[j].Time })
		merged = append(merged, orphans...)
		sort.SliceStable(merged, func(i, j int) bool { return merged[i].Time < merged[j].Time })
	}

	return Render(headers, merged)
}

// Combine merges two timelines without losing a line from either, which is what
// the .lrc page means by merging: a file whose tags hold one version of a song
// and a .lrc somebody has edited hold two, and both are the user's.
//
// A line that appears in both documents at the same timestamp with the same
// text is written once, so importing the same .lrc twice is a no-op rather than
// a document that doubles in size. A line that appears in both at the same
// timestamp with different text is kept twice: there is no way to tell a
// correction of the first from a translation of it, and guessing wrong means
// silently discarding one of them.
//
// The first document's headers are kept, and its lines are listed first at a
// shared timestamp.
func Combine(a, b string) string {
	headers, la := Parse(a)
	_, lb := Parse(b)
	if len(lb) == 0 {
		return Render(headers, la)
	}
	if len(la) == 0 {
		return Render(headers, lb)
	}

	seen := make(map[Line]bool, len(la)+len(lb))
	out := make([]Line, 0, len(la)+len(lb))
	appendOnce := func(l Line) {
		if seen[l] {
			return
		}
		seen[l] = true
		out = append(out, l)
	}

	// b's lines are indexed by time so each can be placed with the a line it
	// shares a timestamp with, rather than at the end.
	at := make(map[time.Duration][]Line, len(lb))
	for _, l := range lb {
		at[l.Time] = append(at[l.Time], l)
	}
	used := make(map[Line]bool, len(lb))
	for _, l := range la {
		appendOnce(l)
		for _, m := range at[l.Time] {
			if m == l {
				used[m] = true
				continue
			}
			appendOnce(m)
			used[m] = true
		}
	}
	for _, l := range lb {
		if !used[l] {
			appendOnce(l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return Render(headers, out)
}

// Contains reports whether every line of inner also appears in outer.
//
// It is how a write that would change nothing is recognised: a .lrc whose lines
// are all already in the file has been imported already, and re-running the
// import must not copy the audio again to arrive at the same place.
func Contains(outer, inner string) bool {
	_, have := Parse(outer)
	_, want := Parse(inner)
	if len(want) == 0 {
		return true
	}
	present := make(map[Line]bool, len(have))
	for _, l := range have {
		present[l] = true
	}
	for _, l := range want {
		if !present[l] {
			return false
		}
	}
	return true
}
