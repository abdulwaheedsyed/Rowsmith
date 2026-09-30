package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// anthropicEngine talks to Claude through the official SDK: the schema is a
// cached system block, reasoning streams as summaries, and a declined
// request is re-served by a fallback model within the same call.
type anthropicEngine struct {
	client   *anthropic.Client
	params   anthropic.BetaMessageNewParams
	messages []anthropic.BetaMessageParam
}

var (
	clientsMu sync.Mutex
	clients   = map[string]*anthropic.Client{}
)

func anthropicClient(cfg Config) *anthropic.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	k := cfg.APIKey + "\x00" + cfg.BaseURL
	if c, ok := clients[k]; ok {
		return c
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey), option.WithMaxRetries(2)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL)) // tests
	}
	c := anthropic.NewClient(opts...)
	clients = map[string]*anthropic.Client{k: &c} // one key at a time
	return &c
}

// adaptive reports models that take adaptive thinking, effort and
// server-side refusal fallbacks.
func adaptive(model string) bool { return !strings.HasPrefix(model, "claude-haiku") }

func newAnthropicEngine(cfg Config, p prompt, tools []toolSpec) *anthropicEngine {
	system := []anthropic.BetaTextBlockParam{{Text: p.Instructions}}
	if p.Schema != "" {
		system = append(system, anthropic.BetaTextBlockParam{Text: p.Schema, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()})
	}
	params := anthropic.BetaMessageNewParams{
		Model:        cfg.Model,
		MaxTokens:    32000,
		System:       system,
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(), // the growing conversation prefix
	}
	for _, t := range tools {
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name: t.Name, Description: anthropic.String(t.Description), Strict: anthropic.Bool(true),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: t.Properties, Required: t.schema()["required"].([]string),
				ExtraFields: map[string]any{"additionalProperties": false}},
		}})
	}
	if adaptive(cfg.Model) {
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplaySummarized,
		}}
		effort := cfg.Effort
		if effort == "" {
			effort = "medium"
		}
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effort)}
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	return &anthropicEngine{client: anthropicClient(cfg), params: params}
}

func (e *anthropicEngine) mark() int       { return len(e.messages) }
func (e *anthropicEngine) rollback(to int) { e.messages = e.messages[:to] }
func (e *anthropicEngine) addUser(text string) {
	e.messages = append(e.messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
}

func (e *anthropicEngine) addToolResults(results []toolResult) {
	blocks := make([]anthropic.BetaContentBlockParamUnion, len(results))
	for i, r := range results {
		blocks[i] = anthropic.NewBetaToolResultBlock(r.ID, r.Text, r.IsError)
	}
	e.messages = append(e.messages, anthropic.NewBetaUserMessage(blocks...))
}

func (e *anthropicEngine) step(ctx context.Context, emit Emit) (stepResult, error) {
	params := e.params
	params.Messages = e.messages
	stream := e.client.Beta.Messages.NewStreaming(ctx, params)
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return stepResult{}, err
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			switch delta := d.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				emit(map[string]any{"t": "text", "text": delta.Text})
			case anthropic.BetaThinkingDelta:
				if delta.Thinking != "" {
					emit(map[string]any{"t": "thinking", "text": delta.Thinking})
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return stepResult{}, anthropicError(err)
	}
	e.messages = append(e.messages, msg.ToParam()) // unchanged, thinking blocks included
	res := stepResult{Model: string(msg.Model), Usage: usage{Input: msg.Usage.InputTokens, Output: msg.Usage.OutputTokens, CacheRead: msg.Usage.CacheReadInputTokens}}
	switch msg.StopReason {
	case anthropic.BetaStopReasonToolUse:
		for _, block := range msg.Content {
			if use, ok := block.AsAny().(anthropic.BetaToolUseBlock); ok {
				res.Calls = append(res.Calls, toolCall{ID: use.ID, Name: use.Name, Input: json.RawMessage(use.JSON.Input.Raw())})
			}
		}
	case anthropic.BetaStopReasonRefusal:
		res.Stop, res.Refusal = "refusal", msg.StopDetails.Explanation
	case anthropic.BetaStopReasonMaxTokens:
		res.Stop = "length"
	default:
		res.Stop = "end"
	}
	return res, nil
}

// anthropicError turns API failures into messages people can act on.
func anthropicError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("stopped")
	}
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		return statusError(apierr.StatusCode, apierr.Error())
	}
	return fmt.Errorf("could not reach the Anthropic API: %w", err)
}

func statusError(code int, detail string) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return errors.New("the API key was rejected; an admin can update it under Administration → AI assistant")
	case code == http.StatusNotFound:
		return fmt.Errorf("the model or endpoint was not found (%s)", clip(detail, 200))
	case code == http.StatusTooManyRequests:
		return errors.New("the provider is rate-limiting this key; try again in a moment")
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		return fmt.Errorf("the request was not accepted: %s", clip(detail, 300))
	case code >= 500:
		return errors.New("the AI provider is unavailable or overloaded; try again in a moment")
	}
	return fmt.Errorf("the AI provider returned %d: %s", code, clip(detail, 300))
}
