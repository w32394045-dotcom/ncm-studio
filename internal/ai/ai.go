// Package ai translates lyric timelines with a language model.
//
// Four providers are supported — OpenAI, Claude, Gemini and DeepSeek — and each
// is called through its own native API rather than through a compatibility
// shim, because the two things this program needs from a reply (the text, and
// nothing but the text) are expressed differently by each of them. DeepSeek is
// the reason that matters: a reasoning model returns its chain of thought in a
// field beside the answer, and any code that reaches for the wrong field writes
// the model's private deliberations into somebody's music file.
//
// What comes back is aligned onto the timestamps the file already has. The
// model never supplies timing, so it cannot move a line: it rewrites the words
// of a timeline that is already correct.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"ncm-studio/internal/lyric"
)

// ErrNoKey is returned when a provider was chosen but no key is stored for it.
var ErrNoKey = errors.New("no API key for this provider")

// shape is which request and response dialect an endpoint speaks. The three
// dialects are not variations on a theme — the field names, the authentication
// header and the place the answer hides are all different.
type shape int

const (
	shapeOpenAI shape = iota // OpenAI, and DeepSeek, which copies it
	shapeClaude
	shapeGemini
)

// Provider is one vendor the translation tab can talk to.
type Provider struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// DefaultModel is what a fresh install uses. Models are listed newest
	// first; a model that has been retired from the list can still be typed in
	// by hand, so the list is a convenience rather than a restriction.
	DefaultModel string   `json:"defaultModel"`
	Models       []string `json:"models"`
	// KeyURL is where a user goes to make a key. It is shown next to the field
	// because it is the one thing somebody setting this up needs and cannot
	// guess.
	KeyURL string `json:"keyUrl"`
	// Thinking says this vendor's models take the thinking-effort controls, and
	// therefore that the settings panel should offer them. It is part of the
	// catalogue rather than a test for one vendor's id, because the request
	// body carrying those fields is only valid where they are understood: the
	// other three reject an unknown parameter outright.
	Thinking bool `json:"thinking"`

	shape    shape
	endpoint string
	// modelsEndpoint is where this vendor lists its models. It is not derivable
	// from the chat endpoint — Gemini's chat URL names the model in its path,
	// and DeepSeek's base URL carries no version segment — so each one is
	// spelled out.
	modelsEndpoint string
}

// Providers is the catalogue the settings panel draws itself from.
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// ProviderByID finds a provider, or returns false.
func ProviderByID(id string) (Provider, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

var providers = []Provider{
	{
		ID:           "openai",
		Label:        "OpenAI",
		DefaultModel: "gpt-4o-mini",
		Models:       []string{"gpt-4o-mini", "gpt-4o", "gpt-4.1-mini", "gpt-4.1"},
		KeyURL:       "https://platform.openai.com/api-keys",
		shape:        shapeOpenAI,
		endpoint:     "https://api.openai.com/v1/chat/completions",

		modelsEndpoint: "https://api.openai.com/v1/models",
	},
	{
		ID:           "claude",
		Label:        "Claude",
		DefaultModel: "claude-sonnet-5-5",
		Models:       []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-4-5-20251001"},
		KeyURL:       "https://console.anthropic.com/settings/keys",
		shape:        shapeClaude,
		endpoint:     "https://api.anthropic.com/v1/messages",

		modelsEndpoint: "https://api.anthropic.com/v1/models",
	},
	{
		ID:           "gemini",
		Label:        "Gemini",
		DefaultModel: "gemini-2.5-flash",
		Models:       []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.0-flash"},
		KeyURL:       "https://aistudio.google.com/apikey",
		shape:        shapeGemini,
		endpoint:     "https://generativelanguage.googleapis.com/v1beta/models/",

		modelsEndpoint: "https://generativelanguage.googleapis.com/v1beta/models",
	},
	{
		ID:           "deepseek",
		Label:        "DeepSeek",
		DefaultModel: "deepseek-chat",
		// The reasoner model is offered, but it is the one that answers with
		// its thinking beside the text; see parseOpenAI.
		Models:   []string{"deepseek-chat", "deepseek-reasoner"},
		KeyURL:   "https://platform.deepseek.com/api_keys",
		Thinking: true,
		shape:    shapeOpenAI,
		// The documented base URL has no /v1 suffix; the chat path does.
		endpoint: "https://api.deepseek.com/chat/completions",

		modelsEndpoint: "https://api.deepseek.com/models",
	},
}

// The thinking effort levels, named the way DeepSeek names them.
//
// ThinkingDefault is the empty string: it means the request says nothing about
// thinking and the provider applies its own default, which for DeepSeek is
// thinking on at high effort. That is deliberately distinct from ThinkingOff —
// "let the vendor decide" and "do not think" are different requests, and the
// second one is the expensive mistake to guess at.
const (
	ThinkingDefault = ""
	ThinkingOff     = "off"
	ThinkingLow     = "low"
	ThinkingHigh    = "high"
	ThinkingMax     = "max"
)

// ThinkingLevels is the set the settings panel offers, in the order it shows
// them. A vendor's model list can narrow this further — DeepSeek publishes the
// levels each model accepts, and does not include "off" among them because
// that is the switch rather than a level.
var ThinkingLevels = []string{ThinkingDefault, ThinkingOff, ThinkingLow, ThinkingHigh, ThinkingMax}

// ValidThinking reports whether a level is one this build knows.
//
// An unknown value is dropped rather than forwarded: the vendor refuses a
// request carrying a parameter it does not understand, so a typo that reached
// the wire would fail every translation in the batch.
func ValidThinking(level string) bool {
	for _, l := range ThinkingLevels {
		if l == level {
			return true
		}
	}
	return false
}

// Options configures a client.
type Options struct {
	// Provider and Model name what to call. An empty Model means the
	// provider's default.
	Provider string
	Model    string
	// Key is the API key. It is passed in rather than read from the settings
	// so that this package never has to know where keys are kept.
	Key string
	// Target is the language to translate into, named the way a person would
	// name it ("简体中文", "English"). It goes into the prompt verbatim.
	Target string
	// Thinking is how hard a reasoning model is asked to think: one of
	// ThinkingDefault, ThinkingOff, ThinkingLow, ThinkingHigh or ThinkingMax.
	// Only a provider with Thinking set reads it; for the others the request
	// says nothing, because they reject parameters they do not know.
	Thinking string
	// HTTP is the client to use. Nil means a client with Timeout.
	HTTP *http.Client
	// Timeout bounds one request. Zero means five minutes: a long song is a
	// few thousand tokens, and a slow model can take a while over it.
	Timeout time.Duration
	// Endpoint overrides the provider's URL. It exists for tests and for
	// somebody who reaches the API through their own gateway.
	Endpoint string
}

// Client translates one timeline at a time.
type Client struct {
	opts     Options
	provider Provider
	http     *http.Client
}

// New builds a client. The provider must be one this build knows.
func New(opts Options) (*Client, error) {
	p, ok := ProviderByID(opts.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", opts.Provider)
	}
	if strings.TrimSpace(opts.Key) == "" {
		return nil, ErrNoKey
	}
	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = p.DefaultModel
	}
	hc := opts.HTTP
	if hc == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{opts: opts, provider: p, http: hc}, nil
}

// Provider returns the provider this client calls.
func (c *Client) Provider() Provider { return c.provider }

// Model returns the model this client calls.
func (c *Client) Model() string { return c.opts.Model }

// Result is a translated timeline.
type Result struct {
	// Translation is an LRC document carrying the source's timestamps.
	Translation string
	Provider    string
	Model       string
}

// Translate rewrites the words of a timeline in the target language.
//
// The document's timing is the input's, unchanged: the parsed lines are sent as
// a numbered list and the answer is placed back onto the same timestamps, so a
// model that miscounts changes the words it was given and cannot shift a line
// to a moment where nobody is singing.
func (c *Client) Translate(ctx context.Context, doc string) (Result, error) {
	headers, lines := lyric.Parse(doc)
	if len(lines) == 0 {
		return Result{}, errors.New("there are no timed lines to translate")
	}
	target := strings.TrimSpace(c.opts.Target)
	if target == "" {
		return Result{}, errors.New("no target language is set")
	}

	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.Text
	}
	system, user, err := buildPrompt(texts, target)
	if err != nil {
		return Result{}, err
	}

	body, err := c.call(ctx, system, user)
	if err != nil {
		return Result{}, err
	}

	translated, err := align(lines, body)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Translation: lyric.Render(headers, translated),
		Provider:    c.provider.ID,
		Model:       c.opts.Model,
	}, nil
}

// call performs one request and returns the model's text.
func (c *Client) call(ctx context.Context, system, user string) (string, error) {
	url := c.opts.Endpoint
	if url == "" {
		url = c.provider.endpoint
	}
	var (
		payload []byte
		err     error
	)
	switch c.provider.shape {
	case shapeClaude:
		payload, err = json.Marshal(map[string]any{
			"model":      c.opts.Model,
			"max_tokens": 8192,
			"system":     system,
			"messages": []map[string]any{
				{"role": "user", "content": user},
			},
		})
	case shapeGemini:
		// Gemini names the model in the path rather than the body. An override
		// that already spells the whole path out is left as the user wrote it.
		if !strings.Contains(url, ":generateContent") {
			url = strings.TrimSuffix(url, "/") + "/" + c.opts.Model + ":generateContent"
		}
		payload, err = json.Marshal(map[string]any{
			"systemInstruction": map[string]any{
				"parts": []map[string]any{{"text": system}},
			},
			"contents": []map[string]any{
				{"role": "user", "parts": []map[string]any{{"text": user}}},
			},
			"generationConfig": map[string]any{"temperature": 0.2},
		})
	default: // shapeOpenAI
		body := map[string]any{
			"model": c.opts.Model,
			"messages": []map[string]any{
				{"role": "system", "content": system},
				{"role": "user", "content": user},
			},
			// Low but not zero: a translation wants to be reproducible, and
			// the sampling here is not what makes it good.
			"temperature": 0.2,
			// Without this a reasoning model may spend its whole budget
			// thinking and return an empty answer.
			"max_tokens": 8192,
			"stream":     false,
		}
		c.applyThinking(body)
		payload, err = json.Marshal(body)
	}
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, c.provider.shape, c.opts.Key)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// A reply is bounded rather than read whole: an endpoint that streams
	// without being asked, or an error page that never ends, must not be able
	// to exhaust memory. A large translation is well under a megabyte.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp.StatusCode, data)
	}

	switch c.provider.shape {
	case shapeClaude:
		return parseClaude(data)
	case shapeGemini:
		return parseGemini(data)
	default:
		return parseOpenAI(data)
	}
}

// applyThinking adds the thinking controls to an OpenAI-shaped body.
//
// It writes nothing for a provider without Thinking, and nothing for the
// default level: leaving the field out is how "let the vendor decide" is said,
// and it keeps a request to OpenAI or Gemini from carrying a parameter those
// APIs refuse. The default level is the state this program was in before the
// control existed, so an install that never touches it sends exactly what it
// always sent.
func (c *Client) applyThinking(body map[string]any) {
	if !c.provider.Thinking || c.opts.Thinking == ThinkingDefault {
		return
	}
	if c.opts.Thinking == ThinkingOff {
		body["thinking"] = map[string]string{"type": "disabled"}
		return
	}
	body["thinking"] = map[string]string{"type": "enabled"}
	body["reasoning_effort"] = c.opts.Thinking
}

// setAuth writes the header a dialect authenticates with. The three are not
// variations on one scheme: Claude wants a key header plus a version, Gemini a
// key header of its own, and everything OpenAI-shaped a bearer token.
func setAuth(req *http.Request, sh shape, key string) {
	switch sh {
	case shapeClaude:
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case shapeGemini:
		req.Header.Set("x-goog-api-key", key)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// Model is one entry of a provider's model list.
type Model struct {
	ID string `json:"id"`
	// Name is the vendor's display name for it, when it gives one.
	Name string `json:"name,omitempty"`
	// ContextWindow and MaxOutput are the vendor's own limits, in tokens.
	ContextWindow int `json:"contextWindow,omitempty"`
	MaxOutput     int `json:"maxOutput,omitempty"`
	// EffortLevels are the thinking levels this model accepts, when the vendor
	// publishes them. Empty means it did not say — not that there are none.
	EffortLevels []string `json:"effortLevels,omitempty"`
	// DefaultEffort is the level the vendor applies when none is asked for.
	DefaultEffort string `json:"defaultEffort,omitempty"`
}

// ListOptions configures a model-list call.
type ListOptions struct {
	// Provider names whose list to ask for, and the dialect to parse it in.
	Provider string
	// Key is the API key, which the list endpoints check like the chat ones do.
	Key string
	// HTTP is the client to use. Nil means a client with Timeout.
	HTTP *http.Client
	// Timeout bounds the request. Zero means thirty seconds: this is a small
	// JSON document and a vendor that takes longer than that is not answering.
	Timeout time.Duration
	// Endpoint overrides the provider's list URL. It exists for tests and for
	// somebody who reaches the API through their own gateway.
	Endpoint string
}

// ListModels asks a provider which models it offers, right now.
//
// The built-in lists are a snapshot taken when this build was made, and vendors
// retire and add models between builds. This is the way to see what a key can
// actually call, and — for DeepSeek — the only way to learn which thinking
// levels each model accepts, since that is published per model rather than
// documented once.
//
// A failure here is never fatal to anything: the model field stays free text,
// and the built-in list stays usable.
func ListModels(ctx context.Context, opts ListOptions) ([]Model, error) {
	p, ok := ProviderByID(opts.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", opts.Provider)
	}
	if strings.TrimSpace(opts.Key) == "" {
		return nil, ErrNoKey
	}
	url := opts.Endpoint
	if url == "" {
		url = p.modelsEndpoint
	}
	if url == "" {
		return nil, fmt.Errorf("%s does not publish a model list", p.Label)
	}
	hc := opts.HTTP
	if hc == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	setAuth(req, p.shape, opts.Key)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Bounded like a reply: model lists are a few kilobytes, and a gateway that
	// streams something else must not be able to exhaust memory.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp.StatusCode, data)
	}
	return parseModels(p.shape, data)
}

// parseModels reads the three list shapes.
func parseModels(sh shape, data []byte) ([]Model, error) {
	switch sh {
	case shapeClaude:
		var env struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("unreadable model list: %w", err)
		}
		out := make([]Model, 0, len(env.Data))
		for _, m := range env.Data {
			if strings.TrimSpace(m.ID) == "" {
				continue
			}
			out = append(out, Model{ID: m.ID, Name: m.DisplayName})
		}
		return out, nil

	case shapeGemini:
		var env struct {
			Models []struct {
				Name             string   `json:"name"`
				DisplayName      string   `json:"displayName"`
				Methods          []string `json:"supportedGenerationMethods"`
				InputTokenLimit  int      `json:"inputTokenLimit"`
				OutputTokenLimit int      `json:"outputTokenLimit"`
			} `json:"models"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("unreadable model list: %w", err)
		}
		out := make([]Model, 0, len(env.Models))
		for _, m := range env.Models {
			// The list holds every model the key can see, including the
			// embedding and tokenizer ones, which cannot hold a conversation.
			// Only what can generate text is offered.
			if !slices.Contains(m.Methods, "generateContent") {
				continue
			}
			id := strings.TrimPrefix(m.Name, "models/")
			if id == "" {
				continue
			}
			out = append(out, Model{
				ID:            id,
				Name:          m.DisplayName,
				ContextWindow: m.InputTokenLimit,
				MaxOutput:     m.OutputTokenLimit,
			})
		}
		return out, nil

	default: // shapeOpenAI: OpenAI, and DeepSeek, which copies it
		var env struct {
			Data []struct {
				ID            string `json:"id"`
				Name          string `json:"name"`
				ContextWindow int    `json:"context_window"`
				MaxOutput     int    `json:"max_output_tokens"`
				// DeepSeek publishes the thinking levels a model takes here,
				// next to the model rather than once for the vendor.
				Effort *struct {
					SupportedLevels []string `json:"supported_levels"`
					DefaultLevel    string   `json:"default_level"`
				} `json:"effort"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("unreadable model list: %w", err)
		}
		out := make([]Model, 0, len(env.Data))
		for _, m := range env.Data {
			if strings.TrimSpace(m.ID) == "" {
				continue
			}
			model := Model{
				ID:            m.ID,
				Name:          m.Name,
				ContextWindow: m.ContextWindow,
				MaxOutput:     m.MaxOutput,
			}
			if m.Effort != nil {
				model.EffortLevels = m.Effort.SupportedLevels
				model.DefaultEffort = m.Effort.DefaultLevel
			}
			out = append(out, model)
		}
		return out, nil
	}
}

// apiError turns a vendor's error body into an error a person can act on.
// Getting this wrong is what produces "translation failed" with no clue in it.
func apiError(status int, data []byte) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error.Message != "" {
		msg := envelope.Error.Message
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return fmt.Errorf("the key was refused (%d): %s", status, msg)
		}
		if status == http.StatusTooManyRequests {
			return fmt.Errorf("the provider is rate limiting (%d): %s", status, msg)
		}
		return fmt.Errorf("%s (%d): %s", http.StatusText(status), status, msg)
	}
	snippet := strings.TrimSpace(string(data))
	if len(snippet) > 200 {
		snippet = snippet[:200] + "…"
	}
	if snippet == "" {
		snippet = "no message"
	}
	return fmt.Errorf("%s (%d): %s", http.StatusText(status), status, snippet)
}

// parseOpenAI reads an OpenAI-shaped reply, which is also DeepSeek's shape.
//
// Only choices[0].message.content is read. A reasoning model puts its chain of
// thought in message.reasoning_content, and that field is not a translation:
// writing it into a file would publish the model's deliberations as somebody's
// lyrics. It is ignored here on purpose, and a reply that carries reasoning but
// no content is an error rather than something to fall back on.
func parseOpenAI(data []byte) (string, error) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				// Declared so the field can be recognised when it is the only
				// thing present — never read as an answer.
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("unreadable reply: %w", err)
	}
	if envelope.Error != nil && envelope.Error.Message != "" {
		return "", errors.New(envelope.Error.Message)
	}
	if len(envelope.Choices) == 0 {
		return "", errors.New("the reply held no answer")
	}
	choice := envelope.Choices[0]
	if text := strings.TrimSpace(choice.Message.Content); text != "" {
		return text, nil
	}
	if strings.TrimSpace(choice.Message.ReasoningContent) != "" {
		// The honest failure: the model thought but never answered. Falling
		// back to the reasoning would write a page of deliberation where the
		// lyrics belong.
		return "", errors.New("the model returned only its reasoning and no translation")
	}
	if choice.FinishReason == "length" {
		return "", errors.New("the model ran out of room before answering; try a shorter song or a larger model")
	}
	return "", errors.New("the model returned an empty answer")
}

// parseClaude joins the text blocks of a Messages reply, ignoring every other
// kind of block.
func parseClaude(data []byte) (string, error) {
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("unreadable reply: %w", err)
	}
	if envelope.Error != nil && envelope.Error.Message != "" {
		return "", errors.New(envelope.Error.Message)
	}
	var parts []string
	for _, block := range envelope.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		if envelope.StopReason == "max_tokens" {
			return "", errors.New("the model ran out of room before answering; try a shorter song or a larger model")
		}
		return "", errors.New("the model returned an empty answer")
	}
	return strings.Join(parts, "\n"), nil
}

// parseGemini joins the text parts of the first candidate.
func parseGemini(data []byte) (string, error) {
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("unreadable reply: %w", err)
	}
	if envelope.Error != nil && envelope.Error.Message != "" {
		return "", errors.New(envelope.Error.Message)
	}
	if envelope.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("the provider refused the text (%s)", envelope.PromptFeedback.BlockReason)
	}
	if len(envelope.Candidates) == 0 {
		return "", errors.New("the reply held no answer")
	}
	var parts []string
	for _, p := range envelope.Candidates[0].Content.Parts {
		if strings.TrimSpace(p.Text) != "" {
			parts = append(parts, p.Text)
		}
	}
	if len(parts) == 0 {
		if envelope.Candidates[0].FinishReason == "MAX_TOKENS" {
			return "", errors.New("the model ran out of room before answering; try a shorter song or a larger model")
		}
		return "", errors.New("the model returned an empty answer")
	}
	return strings.Join(parts, "\n"), nil
}
