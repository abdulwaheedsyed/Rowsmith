// Package ai is Rowsmith's SQL assistant: a language model, given the schema
// of the connection in use and read-only tools, helps write, explain, fix
// and speed up queries. It never changes data; people run what it suggests
// from the editor, where the usual confirmations apply.
//
// Providers: Anthropic's Claude through the official SDK, or any service
// that speaks the OpenAI chat completions API (OpenAI, OpenRouter, Ollama,
// LM Studio, vLLM and others), so teams can bring the account they have.
package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
)

// Provider names stored in the settings.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai" // any OpenAI-compatible endpoint
)

// Config is what an admin chose in the settings.
type Config struct {
	Provider   string
	BaseURL    string // OpenAI-compatible endpoint, e.g. https://openrouter.ai/api/v1
	APIKey     string // optional for local endpoints such as Ollama
	Model      string
	Effort     string // Anthropic: low, medium, high
	Data       bool   // sample rows and read-only queries allowed
	Production bool   // ... on production connections too
}

// Ready reports whether the configuration can make requests.
func (c Config) Ready() bool {
	if c.Provider == ProviderOpenAI {
		return c.BaseURL != "" && c.Model != ""
	}
	return c.APIKey != ""
}

func (c Config) fingerprint() string {
	return strings.Join([]string{c.Provider, c.BaseURL, c.Model, c.APIKey}, "\x00")
}

// AnthropicModels are offered for the Anthropic provider, most capable first.
var AnthropicModels = []struct{ ID, Name string }{
	{"claude-opus-5-5", "Claude Opus 5.5"},
	{"claude-sonnet-5-5", "Claude Sonnet 5.5"},
	{"claude-haiku-4-5", "Claude Haiku 4.5"},
}

const (
	DefaultModel  = "claude-opus-5-5"
	maxToolRounds = 10
	maxTurns      = 40
	convIdle      = 2 * time.Hour
)

// ---- provider-neutral turn machinery ------------------------------------------------

type toolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type toolResult struct {
	ID      string
	Text    string
	IsError bool
}

type usage struct{ Input, Output, CacheRead int64 }

type stepResult struct {
	Calls   []toolCall // tools to run before the next step; empty when the turn is over
	Stop    string     // "end", "length", "refusal"
	Refusal string
	Model   string
	Usage   usage
}

// engine keeps a conversation in its provider's own format, so replayed
// history (thinking signatures, tool-call ids) round-trips exactly.
type engine interface {
	addUser(text string)
	addToolResults(results []toolResult)
	step(ctx context.Context, emit Emit) (stepResult, error)
	mark() int
	rollback(to int)
}

// Assistant holds conversations in memory; they end with the process.
type Assistant struct {
	mu    sync.Mutex
	convs map[string]*conversation
}

type conversation struct {
	mu     sync.Mutex
	id     string
	user   string
	conn   string
	scope  driver.Scope
	config string
	engine engine
	turns  int
	last   time.Time
	busy   bool
}

func New() *Assistant {
	a := &Assistant{convs: map[string]*conversation{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			a.mu.Lock()
			for id, c := range a.convs {
				if time.Since(c.last) > convIdle {
					delete(a.convs, id)
				}
			}
			a.mu.Unlock()
		}
	}()
	return a
}

// Request is one message from the person, with the editor state around it.
type Request struct {
	UserID       string
	ConnectionID string
	Conversation string
	Message      string
	Context      EditorContext
	Environment  string // the connection's environment label
}

// EditorContext is what the person is looking at.
type EditorContext struct {
	Statement string `json:"statement"`
	Selection string `json:"selection"`
	Script    string `json:"script"`
	Error     string `json:"error"`
	Plan      string `json:"plan"`
	Line      int    `json:"line"`
}

// Summary describes a finished turn, for the audit log.
type Summary struct {
	Provider     string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	Tools        []string
}

// Emit sends an event to the browser.
type Emit func(event map[string]any)

// Target is the connection a turn works against.
type Target struct {
	Conn    driver.Conn
	Info    driver.Info
	Server  *driver.ServerInfo
	Scope   driver.Scope
	Session func(ctx context.Context) (driver.Session, error) // read-only console session
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// conversationFor returns the person's conversation, or a new one when there
// is none, it belongs to another connection, scope or provider setup, or it
// is full.
func (a *Assistant) conversationFor(req Request, scope driver.Scope, cfg Config) (*conversation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fp := cfg.fingerprint()
	if c, ok := a.convs[req.Conversation]; ok && c.user == req.UserID && c.conn == req.ConnectionID && c.scope == scope && c.config == fp && c.turns < maxTurns {
		return c, false
	}
	n := 0
	var oldest *conversation
	for _, c := range a.convs {
		if c.user == req.UserID {
			n++
			if oldest == nil || c.last.Before(oldest.last) {
				oldest = c
			}
		}
	}
	if n >= 30 {
		delete(a.convs, oldest.id)
	}
	c := &conversation{id: newID(), user: req.UserID, conn: req.ConnectionID, scope: scope, config: fp, last: time.Now()}
	a.convs[c.id] = c
	return c, true
}

// Forget ends a conversation.
func (a *Assistant) Forget(user, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.convs[id]; ok && c.user == user {
		delete(a.convs, id)
	}
}

func newEngine(cfg Config, p prompt, tools []toolSpec) engine {
	if cfg.Provider == ProviderOpenAI {
		return newOpenAIEngine(cfg, p, tools)
	}
	return newAnthropicEngine(cfg, p, tools)
}

// Ask runs one turn: it streams the answer (and any tool use) through emit.
func (a *Assistant) Ask(ctx context.Context, cfg Config, t Target, req Request, emit Emit) (*Summary, error) {
	if strings.TrimSpace(req.Message) == "" {
		return nil, errors.New("type a question first")
	}
	if cfg.Provider == "" {
		cfg.Provider = ProviderAnthropic
	}
	if cfg.Provider == ProviderAnthropic && cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	conv, fresh := a.conversationFor(req, t.Scope, cfg)
	conv.mu.Lock()
	if conv.busy {
		conv.mu.Unlock()
		return nil, errors.New("the assistant is still answering in this conversation")
	}
	conv.busy = true
	conv.mu.Unlock()
	defer func() {
		conv.mu.Lock()
		conv.busy = false
		conv.last = time.Now()
		conv.mu.Unlock()
	}()
	emit(map[string]any{"t": "conversation", "id": conv.id, "new": fresh})

	data := cfg.Data && (req.Environment != "production" || cfg.Production)
	env := &toolEnv{conn: t.Conn, info: t.Info, scope: t.Scope, data: data, openSess: t.Session}
	defer env.close()
	if conv.engine == nil {
		conv.engine = newEngine(cfg, systemPrompt(ctx, t, req.Environment, data, true), env.definitions())
	}
	eng := conv.engine
	start := eng.mark()
	eng.addUser(userTurn(req))
	conv.turns++

	sum := &Summary{Provider: cfg.Provider, Model: cfg.Model}
	for round := 0; round < maxToolRounds; round++ {
		res, err := eng.step(ctx, emit)
		if err != nil {
			// The turn did not finish: drop it so the history stays valid.
			eng.rollback(start)
			conv.turns--
			return sum, err
		}
		sum.InputTokens += res.Usage.Input
		sum.OutputTokens += res.Usage.Output
		sum.CacheRead += res.Usage.CacheRead
		if res.Model != "" {
			sum.Model = res.Model
		}
		if len(res.Calls) > 0 {
			results := make([]toolResult, 0, len(res.Calls))
			for _, call := range res.Calls {
				emit(map[string]any{"t": "tool", "id": call.ID, "name": call.Name, "input": call.Input})
				text, err := env.run(ctx, call.Name, call.Input)
				if err != nil {
					emit(map[string]any{"t": "toolResult", "id": call.ID, "ok": false, "text": clip(err.Error(), 400)})
					results = append(results, toolResult{ID: call.ID, Text: "Error: " + err.Error(), IsError: true})
					continue
				}
				emit(map[string]any{"t": "toolResult", "id": call.ID, "ok": true, "text": clip(text, 600)})
				results = append(results, toolResult{ID: call.ID, Text: text})
			}
			sum.Tools = append(sum.Tools, env.calls...)
			env.calls = nil
			eng.addToolResults(results)
			continue
		}
		switch res.Stop {
		case "refusal":
			emit(map[string]any{"t": "refusal", "text": res.Refusal})
		case "length":
			emit(map[string]any{"t": "notice", "text": "The answer reached its length limit."})
		}
		emit(map[string]any{"t": "done", "model": sum.Model, "usage": map[string]int64{"input": sum.InputTokens, "output": sum.OutputTokens, "cacheRead": sum.CacheRead}})
		return sum, nil
	}
	emit(map[string]any{"t": "notice", "text": "The assistant stopped after looking things up for too long. Ask a narrower question."})
	emit(map[string]any{"t": "done", "model": sum.Model})
	return sum, nil
}

// Check sends a one-line request to verify the provider settings.
func Check(ctx context.Context, cfg Config) (model, reply string, err error) {
	if cfg.Provider == "" {
		cfg.Provider = ProviderAnthropic
	}
	if cfg.Provider == ProviderAnthropic && cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	cfg.Effort = "low"
	eng := newEngine(cfg, prompt{Instructions: "You are checking a connection. Reply with the single word: OK"}, nil)
	eng.addUser("Reply with OK.")
	var b strings.Builder
	res, err := eng.step(ctx, func(ev map[string]any) {
		if ev["t"] == "text" {
			b.WriteString(ev["text"].(string))
		}
	})
	if err != nil {
		return "", "", err
	}
	return strOr(res.Model, cfg.Model), clip(strings.TrimSpace(b.String()), 80), nil
}
