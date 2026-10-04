package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"ncm-studio/internal/ai"
	"ncm-studio/internal/backfill"
	"ncm-studio/internal/lyric"
	"ncm-studio/internal/tag"
)

// handleAIState answers for the translation pool; see poolState.
func (s *Server) handleAIState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, stateOf(s.ai))
}

func (s *Server) handleAIEvents(w http.ResponseWriter, r *http.Request) {
	s.streamPool(w, r, s.ai)
}

// handleAIStart queues a translation batch.
//
// Force applies to the whole batch — a re-run is usually a decision about a
// folder, because the model was changed or the language was — and ForcePaths
// names the rows a person ticked one by one. The two exist together because
// the standing setting can be "skip what is already translated" while a single
// row is still worth redoing, and a checkbox on that row is exactly that
// decision; without the per-path half the tick would do nothing.
func (s *Server) handleAIStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths      []string `json:"paths"`
		Force      bool     `json:"force"`
		ForcePaths []string `json:"forcePaths"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	items := queuePaths(req.Paths)
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "no files selected")
		return
	}
	forced := make(map[string]bool, len(req.ForcePaths))
	for _, p := range req.ForcePaths {
		if abs, err := filepath.Abs(p); err == nil {
			forced[abs] = true
		}
	}
	for _, it := range items {
		it.Force = req.Force || forced[it.Path]
	}

	// Checked before the run rather than during it, so a page that would do
	// nothing says so instead of starting a progress bar that immediately
	// fills with skips.
	cfg := s.store.Config()
	if strings.TrimSpace(cfg.AITarget) == "" {
		writeError(w, http.StatusBadRequest, "choose a target language first")
		return
	}
	provider := cfg.AIProvider
	if strings.TrimSpace(provider) == "" {
		provider = "deepseek"
	}
	if !s.store.HasAIKey(provider) {
		writeError(w, http.StatusBadRequest, "no API key is stored for %s", provider)
		return
	}

	s.ai.Start(items)

	// Remember the model this batch is about to use — picked from a list or
	// typed by hand, it makes no difference. What the user wants back when they
	// next open the page is the model that actually ran.
	effective := cfg.AIModel
	if strings.TrimSpace(effective) == "" {
		if p, ok := ai.ProviderByID(provider); ok {
			effective = p.DefaultModel
		}
	}
	if effective != cfg.AIModels[provider] {
		if err := s.store.RememberAIModel(provider, effective); err != nil {
			log.Printf("remember model for %s: %v", provider, err)
		}
	}

	log.Printf("started a translation batch of %d file(s) with %s", len(items), provider)
	writeJSON(w, http.StatusOK, map[string]int{"queued": len(items)})
}

func (s *Server) handleAICancel(w http.ResponseWriter, r *http.Request) {
	s.ai.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

// aiProviderView is one provider as the settings panel needs it.
type aiProviderView struct {
	ai.Provider
	// HasKey says whether a key is stored. The key itself is never sent: the
	// settings file is served to the browser on every page load, and a secret
	// that travels to the page is readable by anything that can reach the UI.
	HasKey bool `json:"hasKey"`
	// Active marks the provider the next batch will use.
	Active bool `json:"active"`
}

type aiSettingsView struct {
	Providers []aiProviderView `json:"providers"`
	Provider  string           `json:"provider"`
	Model     string           `json:"model"`
	Target    string           `json:"target"`
	// Targets are the languages offered as one-tap choices. Any language can
	// be typed instead; these are the ones this library actually holds.
	Targets []string `json:"targets"`
	// Thinking is the effort level in force, and ThinkingLevels the ones the
	// panel offers. Both travel together: a level is only meaningful to a
	// provider whose models take it, and the page asks the provider's own
	// "thinking" flag before drawing the control at all.
	Thinking       string   `json:"thinking"`
	ThinkingLevels []string `json:"thinkingLevels"`
	// LastModel is the model each provider was last used with, so switching
	// provider in the panel lands on that one rather than on the default again.
	LastModel map[string]string `json:"lastModel,omitempty"`
	// Known is the model list last fetched from each provider, so the
	// suggestions survive a reload without asking the vendor again — and so a
	// provider the panel switches back to still offers what its key can call.
	Known map[string][]string `json:"known,omitempty"`
	// Configured is false when a batch would refuse to start, which is what
	// the page shows a first-run hint for.
	Configured bool `json:"configured"`
}

// aiTargets are the languages offered by name.
//
// Named the way a translator would be asked for them, in the languages this UI
// speaks, because the string goes into the prompt verbatim and a model handles
// "简体中文" better than "zh-CN".
var aiTargets = []string{
	"简体中文", "繁體中文", "English", "日本語", "한국어",
	"Français", "Deutsch", "Español", "Русский", "Português",
}

func (s *Server) handleAIProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.aiSettings())
}

func (s *Server) aiSettings() aiSettingsView {
	cfg := s.store.Config()
	provider := cfg.AIProvider
	if strings.TrimSpace(provider) == "" {
		provider = "deepseek"
	}
	view := aiSettingsView{
		Provider:       provider,
		Model:          cfg.AIModel,
		Target:         cfg.AITarget,
		Targets:        aiTargets,
		Thinking:       cfg.AIThinking,
		ThinkingLevels: ai.ThinkingLevels,
		LastModel:      cfg.AIModels,
		Known:          cfg.AIModelList,
	}
	for _, p := range ai.Providers() {
		view.Providers = append(view.Providers, aiProviderView{
			Provider: p,
			HasKey:   s.store.HasAIKey(p.ID),
			Active:   p.ID == provider,
		})
	}
	view.Configured = strings.TrimSpace(cfg.AITarget) != "" && s.store.HasAIKey(provider)
	return view
}

// handleAIKey stores or clears one provider's key.
//
// The store writes it to a file of its own with owner-only permissions; the
// reply says only whether a key is now held, never what it is.
func (s *Server) handleAIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if _, ok := ai.ProviderByID(req.Provider); !ok {
		writeError(w, http.StatusBadRequest, "unknown provider %q", req.Provider)
		return
	}
	if err := s.store.SetAIKey(req.Provider, req.Key); err != nil {
		writeError(w, http.StatusInternalServerError, "save key: %v", err)
		return
	}
	log.Printf("a key was %s for %s", map[bool]string{true: "stored", false: "cleared"}[strings.TrimSpace(req.Key) != ""], req.Provider)
	writeJSON(w, http.StatusOK, s.aiSettings())
}

// handleAIModels asks a provider which models its key can call.
//
// The built-in lists are a snapshot from when this build was made, and vendors
// add and retire models between builds. Asking costs one request and answers
// the two questions the panel cannot answer on its own: what a key can actually
// call today, and — for DeepSeek — which thinking levels each model takes.
//
// A failure is never fatal: the field stays free text and the built-in list is
// still there, so the page reports the vendor's message and leaves everything
// as it was.
func (s *Server) handleAIModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = s.store.Config().AIProvider
	}
	if strings.TrimSpace(provider) == "" {
		provider = "deepseek"
	}
	key := s.store.AIKey(provider)
	if key == "" {
		writeError(w, http.StatusBadRequest, "no API key is stored for %s", provider)
		return
	}

	models, err := ai.ListModels(r.Context(), ai.ListOptions{Provider: provider, Key: key})
	if err != nil {
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}

	// Cached so the suggestion list is still there after a reload. The ids are
	// what the datalist needs; the rest — the vendor's display names and the
	// per-model effort levels — goes to the page that asked, which is the only
	// thing that can use it.
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if err := s.store.RememberAIModelList(provider, ids); err != nil {
		log.Printf("remember model list for %s: %v", provider, err)
	}
	log.Printf("listed %d model(s) for %s", len(models), provider)
	writeJSON(w, http.StatusOK, map[string]any{"provider": provider, "models": models})
}

// aiPreview is what a translation of one file would cost and produce.
type aiPreview struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Source is the timeline that would be sent, and Lines how many of them.
	Source string `json:"source"`
	From   string `json:"from"`
	Lines  int    `json:"lines"`
	// Already is a translation the file already carries, when it does.
	Already  string `json:"already,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Target   string `json:"target"`
	// Ready is false when the batch would skip this file, with Reason saying
	// why — so a page can say it before the user spends a request on it.
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// handleAIPreview shows what would be sent for one file, without sending it.
//
// Translating a library costs real money per request, and the two ways to waste
// it are the ones this answers: a file with nothing to translate, and a file
// that already carries a translation.
func (s *Server) handleAIPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "no path")
		return
	}

	cfg := s.store.Config()
	provider := cfg.AIProvider
	if strings.TrimSpace(provider) == "" {
		provider = "deepseek"
	}
	out := aiPreview{Path: req.Path, Provider: provider, Target: cfg.AITarget}
	if p, ok := ai.ProviderByID(provider); ok {
		out.Model = cfg.AIModel
		if out.Model == "" {
			out.Model = p.DefaultModel
		}
	}

	info, err := tag.Inspect(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	out.Name = filepath.Base(req.Path)
	source, from := backfill.TranslationSource(info)
	out.Source, out.From = source, from
	out.Already = info.Lyrics.Translation
	_, lines := lyric.Parse(source)
	out.Lines = len(lines)

	switch {
	case strings.TrimSpace(source) == "":
		out.Reason = "this file has no lyrics to translate"
	case info.Lyrics.MachineTranslated() && !cfg.AITranslated.Redoes():
		// The preview describes what a run would do, so it follows the same
		// standing rule the run does. Under the setting that redoes such a
		// file, "already translated" is not a reason to stop.
		out.Reason = "this file already carries a machine translation"
	case strings.TrimSpace(out.Target) == "":
		out.Reason = "no target language is set"
	case !s.store.HasAIKey(provider):
		out.Reason = "no API key is stored for " + provider
	default:
		out.Ready = true
	}
	writeJSON(w, http.StatusOK, out)
}
