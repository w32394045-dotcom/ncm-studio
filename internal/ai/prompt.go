package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ncm-studio/internal/lyric"
)

// buildPrompt asks for one translated line per source line, as JSON.
//
// The lines are sent as a JSON array and the answer is asked for in the same
// shape for one reason: it is the only form where the count is unambiguous. A
// numbered list invites the model to renumber, a blank-line-separated block
// invites it to merge two short lines into one, and both of those produce a
// translation that is a line out of step with the music from that point on.
func buildPrompt(lines []string, target string) (system, user string, err error) {
	system = strings.Join([]string{
		"You translate song lyrics.",
		"You are given a JSON array of lyric lines and a target language.",
		"You reply with JSON and nothing else, in exactly this shape:",
		`{"lines": ["...", "..."]}`,
		"The lines array has exactly as many entries as the input, in the same order, one translation per input line.",
		"Never merge, split, drop, reorder or renumber lines.",
		"A line that is already in the target language, or that carries no words to translate (an instrumental mark, a name, a sound), is copied through unchanged.",
		"Do not add timestamps, headers, notes, explanations or romanisation.",
		"Do not wrap the JSON in a code fence.",
	}, "\n")

	encoded, err := json.Marshal(lines)
	if err != nil {
		return "", "", err
	}
	user = fmt.Sprintf("Translate these lyrics into %s.\n\n%s", target, encoded)
	return system, user, nil
}

// align places the model's answer back onto the timeline it was given.
//
// Every accepted reply shape ends up here, and all of them end the same way:
// the timestamps come from the source. What the model produced is only ever the
// words.
func align(src []lyric.Line, reply string) ([]lyric.Line, error) {
	texts, timed, err := parseReply(reply)
	if err != nil {
		return nil, err
	}

	if len(timed) > 0 {
		return alignTimed(src, timed)
	}

	// A model that answered with a JSON array of LRC lines has timed its own
	// answer, one entry at a time. Read it as what it is.
	if len(texts) > 0 {
		if _, ls := lyric.Parse(strings.Join(texts, "\n")); len(ls) == len(texts) {
			return alignTimed(src, ls)
		}
	}

	if len(texts) != len(src) {
		// Refused rather than padded or truncated: a translation that is one
		// line out from the third verse onward is worse than none, because it
		// looks right.
		return nil, fmt.Errorf("the model returned %d lines for %d; nothing was written", len(texts), len(src))
	}
	out := make([]lyric.Line, len(src))
	for i, l := range src {
		text := strings.TrimSpace(texts[i])
		if text == "" {
			text = l.Text // an empty answer is not a translation
		}
		out[i] = lyric.Line{Time: l.Time, Text: text}
	}
	return out, nil
}

// alignTimed keeps the source's timing and takes the words that land on it.
//
// A line the reply does not cover keeps the words it already had, so a model
// that skipped a chorus leaves one line untranslated rather than one line
// missing. A reply whose timestamps match nothing at all is refused: it is
// about a different song, or about no song.
func alignTimed(src, timed []lyric.Line) ([]lyric.Line, error) {
	byTime := make(map[string]string, len(timed))
	for _, l := range timed {
		key := lyric.FormatTime(l.Time)
		// The first line at a timestamp wins; a repeated timestamp is a
		// bilingual file's second language, not a second lyric.
		if _, ok := byTime[key]; !ok {
			byTime[key] = l.Text
		}
	}
	out := make([]lyric.Line, 0, len(src))
	missing := 0
	for _, l := range src {
		text, ok := byTime[lyric.FormatTime(l.Time)]
		if !ok {
			missing++
			text = l.Text
		}
		out = append(out, lyric.Line{Time: l.Time, Text: strings.TrimSpace(text)})
	}
	if missing == len(src) {
		return nil, errors.New("the reply's timestamps do not match the song")
	}
	return out, nil
}

// parseReply reads whichever of the accepted shapes the model used.
//
// Models follow the JSON instruction most of the time and something adjacent
// the rest of the time, and a reply that is nearly right is not worth throwing
// away over its packaging. Ordered from most to least specific:
//
//	{"lines": [...]}          what the prompt asked for
//	{"translation": "..."}    a single string, LRC or plain
//	[...]                     a bare array
//	[00:01.000]text           an LRC document
//	plain text                one line per line
func parseReply(reply string) (lines []string, timed []lyric.Line, err error) {
	body := stripFence(reply)
	if strings.TrimSpace(body) == "" {
		return nil, nil, errors.New("the model returned an empty answer")
	}

	if obj, ok := jsonObject(body); ok {
		for _, key := range []string{"lines", "lyrics", "translation", "translations", "text"} {
			raw, ok := obj[key]
			if !ok {
				continue
			}
			switch v := raw.(type) {
			case []any:
				return stringsFromArray(v), nil, nil
			case string:
				return fromText(v)
			}
		}
		// An object with none of the expected keys: fall through and try to
		// read the whole thing as text rather than failing outright.
	}

	if arr, ok := jsonArray(body); ok {
		return stringsFromArray(arr), nil, nil
	}
	return fromText(body)
}

// fromText reads a reply that is words rather than JSON: either an LRC document
// or one line per line.
func fromText(s string) ([]string, []lyric.Line, error) {
	_, lines := lyric.Parse(s)
	if len(lines) > 0 {
		return nil, lines, nil
	}
	var out []string
	for _, raw := range strings.Split(s, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return nil, nil, errors.New("the model returned an empty answer")
	}
	return out, nil, nil
}

// stringsFromArray reads an array of strings, or an array of objects carrying
// their text under one of the obvious keys.
func stringsFromArray(arr []any) []string {
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		switch v := item.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			for _, key := range []string{"text", "line", "translation", "lyrics", "content"} {
				if s, ok := v[key].(string); ok {
					out = append(out, s)
					break
				}
			}
		}
	}
	return out
}

// stripFence removes a Markdown code fence, which models add despite being told
// not to.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence and its language tag.
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		return ""
	}
	if end := strings.LastIndex(s, "```"); end >= 0 {
		s = s[:end]
	}
	return strings.TrimSpace(s)
}

// jsonObject decodes an object, reporting whether the text was one.
func jsonObject(s string) (map[string]any, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// jsonArray decodes an array, reporting whether the text was one. A model that
// emitted an array and then a sentence after it still counts: the array is the
// answer and the sentence is commentary.
func jsonArray(s string) ([]any, bool) {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return nil, false
	}
	// A decoder rather than Unmarshal: it stops at the end of the first value,
	// so trailing prose does not invalidate the answer.
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return nil, false
	}
	return arr, true
}
