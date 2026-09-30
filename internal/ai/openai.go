package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// openaiEngine speaks the OpenAI chat completions API with streaming, which
// OpenAI, OpenRouter, Ollama, LM Studio, vLLM and many gateways accept.
// Servers differ in the details, so it degrades gracefully: without tools
// when the model cannot call them, without usage when that is rejected.
type openaiEngine struct {
	cfg        Config
	system     string
	tools      []toolSpec
	messages   []oaMessage
	noTools    bool
	noUsage    bool
	httpClient *http.Client
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func strPtr(s string) *string { return &s }

func newOpenAIEngine(cfg Config, p prompt, tools []toolSpec) *openaiEngine {
	return &openaiEngine{cfg: cfg, system: p.text(), tools: tools, httpClient: &http.Client{}}
}

func (e *openaiEngine) mark() int       { return len(e.messages) }
func (e *openaiEngine) rollback(to int) { e.messages = e.messages[:to] }
func (e *openaiEngine) addUser(text string) {
	e.messages = append(e.messages, oaMessage{Role: "user", Content: strPtr(text)})
}

func (e *openaiEngine) addToolResults(results []toolResult) {
	for _, r := range results {
		e.messages = append(e.messages, oaMessage{Role: "tool", ToolCallID: r.ID, Content: strPtr(r.Text)})
	}
}

func endpoint(base, path string) string { return strings.TrimRight(base, "/") + path }

func (e *openaiEngine) headers(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	if u, err := url.Parse(e.cfg.BaseURL); err == nil && strings.HasSuffix(u.Hostname(), "openrouter.ai") {
		req.Header.Set("X-Title", "Rowsmith") // OpenRouter's app attribution
	}
}

func (e *openaiEngine) body() map[string]any {
	system := e.system
	if e.noTools && len(e.tools) > 0 {
		system += "\n- You have no tools in this setup: rely on the schema above, and write queries the person can run to check anything else.\n"
	}
	msgs := append([]oaMessage{{Role: "system", Content: strPtr(system)}}, e.messages...)
	body := map[string]any{"model": e.cfg.Model, "messages": msgs, "stream": true}
	if !e.noUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(e.tools) > 0 && !e.noTools {
		tools := make([]map[string]any, len(e.tools))
		for i, t := range e.tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.schema()}}
		}
		body["tools"] = tools
	}
	return body
}

type oaChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          *string `json:"content"`
			Reasoning        *string `json:"reasoning"`         // OpenRouter
			ReasoningContent *string `json:"reasoning_content"` // DeepSeek, vLLM, LM Studio
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (e *openaiEngine) step(ctx context.Context, emit Emit) (stepResult, error) {
	for attempt := 0; ; attempt++ {
		res, retry, err := e.try(ctx, emit)
		if retry && attempt < 2 {
			continue
		}
		return res, err
	}
}

// try makes one request. retry is set when it switched off a feature the
// server rejected and nothing was streamed yet.
func (e *openaiEngine) try(ctx context.Context, emit Emit) (stepResult, bool, error) {
	payload, _ := json.Marshal(e.body())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(e.cfg.BaseURL, "/chat/completions"), bytes.NewReader(payload))
	if err != nil {
		return stepResult{}, false, fmt.Errorf("invalid endpoint: %w", err)
	}
	e.headers(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return stepResult{}, false, errors.New("stopped")
		}
		return stepResult{}, false, fmt.Errorf("could not reach %s: %v", e.cfg.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg := errorMessage(resp)
		low := strings.ToLower(msg)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusNotFound {
			switch {
			case !e.noTools && len(e.tools) > 0 && (strings.Contains(low, "tool") || strings.Contains(low, "function")):
				e.noTools = true // the model cannot call tools; answer from the schema
				return stepResult{}, true, nil
			case !e.noUsage && strings.Contains(low, "stream_options"):
				e.noUsage = true
				return stepResult{}, true, nil
			}
		}
		return stepResult{}, false, statusError(resp.StatusCode, msg)
	}

	var text strings.Builder
	type pending struct{ id, name, args string }
	var calls []*pending
	res := stepResult{Stop: "end"}
	finish := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch oaChunk
		if json.Unmarshal([]byte(data), &ch) != nil {
			continue
		}
		if ch.Error != nil {
			return stepResult{}, false, fmt.Errorf("the provider reported: %s", clip(ch.Error.Message, 300))
		}
		if ch.Model != "" {
			res.Model = ch.Model
		}
		if ch.Usage != nil {
			res.Usage.Input, res.Usage.Output = ch.Usage.PromptTokens, ch.Usage.CompletionTokens
			if ch.Usage.PromptTokensDetails != nil {
				res.Usage.CacheRead = ch.Usage.PromptTokensDetails.CachedTokens
			}
		}
		for _, c := range ch.Choices {
			d := c.Delta
			if d.Content != nil && *d.Content != "" {
				text.WriteString(*d.Content)
				emit(map[string]any{"t": "text", "text": *d.Content})
			}
			for _, r := range []*string{d.Reasoning, d.ReasoningContent} {
				if r != nil && *r != "" {
					emit(map[string]any{"t": "thinking", "text": *r})
				}
			}
			for _, tc := range d.ToolCalls {
				for len(calls) <= tc.Index {
					calls = append(calls, &pending{})
				}
				p := calls[tc.Index]
				if tc.ID != "" {
					p.id = tc.ID
				}
				p.name += tc.Function.Name
				p.args += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return stepResult{}, false, errors.New("stopped")
		}
		return stepResult{}, false, fmt.Errorf("the answer was cut off: %v", err)
	}

	msg := oaMessage{Role: "assistant"}
	if text.Len() > 0 || len(calls) == 0 {
		msg.Content = strPtr(text.String())
	}
	for i, p := range calls {
		if p.name == "" {
			continue
		}
		if p.id == "" {
			p.id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i) // some servers send none
		}
		if strings.TrimSpace(p.args) == "" {
			p.args = "{}"
		}
		tc := oaToolCall{ID: p.id, Type: "function"}
		tc.Function.Name, tc.Function.Arguments = p.name, p.args
		msg.ToolCalls = append(msg.ToolCalls, tc)
		res.Calls = append(res.Calls, toolCall{ID: p.id, Name: p.name, Input: json.RawMessage(p.args)})
	}
	e.messages = append(e.messages, msg)
	switch finish {
	case "length":
		res.Stop = "length"
	case "content_filter":
		res.Stop, res.Refusal = "refusal", "The provider's content filter stopped this answer."
	}
	if len(res.Calls) > 0 {
		res.Stop = ""
	}
	return res, false, nil
}

func errorMessage(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch x := e.Error.(type) {
		case string:
			return x
		case map[string]any:
			if m, ok := x["message"].(string); ok {
				return m
			}
		}
	}
	return strings.TrimSpace(string(raw))
}

// ListModels returns the models an OpenAI-compatible endpoint offers.
func ListModels(ctx context.Context, cfg Config) ([]string, error) {
	if cfg.Provider != ProviderOpenAI {
		out := make([]string, len(AnthropicModels))
		for i, m := range AnthropicModels {
			out[i] = m.ID
		}
		return out, nil
	}
	e := &openaiEngine{cfg: cfg}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(cfg.BaseURL, "/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	e.headers(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %v", cfg.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError(resp.StatusCode, errorMessage(resp))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list); err != nil {
		return nil, errors.New("the endpoint did not return a model list")
	}
	out := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out, nil
}
