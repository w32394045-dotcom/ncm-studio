package lyric

import (
	"strings"
	"testing"
	"time"
)

func TestParseTimestamps(t *testing.T) {
	doc := "[ti:Test]\n[by:someone]\n[00:01.50]first\n[01:02.345]second\n[00:03.5]tenths\n"
	headers, lines := Parse(doc)

	if len(headers) != 2 || headers[0] != "[ti:Test]" || headers[1] != "[by:someone]" {
		t.Errorf("headers = %q", headers)
	}
	want := []Line{
		{1500 * time.Millisecond, "first"},
		{62345 * time.Millisecond, "second"},
		{3500 * time.Millisecond, "tenths"},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line[%d] = %+v, want %+v", i, lines[i], want[i])
		}
	}
}

func TestParseRepeatedTimestamp(t *testing.T) {
	// One line, two timestamps: the text repeats at both times.
	_, lines := Parse("[00:01.00][00:05.00]chorus\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
	if lines[0].Text != "chorus" || lines[1].Text != "chorus" {
		t.Errorf("text not repeated: %+v", lines)
	}
	if lines[1].Time != 5*time.Second {
		t.Errorf("second time = %v", lines[1].Time)
	}
}

func TestIsEmptyRecognisesPlaceholders(t *testing.T) {
	cases := map[string]bool{
		"":                  true,
		"[00:00.00]暂无歌词":    true,
		"[99:00.00]纯音乐，请欣赏": true,
		"[00:00.000]该歌曲为纯音乐，请欣赏": true,
		"[00:01.00]real words":   false,
		"[00:01.00]作词 : 黄家驹":     false,
	}
	for doc, want := range cases {
		if got := IsEmpty(doc); got != want {
			t.Errorf("IsEmpty(%q) = %v, want %v", doc, got, want)
		}
	}
}

func TestMergePutsTranslationUnderOriginal(t *testing.T) {
	orig := "[00:01.00]Hello\n[00:02.00]World\n"
	trans := "[00:01.00]你好\n[00:02.00]世界\n"

	got := Merge(orig, trans)
	want := "[00:01.000]Hello\n[00:01.000]你好\n[00:02.000]World\n[00:02.000]世界\n"
	if got != want {
		t.Errorf("Merge:\n got %q\nwant %q", got, want)
	}
}

func TestMergeDropsDuplicatedLines(t *testing.T) {
	// A translation identical to the original would double every line.
	orig := "[00:01.00]Same\n"
	got := Merge(orig, "[00:01.00]Same\n")
	if strings.Count(got, "Same") != 1 {
		t.Errorf("duplicate line kept: %q", got)
	}
}

func TestMergeKeepsUnmatchedLines(t *testing.T) {
	orig := "[00:01.00]Hello\n[00:02.00]World\n"
	trans := "[00:01.00]你好\n[00:03.00]只有翻译\n"

	got := Merge(orig, trans)
	for _, want := range []string{"Hello", "你好", "World", "只有翻译"} {
		if !strings.Contains(got, want) {
			t.Errorf("Merge dropped %q:\n%s", want, got)
		}
	}
	// The translation-only line must land in time order, not at the front.
	if strings.Index(got, "只有翻译") < strings.Index(got, "World") {
		t.Errorf("orphan translation is out of order:\n%s", got)
	}
}

func TestMergeWithoutTranslationReturnsOriginal(t *testing.T) {
	orig := "[00:01.00]Hello\n"
	if got := Merge(orig, ""); got != "[00:01.000]Hello\n" {
		t.Errorf("Merge with empty translation = %q", got)
	}
	if got := Merge(orig, "[00:00.00]暂无歌词"); got != "[00:01.000]Hello\n" {
		t.Errorf("Merge with placeholder translation = %q", got)
	}
}

func TestFormatTime(t *testing.T) {
	cases := map[time.Duration]string{
		0:                             "[00:00.000]",
		1500 * time.Millisecond:       "[00:01.500]",
		62345 * time.Millisecond:      "[01:02.345]",
		3*time.Minute + 7*time.Second: "[03:07.000]",
	}
	for d, want := range cases {
		if got := FormatTime(d); got != want {
			t.Errorf("FormatTime(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCombineKeepsEveryLineOnce(t *testing.T) {
	a := "[00:01.00]Hello\n[00:02.00]World\n"
	b := "[00:01.00]Hello\n[00:03.00]Later\n"

	got := Combine(a, b)
	want := "[00:01.000]Hello\n[00:02.000]World\n[00:03.000]Later\n"
	if got != want {
		t.Errorf("Combine:\n got %q\nwant %q", got, want)
	}

	// Merging a document with itself must not double it: this is what makes
	// importing the same .lrc twice converge instead of growing the file.
	if again := Combine(got, b); again != got {
		t.Errorf("Combine is not idempotent:\n got %q\nwant %q", again, got)
	}
}

func TestCombineKeepsBothTextsAtOneTimestamp(t *testing.T) {
	// A corrected line and a translated line look the same from here, and
	// guessing wrong would discard one of the user's own documents.
	a := "[00:01.00]Hello\n"
	b := "[00:01.00]你好\n"
	got := Combine(a, b)
	for _, want := range []string{"Hello", "你好"} {
		if !strings.Contains(got, want) {
			t.Errorf("Combine dropped %q: %q", want, got)
		}
	}
	// The first document's line is listed first at a shared timestamp.
	if strings.Index(got, "Hello") > strings.Index(got, "你好") {
		t.Errorf("Combine reordered the first document: %q", got)
	}
}

func TestCombineKeepsHeaders(t *testing.T) {
	got := Combine("[by:someone]\n[00:01.00]Hello\n", "[00:02.00]World\n")
	if !strings.HasPrefix(got, "[by:someone]\n") {
		t.Errorf("Combine dropped the header: %q", got)
	}
}

func TestCombineWithOneSidedInputs(t *testing.T) {
	only := "[00:05.00]Only\n"
	if got := Combine("", only); got != "[00:05.000]Only\n" {
		t.Errorf("Combine with no first document = %q", got)
	}
	if got := Combine(only, ""); got != "[00:05.000]Only\n" {
		t.Errorf("Combine with no second document = %q", got)
	}
}

func TestContains(t *testing.T) {
	outer := "[00:01.00]Hello\n[00:02.00]World\n"
	cases := []struct {
		name  string
		inner string
		want  bool
	}{
		{"empty is always contained", "", true},
		{"both lines present", "[00:02.00]World\n[00:01.00]Hello\n", true},
		{"one line missing", "[00:01.00]Hello\n[00:03.00]Other\n", false},
		{"same text, different time", "[00:03.00]Hello\n", false},
		{"untimed text carries nothing", "[ar:someone]\n", true},
	}
	for _, c := range cases {
		if got := Contains(outer, c.inner); got != c.want {
			t.Errorf("%s: Contains = %v, want %v", c.name, got, c.want)
		}
	}
}
