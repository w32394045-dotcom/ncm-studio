package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sourceLRC = "[00:01.000]きみのこえ\n[00:05.000]ひめごと\n"

// server stands in for one provider: it records the request and answers with a
// canned body.
type server struct {
	*httptest.Server
	path   string
	auth   string
	header http.Header
	body   map[string]any
	status int
}

func newServer(t *testing.T, reply string) *server {
	t.Helper()
	s := &server{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.path = r.URL.Path
		s.header = r.Header.Clone()
		s.auth = r.Header.Get("Authorization")
		if s.auth == "" {
			s.auth = r.Header.Get("x-api-key")
		}
		if s.auth == "" {
			s.auth = r.Header.Get("x-goog-api-key")
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &s.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) userText(t *testing.T) string {
	t.Helper()
	return s.messageContent(t, "user")
}

func (s *server) messageContent(t *testing.T, role string) string {
	t.Helper()
	msgs, _ := s.body["messages"].([]any)
	for _, m := range msgs {
		entry, _ := m.(map[string]any)
		if entry["role"] == role {
			text, _ := entry["content"].(string)
			return text
		}
	}
	return ""
}

func clientFor(t *testing.T, s *server, opts Options) *Client {
	t.Helper()
	opts.Endpoint = s.URL
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTranslateAlignsOntoTheSourceTimestamps(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"role":"assistant",
		"content":"{\"lines\":[\"你的声音\",\"秘密\"]}"}}]}`)
	c := clientFor(t, s, Options{
		Provider: "openai", Key: "sk-test", Target: "简体中文",
	})

	res, err := c.Translate(context.Background(), sourceLRC)
	if err != nil {
		t.Fatal(err)
	}
	want := "[00:01.000]你的声音\n[00:05.000]秘密\n"
	if res.Translation != want {
		t.Errorf("Translation:\n got %q\nwant %q", res.Translation, want)
	}
	if res.Provider != "openai" || res.Model != "gpt-4o-mini" {
		t.Errorf("attribution = %s/%s", res.Provider, res.Model)
	}
	if !strings.Contains(s.userText(t), "简体中文") {
		t.Error("the target language was not sent")
	}
	if s.auth != "Bearer sk-test" {
		t.Errorf("auth = %q", s.auth)
	}
}

// The trap this package was written around: DeepSeek's reasoner answers with
// its chain of thought in a field beside the text. Reading the wrong field
// writes the model's deliberations into somebody's lyrics.
func TestDeepSeekReasoningIsNeverTheTranslation(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"role":"assistant",
		"reasoning_content":"We must translate each line. Line 1 is きみのこえ, meaning your voice...",
		"content":"{\"lines\":[\"你的声音\",\"秘密\"]}"}}]}`)
	c := clientFor(t, s, Options{
		Provider: "deepseek", Key: "sk-test", Target: "简体中文",
	})
	if c.Model() != "deepseek-chat" {
		t.Errorf("default model = %q", c.Model())
	}

	res, err := c.Translate(context.Background(), sourceLRC)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Translation, "We must translate") {
		t.Fatalf("the reasoning was written as lyrics:\n%s", res.Translation)
	}
	if !strings.Contains(res.Translation, "你的声音") {
		t.Errorf("the translation was lost:\n%s", res.Translation)
	}
}

// And when the model produced reasoning but no answer at all, the honest
// outcome is a failure with nothing written.
func TestDeepSeekReasoningWithoutAnAnswerFails(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"role":"assistant",
		"reasoning_content":"Let me think about this...","content":""},
		"finish_reason":"length"}]}`)
	c := clientFor(t, s, Options{Provider: "deepseek", Key: "sk-test", Target: "English"})

	_, err := c.Translate(context.Background(), sourceLRC)
	if err == nil {
		t.Fatal("a reasoning-only reply was accepted")
	}
	if !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestClaudeJoinsTextBlocksOnly(t *testing.T) {
	s := newServer(t, `{"content":[
		{"type":"thinking","thinking":"hmm"},
		{"type":"text","text":"{\"lines\":[\"Your voice\",\"A secret\"]}"}],
		"stop_reason":"end_turn"}`)
	c := clientFor(t, s, Options{
		Provider: "claude", Key: "sk-ant", Target: "English", Model: "claude-sonnet-5-5",
	})

	res, err := c.Translate(context.Background(), sourceLRC)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Translation, "Your voice") || strings.Contains(res.Translation, "hmm") {
		t.Errorf("blocks were misread:\n%s", res.Translation)
	}
	if s.auth != "sk-ant" {
		t.Errorf("auth = %q", s.auth)
	}
	if s.body["system"] == nil {
		t.Error("the system prompt was not sent")
	}
}

func TestGeminiNamesTheModelInThePath(t *testing.T) {
	s := newServer(t, `{"candidates":[{"content":{"parts":[
		{"text":"{\"lines\":[\"Your voice\",\"A secret\"]}"}]},"finishReason":"STOP"}]}`)
	c := clientFor(t, s, Options{Provider: "gemini", Key: "AIza-test", Target: "English"})

	res, err := c.Translate(context.Background(), sourceLRC)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Translation, "Your voice") {
		t.Errorf("Translation = %q", res.Translation)
	}
	if !strings.HasSuffix(s.path, "/gemini-2.5-flash:generateContent") {
		t.Errorf("path = %q", s.path)
	}
	if s.auth != "AIza-test" {
		t.Errorf("auth = %q", s.auth)
	}
}

// A refusal that says what happened is the difference between a user fixing
// their key and a user giving up.
func TestProviderErrorsCarryTheirMessage(t *testing.T) {
	s := newServer(t, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`)
	s.status = http.StatusUnauthorized
	c := clientFor(t, s, Options{Provider: "openai", Key: "bad", Target: "English"})

	_, err := c.Translate(context.Background(), sourceLRC)
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !strings.Contains(err.Error(), "Incorrect API key provided") {
		t.Errorf("error lost the provider's message: %v", err)
	}
}

func TestNewRefusesAMissingKey(t *testing.T) {
	if _, err := New(Options{Provider: "openai", Target: "English"}); err == nil {
		t.Error("a client was built with no key")
	}
	if _, err := New(Options{Provider: "nope", Key: "x"}); err == nil {
		t.Error("a client was built for an unknown provider")
	}
}

func TestTranslateRefusesADocumentWithNoLines(t *testing.T) {
	s := newServer(t, `{}`)
	c := clientFor(t, s, Options{Provider: "openai", Key: "k", Target: "English"})
	if _, err := c.Translate(context.Background(), "[ar:someone]\n"); err == nil {
		t.Error("an untimed document was accepted")
	}
	if s.path != "" {
		t.Error("a request was made for a document with no lines")
	}
}

// Models wrap their JSON in a code fence, answer with a bare array, or write
// the whole LRC themselves. All of those are the right answer in the wrong
// packaging.
func TestReplyShapes(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
	}{
		{"fenced json", "```json\n{\"lines\":[\"A\",\"B\"]}\n```", "[00:01.000]A\n[00:05.000]B\n"},
		{"bare array", `["A","B"]`, "[00:01.000]A\n[00:05.000]B\n"},
		{"array with prose", "Here you go:\n[\"A\",\"B\"]\nHope that helps!", "[00:01.000]A\n[00:05.000]B\n"},
		{"array of objects", `[{"text":"A"},{"line":"B"}]`, "[00:01.000]A\n[00:05.000]B\n"},
		{"translation string", `{"translation":"[00:01.000]A\n[00:05.000]B"}`, "[00:01.000]A\n[00:05.000]B\n"},
		{"plain lines", "A\nB\n", "[00:01.000]A\n[00:05.000]B\n"},
		{"lrc directly", "[00:01.000]A\n[00:05.000]B\n", "[00:01.000]A\n[00:05.000]B\n"},
		{"json array of lrc", `["[00:01.000]A","[00:05.000]B"]`, "[00:01.000]A\n[00:05.000]B\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, _ := json.Marshal(c.reply)
			s := newServer(t, `{"choices":[{"message":{"content":`+string(reply)+`}}]}`)
			cl := clientFor(t, s, Options{Provider: "openai", Key: "k", Target: "English"})
			res, err := cl.Translate(context.Background(), sourceLRC)
			if err != nil {
				t.Fatal(err)
			}
			if res.Translation != c.want {
				t.Errorf("Translation:\n got %q\nwant %q", res.Translation, c.want)
			}
		})
	}
}

// A translation that is a line out is worse than none, because it looks right.
func TestLineCountMismatchIsRefused(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"content":"{\"lines\":[\"only one\"]}"}}]}`)
	c := clientFor(t, s, Options{Provider: "openai", Key: "k", Target: "English"})

	_, err := c.Translate(context.Background(), sourceLRC)
	if err == nil {
		t.Fatal("a short answer was accepted")
	}
	if !strings.Contains(err.Error(), "1 lines for 2") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A reply about a different song must not be pasted onto this one.
func TestTimestampsThatMatchNothingAreRefused(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"content":"[00:30.000]A\n[00:40.000]B\n"}}]}`)
	c := clientFor(t, s, Options{Provider: "openai", Key: "k", Target: "English"})

	if _, err := c.Translate(context.Background(), sourceLRC); err == nil {
		t.Fatal("a reply with foreign timestamps was accepted")
	}
}

// A model that skipped a line leaves that line as it was rather than shifting
// every line after it.
func TestPartialTimedReplyKeepsTheLinesItSkipped(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"content":"[00:05.000]A secret\n"}}]}`)
	c := clientFor(t, s, Options{Provider: "openai", Key: "k", Target: "English"})

	res, err := c.Translate(context.Background(), sourceLRC)
	if err != nil {
		t.Fatal(err)
	}
	want := "[00:01.000]きみのこえ\n[00:05.000]A secret\n"
	if res.Translation != want {
		t.Errorf("Translation:\n got %q\nwant %q", res.Translation, want)
	}
}

func TestTranslateCarriesTheHeadersThrough(t *testing.T) {
	s := newServer(t, `{"choices":[{"message":{"content":"{\"lines\":[\"A\",\"B\"]}"}}]}`)
	c := clientFor(t, s, Options{Provider: "openai", Key: "k", Target: "English"})

	res, err := c.Translate(context.Background(), "[by:someone]\n"+sourceLRC)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Translation, "[by:someone]\n") {
		t.Errorf("the header was dropped:\n%s", res.Translation)
	}
}

func TestProvidersCoverTheFourVendors(t *testing.T) {
	for _, id := range []string{"openai", "claude", "gemini", "deepseek"} {
		p, ok := ProviderByID(id)
		if !ok {
			t.Fatalf("%s is missing", id)
		}
		if p.DefaultModel == "" || p.KeyURL == "" || len(p.Models) == 0 {
			t.Errorf("%s is incomplete: %+v", id, p)
		}
		if p.modelsEndpoint == "" {
			t.Errorf("%s has no model list URL", id)
		}
	}
	if len(Providers()) != len(providers) {
		t.Error("Providers() does not match the table")
	}
}

const twoLineReply = `{"choices":[{"message":{"content":"{\"lines\":[\"A\",\"B\"]}"}}]}`

// replyFor is a two-line answer in the dialect a provider reads.
func replyFor(provider string) string {
	switch provider {
	case "claude":
		return `{"content":[{"type":"text","text":"{\"lines\":[\"A\",\"B\"]}"}]}`
	case "gemini":
		return `{"candidates":[{"content":{"parts":[{"text":"{\"lines\":[\"A\",\"B\"]}"}]}}]}`
	default:
		return twoLineReply
	}
}

// The thinking controls go to DeepSeek and to nobody else. The other three
// reject a parameter they do not know, so a field that leaked into their
// request would fail every translation in the batch rather than being ignored.
func TestThinkingReachesOnlyTheProviderThatTakesIt(t *testing.T) {
	cases := []struct {
		provider string
		level    string
		// wantType is the thinking.type sent, "" for "the key is absent".
		wantType string
		wantEff  string
	}{
		{"deepseek", ThinkingDefault, "", ""},
		{"deepseek", ThinkingOff, "disabled", ""},
		{"deepseek", ThinkingLow, "enabled", "low"},
		{"deepseek", ThinkingHigh, "enabled", "high"},
		{"deepseek", ThinkingMax, "enabled", "max"},
		{"openai", ThinkingHigh, "", ""},
		{"openai", ThinkingOff, "", ""},
		{"gemini", ThinkingMax, "", ""},
	}
	for _, c := range cases {
		t.Run(c.provider+"/"+c.level, func(t *testing.T) {
			s := newServer(t, replyFor(c.provider))
			cl := clientFor(t, s, Options{
				Provider: c.provider, Key: "k", Target: "English", Thinking: c.level,
			})
			if _, err := cl.Translate(context.Background(), sourceLRC); err != nil {
				t.Fatal(err)
			}

			thinking, _ := s.body["thinking"].(map[string]any)
			gotType, _ := thinking["type"].(string)
			if gotType != c.wantType {
				t.Errorf("thinking.type = %q, want %q (body: %v)", gotType, c.wantType, s.body)
			}
			gotEff, _ := s.body["reasoning_effort"].(string)
			if gotEff != c.wantEff {
				t.Errorf("reasoning_effort = %q, want %q", gotEff, c.wantEff)
			}
			if c.wantType == "" {
				if _, ok := s.body["thinking"]; ok {
					t.Errorf("%s was sent a thinking field: %v", c.provider, s.body)
				}
			}
		})
	}
}

// Claude authenticates with a header of its own, and the list call has to use
// the same one the chat call does or every provider but OpenAI answers 401.
func TestListModelsParsesEveryDialect(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		reply    string
		want     []string
		// wantEffort is checked against the first model.
		wantEffort  []string
		wantDefault string
	}{
		{
			name:     "deepseek publishes thinking levels per model",
			provider: "deepseek",
			reply: `{"object":"list","data":[
				{"id":"deepseek-flash","object":"model","name":"DeepSeek-V4.1-Flash",
				 "context_window":1048576,"max_output_tokens":393216,
				 "effort":{"supported_levels":["low","high","max"],"default_level":"high"}},
				{"id":"deepseek-v4-pro","object":"model"}]}`,
			want:        []string{"deepseek-flash", "deepseek-v4-pro"},
			wantEffort:  []string{"low", "high", "max"},
			wantDefault: "high",
		},
		{
			name:     "openai lists bare ids",
			provider: "openai",
			reply:    `{"object":"list","data":[{"id":"gpt-4o-mini"},{"id":"gpt-4o"}]}`,
			want:     []string{"gpt-4o-mini", "gpt-4o"},
		},
		{
			name:     "claude uses display names",
			provider: "claude",
			reply: `{"data":[{"id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5"},
				{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}]}`,
			want: []string{"claude-sonnet-5-5", "claude-opus-5-5"},
		},
		{
			name:     "gemini drops the prefix and the non-generating models",
			provider: "gemini",
			reply: `{"models":[
				{"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash",
				 "supportedGenerationMethods":["generateContent","countTokens"],
				 "inputTokenLimit":1048576,"outputTokenLimit":65536},
				{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]},
				{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]}]}`,
			want: []string{"gemini-2.5-flash", "gemini-2.5-pro"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, c.reply)
			models, err := ListModels(context.Background(), ListOptions{
				Provider: c.provider, Key: "k", Endpoint: s.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range models {
				got = append(got, m.ID)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("ids = %v, want %v", got, c.want)
			}
			if c.wantEffort != nil {
				if strings.Join(models[0].EffortLevels, ",") != strings.Join(c.wantEffort, ",") {
					t.Errorf("effort levels = %v, want %v", models[0].EffortLevels, c.wantEffort)
				}
				if models[0].DefaultEffort != c.wantDefault {
					t.Errorf("default effort = %q, want %q", models[0].DefaultEffort, c.wantDefault)
				}
			}
			if !strings.Contains(s.auth, "k") {
				t.Errorf("the key was not sent: %q", s.auth)
			}
		})
	}
}

func TestListModelsRefusesWithoutAKey(t *testing.T) {
	if _, err := ListModels(context.Background(), ListOptions{Provider: "deepseek"}); err == nil {
		t.Error("a list was fetched with no key")
	}
	if _, err := ListModels(context.Background(), ListOptions{Provider: "nope", Key: "k"}); err == nil {
		t.Error("a list was fetched for an unknown provider")
	}
}

func TestListModelsReportsAVendorsRefusal(t *testing.T) {
	s := newServer(t, `{"error":{"message":"Authentication Fails"}}`)
	s.status = http.StatusUnauthorized
	_, err := ListModels(context.Background(), ListOptions{
		Provider: "deepseek", Key: "bad", Endpoint: s.URL,
	})
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !strings.Contains(err.Error(), "Authentication Fails") {
		t.Errorf("the vendor's message was lost: %v", err)
	}
}

func TestValidThinking(t *testing.T) {
	for _, level := range ThinkingLevels {
		if !ValidThinking(level) {
			t.Errorf("%q is offered but refused", level)
		}
	}
	for _, bad := range []string{"none", "medium", "HIGH", "xhigh", "ultra"} {
		if ValidThinking(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}
