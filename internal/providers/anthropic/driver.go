// Package anthropic implements the Provider interface for Anthropic's Messages
// API. Streaming-only; we own SSE parsing so we can place cache_control
// breakpoints exactly where the loop wants them.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	defaultTimeout = 5 * time.Minute
)

// Driver is the Anthropic Messages API implementation of providers.Provider.
type Driver struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// New constructs a Driver. baseURL may be empty for the default endpoint.
func New(apiKey, baseURL string, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Driver{apiKey: apiKey, baseURL: baseURL, http: httpClient}
}

func (d *Driver) ID() string { return "anthropic" }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: true,
		ParallelToolCalls: true,
		Streaming:         true,
		MaxContextTokens:  200_000,
		SupportsThinking:  true,
		SupportsEffort:    true,
	}
}

// Call sends the request and returns a channel of Events. The channel is
// closed when the stream ends (clean or error). Cancel ctx to abort.
//
// 429 retry: if Anthropic rate-limits the request, ratelimit.Do honours
// the Retry-After header (or the exponential fallback) and re-sends the
// same body bytes — preserving the full conversation history, tool
// definitions, system blocks, and cache_control breakpoints. The agent
// loop sees no failure on a transient rate-limit; it just observes a
// longer round-trip.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	attempt := func(ctx context.Context) (*http.Response, error) {
		// Build a fresh request each attempt — http.Request.Body is
		// consumed by Do, so a single Reader can't be reused.
		req, err := http.NewRequestWithContext(ctx, "POST", d.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("x-api-key", d.apiKey)
		req.Header.Set("anthropic-version", apiVersion)
		return d.http.Do(req)
	}

	resp, err := ratelimit.Do(ctx, ratelimit.Config{
		Provider:    "anthropic",
		ParseHeader: ratelimit.AnthropicRetryAfter,
		OnEvent:     req.OnEvent,
	}, attempt)
	if err != nil {
		// ctx errors aren't HTTP errors — propagate as-is so the loop's
		// errors.Is(ctx.Err()) checks aren't masked by the "http:" prefix.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	out := make(chan providers.Event, 16)
	go streamEvents(ctx, resp.Body, out)
	return out, nil
}

// --- request marshalling ---

type wireRequest struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	System      []wireSystemBlock    `json:"system,omitempty"`
	Messages    []wireMessage        `json:"messages"`
	Tools       []providers.ToolSpec `json:"tools,omitempty"`
	Temperature *float64             `json:"temperature,omitempty"`
	Stream      bool                 `json:"stream"`
	// Thinking opts the request into Anthropic's extended-thinking
	// surface. Only sent when the agent declared an effort hint AND
	// the model is reasoning-capable (sonnet/opus, NOT haiku). When
	// the field is nil it's omitted from the wire — non-reasoning
	// models would 400 on its presence.
	Thinking *wireThinking `json:"thinking,omitempty"`
}

// wireThinking is Anthropic's extended-thinking opt-in. The API spec
// uses {"type":"enabled","budget_tokens":N} with budget ≥ 1024 and
// ≤ MaxTokens. We never send a disabled form; "no thinking" = field
// omitted entirely.
type wireThinking struct {
	Type         string `json:"type"`          // always "enabled"
	BudgetTokens int    `json:"budget_tokens"` // ≥ 1024 per Anthropic spec
}

type wireSystemBlock struct {
	Type         string            `json:"type"` // "text"
	Text         string            `json:"text"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type wireMessage struct {
	Role    string             `json:"role"`
	Content []wireContentBlock `json:"content"`
}

type wireContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func buildRequestBody(req providers.Request) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		// 8192 picked over the previous 4096: haiku-4-5/sonnet-4-6
		// both support up to 64k output, and 4096 was empirically
		// truncating verdict-batch agents (~12k chars output ≈ 4k
		// tokens). 8k is still conservative — agents that need more
		// can set max_tokens: NN explicitly in their YAML.
		maxTokens = 8192
	}

	w := wireRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Tools:       req.Tools,
		Temperature: req.Temperature,
		Stream:      true, // we always stream
	}

	// Translate the agent's effort hint to Anthropic's extended-
	// thinking block when the model is reasoning-capable. haiku
	// models don't support extended thinking — sending the field
	// would 400 — so we gate on the model name. The decision table:
	//
	//   effort == ""     → no thinking field (driver default)
	//   model is haiku   → no thinking field (model can't think)
	//   effort == "low"  → no thinking field (low maps to "skip")
	//   effort == "med"  → thinking{enabled, budget=2048}
	//   effort == "high" → thinking{enabled, budget=8192}
	//
	// "low" intentionally maps to "no thinking" rather than "low
	// budget" because Anthropic's spec requires budget ≥ 1024 and
	// we want the operator's "low effort" intent to mean "answer
	// fast, don't reason" — the cheapest behaviour the wire
	// supports. Medium and high pick conservative budgets that
	// fit comfortably under the default 8192 max_tokens.
	if budget := anthropicEffortBudget(req.Effort, req.Model); budget > 0 {
		// Anthropic requires budget < max_tokens. When the requested
		// budget would equal or exceed max_tokens, leave a 1024-token
		// response margin (matches Anthropic's stated 1024 minimum
		// for the budget itself — symmetric headroom on both sides
		// of the split). Floor the result at 1024; if max_tokens is
		// too small to satisfy that, skip thinking rather than send
		// an invalid request.
		if budget >= maxTokens {
			budget = maxTokens - 1024
		}
		if budget >= 1024 {
			w.Thinking = &wireThinking{Type: "enabled", BudgetTokens: budget}
		}
	}

	for _, sb := range req.System {
		if sb.Type != "text" && sb.Type != "" {
			continue // unsupported system block type
		}
		out := wireSystemBlock{Type: "text", Text: sb.Text}
		if sb.Cacheable {
			out.CacheControl = &wireCacheControl{Type: "ephemeral"}
		}
		w.System = append(w.System, out)
	}

	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role}
		for _, c := range m.Content {
			wm.Content = append(wm.Content, toWireBlock(c))
		}
		w.Messages = append(w.Messages, wm)
	}

	return json.Marshal(w)
}

func toWireBlock(c providers.ContentBlock) wireContentBlock {
	switch c.Type {
	case "tool_use":
		return wireContentBlock{
			Type:  "tool_use",
			ID:    c.ToolUseID,
			Name:  c.ToolName,
			Input: c.ToolInput,
		}
	case "tool_result":
		return wireContentBlock{
			Type:      "tool_result",
			ToolUseID: c.ToolUseID,
			Content:   c.Text,
			IsError:   c.IsError,
		}
	default: // "text"
		return wireContentBlock{Type: "text", Text: c.Text}
	}
}

// --- SSE parsing ---
//
// Anthropic streams a sequence of events. We care about:
//   - message_start          : carries the resolved model alias (e.g.
//                              `claude-haiku-4-5-20251001`); needed so
//                              the SSE Usage event can carry model and
//                              the consumer can price the run.
//   - content_block_start    : begins a text or tool_use block
//   - content_block_delta    : text_delta or input_json_delta
//   - content_block_stop     : finalize current block (emit tool_call if tool_use)
//   - message_delta          : carries stop_reason + cumulative usage
//   - message_stop           : end of stream
//   - error                  : provider-side error
//
// Other event types are ignored.

type sseFrame struct {
	event string
	data  []byte
}

func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event) {
	defer body.Close()
	defer close(out)

	// send respects ctx so a cancelled request doesn't leak this goroutine on
	// a full unread channel. Returns false if ctx ended; callers should return
	// immediately so defer close(out) fires and the consumer's range exits.
	send := func(ev providers.Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var current pendingBlock
	var stopReason string
	var usage *providers.Usage
	// Resolved model alias from message_start. Anthropic sometimes
	// returns a date-suffixed alias (`claude-haiku-4-5-20251001`)
	// distinct from the request alias (`claude-haiku-4-5`); capture
	// the wire value so downstream pricing matches what was billed.
	var model string

	var frame sseFrame
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			// dispatch frame
			if frame.event != "" || len(frame.data) > 0 {
				if !processFrame(frame, &current, &stopReason, &model, &usage, send) {
					return
				}
			}
			frame = sseFrame{}
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			frame.event = strings.TrimSpace(string(line[len("event:"):]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			frame.data = bytes.TrimSpace(line[len("data:"):])
		}
	}
	if err := scanner.Err(); err != nil {
		send(providers.Event{Type: providers.EventError, Error: "stream read: " + err.Error()})
		return
	}
	// Final done event with stop_reason + usage.
	send(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: usage})
}

type pendingBlock struct {
	kind      string // "text" | "tool_use"
	toolID    string
	toolName  string
	toolInput strings.Builder
}

// processFrame returns false if the caller should abort (ctx cancelled while
// trying to send). All non-send paths return true unconditionally.
func processFrame(
	f sseFrame,
	current *pendingBlock,
	stopReason *string,
	model *string,
	usage **providers.Usage,
	send func(providers.Event) bool,
) bool {
	switch f.event {
	case "message_start":
		// Anthropic's message_start carries the full Message object,
		// including the resolved model alias. We don't need anything
		// else from it here — usage in message_start is partial
		// (input_tokens only, no cumulative); message_delta carries
		// the final cumulative usage. Just stash the model.
		var ev struct {
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal(f.data, &ev); err != nil {
			return true
		}
		if ev.Message.Model != "" {
			*model = ev.Message.Model
		}

	case "content_block_start":
		var ev struct {
			ContentBlock struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(f.data, &ev); err != nil {
			return true
		}
		current.kind = ev.ContentBlock.Type
		current.toolID = ev.ContentBlock.ID
		current.toolName = ev.ContentBlock.Name
		current.toolInput.Reset()
		// tool_use can ship initial input bytes inline (rare for streaming)
		if len(ev.ContentBlock.Input) > 0 && string(ev.ContentBlock.Input) != "{}" {
			current.toolInput.Write(ev.ContentBlock.Input)
		}

	case "content_block_delta":
		var ev struct {
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(f.data, &ev); err != nil {
			return true
		}
		switch ev.Delta.Type {
		case "text_delta":
			if !send(providers.Event{Type: providers.EventText, Text: ev.Delta.Text}) {
				return false
			}
		case "thinking_delta":
			// Anthropic extended-thinking chunks. Surfaced live as
			// EventThinking so consumers can render the trace
			// independently of EventText. The signature_delta type
			// (carrying the cryptographic seal of the thinking block)
			// is intentionally not surfaced — it's metadata for the
			// next-turn echo, not user-visible content.
			if !send(providers.Event{Type: providers.EventThinking, Text: ev.Delta.Thinking}) {
				return false
			}
		case "input_json_delta":
			current.toolInput.WriteString(ev.Delta.PartialJSON)
		}

	case "content_block_stop":
		if current.kind == "tool_use" {
			input := json.RawMessage(current.toolInput.String())
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			if !send(providers.Event{
				Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{
					ID:    current.toolID,
					Name:  current.toolName,
					Input: input,
				},
			}) {
				return false
			}
		}
		current.kind = ""
		current.toolInput.Reset()

	case "message_delta":
		var ev struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens         int `json:"input_tokens"`
				OutputTokens        int `json:"output_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
				CacheReadTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(f.data, &ev); err != nil {
			return true
		}
		if ev.Delta.StopReason != "" {
			*stopReason = ev.Delta.StopReason
		}
		// usage in message_delta is final cumulative; it overwrites.
		// Model comes from message_start (captured into *model above);
		// without it the consumer can't price the run.
		*usage = &providers.Usage{
			InputTokens:         ev.Usage.InputTokens,
			OutputTokens:        ev.Usage.OutputTokens,
			CacheCreationTokens: ev.Usage.CacheCreationTokens,
			CacheReadTokens:     ev.Usage.CacheReadTokens,
			Model:               *model,
		}

	case "error":
		var ev struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(f.data, &ev); err != nil {
			return send(providers.Event{Type: providers.EventError, Error: "anthropic stream error (unparseable)"})
		}
		return send(providers.Event{Type: providers.EventError, Error: ev.Error.Type + ": " + ev.Error.Message})
	}
	return true
}

// Probe checks reachability + auth by hitting GET /v1/models with a short
// deadline. Returns nil iff the response is 200 OK. The shared
// implementation with ListModels does the same round-trip; we keep them
// separate so callers that only need health-check don't pay the JSON
// decode cost.
func (d *Driver) Probe(ctx context.Context) error {
	_, err := d.fetchModels(ctx)
	return err
}

// ListModels returns the wire aliases Anthropic currently exposes (the
// `data[].id` array from /v1/models). Used by the resolver's startup
// + periodic probe to populate the Listed flag in ModelStatus.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	return d.fetchModels(ctx)
}

// anthropicEffortBudget translates the operator's effort hint into a
// concrete thinking budget for Anthropic's extended-thinking surface.
// Returns 0 to mean "skip the thinking block entirely" (either effort
// is unset, the model isn't reasoning-capable, or low maps to skip).
//
// Model gating: only opus and sonnet support extended thinking on the
// 4.x line. Haiku 4.5 doesn't, and sending the field would 400. We
// pattern-match the model name rather than maintain an explicit list
// — Anthropic's naming convention (claude-{family}-{version}) is
// stable enough that "haiku in name" is reliable. If a future model
// changes the convention, this gate is the one place to update.
func anthropicEffortBudget(effort, model string) int {
	if effort == "" {
		return 0
	}
	if strings.Contains(model, "haiku") {
		return 0
	}
	switch effort {
	case "low":
		// Low effort = "answer fast, don't reason." Map to "skip
		// thinking" rather than "minimum budget" — the cheapest
		// behaviour at the wire is no thinking at all.
		return 0
	case "medium":
		return 2048
	case "high":
		return 8192
	default:
		return 0 // unknown effort string; defensive
	}
}

// fetchModels is the shared GET /v1/models round-trip. Anthropic's
// response shape:
//
//	{"data": [{"id": "claude-haiku-4-5", "type": "model", ...}, ...],
//	 "first_id": "...", "last_id": "...", "has_more": false}
//
// We surface only the IDs and ignore pagination — the response is
// small enough (handful of models) that one page is enough for the
// resolver's purposes.
func (d *Driver) fetchModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", d.apiKey)
	req.Header.Set("anthropic-version", apiVersion)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic /v1/models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("anthropic /v1/models: status %d (%s)", resp.StatusCode, string(body))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("anthropic /v1/models decode: %w", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		out = append(out, m.ID)
	}
	return out, nil
}
