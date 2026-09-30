package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"rowsmith/internal/ai"
	"rowsmith/internal/driver"
	"rowsmith/internal/store"
)

// aiConfig reads the assistant settings. For Anthropic the API key may also
// come from ANTHROPIC_API_KEY.
func (s *Server) aiConfig(r *http.Request) (ai.Config, bool) {
	get := func(k string) string {
		v, _, _ := s.store.Setting(r.Context(), k)
		return v
	}
	cfg := ai.Config{
		Provider:   get("ai.provider"),
		BaseURL:    get("ai.base_url"),
		APIKey:     s.SecretSetting(r, "ai.api_key"),
		Model:      get("ai.model"),
		Effort:     get("ai.effort"),
		Data:       get("ai.data") == "data",
		Production: get("ai.production") == "true",
	}
	if cfg.Provider != ai.ProviderOpenAI {
		cfg.Provider, cfg.BaseURL = ai.ProviderAnthropic, ""
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		if cfg.Model == "" || !strings.HasPrefix(cfg.Model, "claude-") {
			cfg.Model = ai.DefaultModel
		}
	}
	return cfg, get("ai.enabled") == "true" && cfg.Ready()
}

// providerName is how the settings describe where requests go.
func providerName(cfg ai.Config) string {
	if cfg.Provider != ai.ProviderOpenAI {
		return "Anthropic"
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return cfg.BaseURL
	}
	switch h := u.Hostname(); {
	case h == "api.openai.com":
		return "OpenAI"
	case strings.HasSuffix(h, "openrouter.ai"):
		return "OpenRouter"
	default:
		return u.Host
	}
}

func (s *Server) aiStatus(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	cfg, on := s.aiConfig(r)
	name := cfg.Model
	for _, m := range ai.AnthropicModels {
		if m.ID == cfg.Model {
			name = m.Name
		}
	}
	out := map[string]any{"enabled": on, "ready": cfg.Ready(), "provider": cfg.Provider, "providerName": providerName(cfg),
		"model": cfg.Model, "modelName": name, "data": cfg.Data, "production": cfg.Production, "anthropicModels": ai.AnthropicModels}
	if rc.user.Role.AtLeast(store.RoleAdmin) {
		out["envKey"] = os.Getenv("ANTHROPIC_API_KEY") != ""
	}
	writeJSON(w, 200, out)
}

// aiDraft is the settings form before it is saved; an empty key means the
// saved one.
type aiDraft struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"baseUrl"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
}

func (s *Server) draftConfig(r *http.Request, d aiDraft) (ai.Config, error) {
	cfg := ai.Config{Provider: d.Provider, BaseURL: strings.TrimSpace(d.BaseURL), APIKey: d.APIKey, Model: strings.TrimSpace(d.Model)}
	if cfg.Provider == ai.ProviderOpenAI {
		if err := validEndpoint(cfg.BaseURL); err != nil {
			return cfg, err
		}
	} else {
		cfg.Provider, cfg.BaseURL = ai.ProviderAnthropic, ""
	}
	// The saved key only ever goes back to the endpoint it was saved for.
	if saved, _ := s.aiConfig(r); cfg.APIKey == "" && sameEndpoint(saved, cfg) {
		cfg.APIKey = saved.APIKey
	}
	return cfg, nil
}

func sameEndpoint(a, b ai.Config) bool {
	return a.Provider == b.Provider && strings.TrimRight(a.BaseURL, "/") == strings.TrimRight(b.BaseURL, "/")
}

func validEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("enter the endpoint as a full URL, e.g. https://openrouter.ai/api/v1")
	}
	return nil
}

func (s *Server) aiModels(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	var d aiDraft
	if err := readJSON(r, &d); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg, err := s.draftConfig(r, d)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := ai.ListModels(ctx, cfg)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

func (s *Server) aiCheck(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	var d aiDraft
	if err := readJSON(r, &d); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg, err := s.draftConfig(r, d)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !cfg.Ready() {
		writeErr(w, 400, map[bool]string{true: "choose a model first", false: "enter an API key first"}[cfg.Provider == ai.ProviderOpenAI])
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	model, reply, err := ai.Check(ctx, cfg)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "model": model, "reply": reply})
}

type aiReq struct {
	Conversation string           `json:"conversation"`
	Message      string           `json:"message"`
	Database     string           `json:"database"`
	Schema       string           `json:"schema"`
	Context      ai.EditorContext `json:"context"`
}

func (s *Server) wsAsk(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req aiReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg, on := s.aiConfig(r)
	if !on {
		writeErr(w, 403, "the AI assistant is turned off; an admin can turn it on under Administration → AI assistant")
		return
	}
	if ok, _ := s.aiLimit.Allow(rc.user.ID); !ok {
		writeErr(w, 429, "too many questions in a short time; wait a moment")
		return
	}
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	openCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	lease, err := s.sessions.Acquire(openCtx, c, true) // the assistant only ever reads
	cancel()
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer lease.Release()
	scope := driver.Scope{Database: req.Database, Schema: req.Schema}
	sctx, scancel := context.WithTimeout(r.Context(), driver.CatalogTimeout)
	server, _ := lease.Conn.Server(sctx)
	scancel()
	target := ai.Target{Conn: lease.Conn, Info: lease.Driver.Info(), Server: server, Scope: scope,
		Session: func(ctx context.Context) (driver.Session, error) { return lease.Conn.NewSession(ctx, scope) }}

	p := newProgress(w)
	defer p.close()
	sum, err := s.ai.Ask(r.Context(), cfg, target, ai.Request{
		UserID: rc.user.ID, ConnectionID: c.ID, Conversation: req.Conversation, Message: req.Message,
		Context: req.Context, Environment: c.Environment,
	}, func(ev map[string]any) { p.send(ev) })
	if err != nil {
		p.send(map[string]any{"t": "error", "message": err.Error()})
	}
	if sum != nil && (sum.InputTokens > 0 || sum.OutputTokens > 0) {
		s.audit(context.WithoutCancel(r.Context()), rc, "ai.asked", c.Name, map[string]any{
			"model": sum.Model, "inputTokens": sum.InputTokens, "outputTokens": sum.OutputTokens,
			"cacheReadTokens": sum.CacheRead, "tools": sum.Tools})
	}
}

func (s *Server) aiForget(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Conversation string `json:"conversation"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.ai.Forget(rc.user.ID, req.Conversation)
	writeJSON(w, 200, map[string]any{"ok": true})
}
