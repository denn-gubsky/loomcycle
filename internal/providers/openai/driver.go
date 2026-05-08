// Package openai implements the Provider interface for OpenAI's Chat
// Completions API. We picked Chat Completions over the newer Responses API
// because (a) most OpenAI-compatible endpoints (vLLM, LiteLLM, Anyscale,
// Together, ...) speak Chat Completions, not Responses, and (b) the Responses
// API ships built-in tools (web_search, code_interpreter) that fight our
// "we own the tool loop" stance — Chat Completions stays out of our way.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultTimeout = 5 * time.Minute
)

// Driver speaks Chat Completions.
type Driver struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// New constructs a Driver. baseURL may be empty for the default endpoint, or
// set to any OpenAI-compatible base (e.g. "http://localhost:8000/v1" for vLLM).
func New(apiKey, baseURL string, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Driver{apiKey: apiKey, baseURL: baseURL, http: httpClient}
}

func (d *Driver) ID() string { return "openai" }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: false, // OpenAI auto-caches; no explicit knob like Anthropic
		ParallelToolCalls: true,
		Streaming:         true,
		MaxContextTokens:  128_000, // gpt-4o family default; bigger on some
		SupportsThinking:  false,
		// SupportsEffort=true means the driver translates Request.Effort
		// to reasoning_effort on the wire. Whether the resolved model
		// actually honours it (o-series + GPT-5 do; chat-only models
		// like gpt-5.4-mini reject) is the API's decision — the driver
		// passes through; operators using effort with non-reasoning
		// models will see the API's 400 surface clearly.
		SupportsEffort: true,
	}
}

// Call sends the request and returns a channel of Events. The goroutine that
// reads the response closes the channel when the stream ends.
//
// 429 retry: ratelimit.Do honours OpenAI's Retry-After header when present;
// otherwise it reads the bigger of x-ratelimit-reset-{requests,tokens}
// (relative durations like "120ms" or "12.5s") so we wait for the more
// constrained bucket. The same body bytes are re-sent — full conversation
// context preserved across the rate limit.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	attempt := func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", d.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		if d.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+d.apiKey)
		}
		return d.http.Do(req)
	}

	resp, err := ratelimit.Do(ctx, ratelimit.Config{
		Provider:    "openai",
		ParseHeader: ratelimit.OpenAIRetryAfter,
		OnEvent:     req.OnEvent,
	}, attempt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	out := make(chan providers.Event, 16)
	go streamEvents(ctx, resp.Body, out)
	return out, nil
}

// --- request marshalling ---
//
// Chat Completions message shapes:
//
//   { "role": "system",    "content": "..." }
//   { "role": "user",      "content": "..." }
//   { "role": "assistant", "content": "...", "tool_calls": [...] }
//   { "role": "tool",      "tool_call_id": "...", "content": "..." }
//
// Our internal ContentBlock union flattens to these as follows:
//   - Anthropic-style assistant message with tool_use blocks → role:"assistant"
//     with the text concatenated into "content" and tool_use blocks lifted
//     into "tool_calls".
//   - tool_result blocks (which we encode in user-role messages) → split out
//     into separate role:"tool" messages.

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`

	// stream_options.include_usage tells OpenAI to include final usage in the
	// last data frame before [DONE]. Without this we have no token counts.
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`

	// ReasoningEffort is the wire param OpenAI's reasoning models
	// (o-series, GPT-5+) accept. Pass-through of the operator's
	// effort hint: "low" / "medium" / "high". Empty = omit (driver
	// default; chat models that don't accept the param ignore it).
	// DeepSeek's /v1/chat/completions wrapper inherits this verbatim
	// — DeepSeek V4 accepts the same field name per its OpenAI-compat
	// surface. The API rejects the field on non-reasoning models
	// (gpt-5.4-mini, etc.) with a 400; operators using effort with
	// those models should expect the rejection rather than silent drop.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"` // always "function"
	Function wireToolCallFn `json:"function"`
}

type wireToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string, not a JSON object
}

type wireTool struct {
	Type     string       `json:"type"` // "function"
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func buildRequestBody(req providers.Request) ([]byte, error) {
	w := wireRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
		// Pass-through of Request.Effort to OpenAI's reasoning_effort
		// param. Empty stays empty — omitempty drops it from the wire.
		// "low" / "medium" / "high" pass through verbatim; the API
		// rejects unknown strings, which is fine because we only ever
		// receive values from the validated Effort enum at the config
		// layer.
		ReasoningEffort: req.Effort,
	}

	// System blocks become a single role:"system" message at the top.
	// Cacheable hint is dropped — OpenAI auto-caches and gives no API knob.
	if len(req.System) > 0 {
		var sysText strings.Builder
		for _, sb := range req.System {
			if sysText.Len() > 0 {
				sysText.WriteString("\n\n")
			}
			sysText.WriteString(sb.Text)
		}
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: sysText.String()})
	}

	for _, m := range req.Messages {
		w.Messages = append(w.Messages, flattenMessage(m)...)
	}

	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Type: "function",
			Function: wireFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	return json.Marshal(w)
}

// flattenMessage maps one of our ContentBlock-union messages into one or more
// Chat Completions wire messages. The split is needed because:
//   - Assistant messages combine text + tool_use blocks (one wire message).
//   - User messages may carry tool_result blocks; each tool_result becomes
//     its own role:"tool" wire message, and any non-tool_result text blocks
//     stay in the original user message.
func flattenMessage(m providers.Message) []wireMessage {
	if m.Role == "assistant" {
		var text strings.Builder
		var calls []wireToolCall
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				text.WriteString(c.Text)
			case "tool_use":
				calls = append(calls, wireToolCall{
					ID:   c.ToolUseID,
					Type: "function",
					Function: wireToolCallFn{
						Name:      c.ToolName,
						Arguments: string(c.ToolInput),
					},
				})
			}
		}
		return []wireMessage{{Role: "assistant", Content: text.String(), ToolCalls: calls}}
	}

	// user role: split tool_result blocks into their own messages.
	var out []wireMessage
	var userText strings.Builder
	for _, c := range m.Content {
		switch c.Type {
		case "tool_result":
			out = append(out, wireMessage{
				Role:       "tool",
				ToolCallID: c.ToolUseID,
				Content:    c.Text,
			})
		case "text":
			if userText.Len() > 0 {
				userText.WriteString("\n")
			}
			userText.WriteString(c.Text)
		}
	}
	if userText.Len() > 0 {
		// Plain user text comes first; tool messages follow.
		out = append([]wireMessage{{Role: "user", Content: userText.String()}}, out...)
	}
	return out
}

// --- streaming response parsing ---
//
// Chat Completions SSE frames look like:
//   data: {"choices":[{"delta":{"content":"..."}}]}\n\n
//   data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"...","function":{...}}]}}]}\n\n
//   data: [DONE]\n\n
//
// Tool calls stream by index: the first delta for an index has the id +
// function.name, subsequent deltas only fill in function.arguments piecewise.
// We accumulate into toolAcc[index] and emit one EventToolCall per index when
// streaming completes.

type chunkDelta struct {
	Content   string             `json:"content"`
	ToolCalls []chunkToolCallDel `json:"tool_calls"`
}

type chunkToolCallDel struct {
	Index    int            `json:"index"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function chunkFunctionD `json:"function"`
}

type chunkFunctionD struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chunk struct {
	// Model is the wire-resolved alias (e.g. "gpt-4o-mini-2024-07-18"
	// or "deepseek-chat") set on every chunk by OpenAI-compatible
	// servers. We capture it so the final EventDone.Usage carries
	// the actual billed model rather than an empty string. Without
	// this, runs.model never populates downstream — same regression
	// class as the v0.4.0 Anthropic fix (commit 5bdccfc), just for
	// OpenAI / DeepSeek / vLLM / any OpenAI-compatible endpoint.
	Model   string `json:"model"`
	Choices []struct {
		Delta        chunkDelta `json:"delta"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		// gpt-4o-mini and later expose cached prompt tokens here:
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// toolAccumulator buffers a streaming tool_call.
type toolAccumulator struct {
	id   string
	name string
	args strings.Builder
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

	tools := map[int]*toolAccumulator{}
	var stopReason string
	var usage *providers.Usage
	// model is the wire-resolved alias captured from the first
	// chunk that carries one. Stamped onto Usage when the usage
	// chunk arrives (or onto an empty Usage at done time if
	// stream_options.include_usage didn't fire — defensive, since
	// some OpenAI-compatible servers omit usage on cancelled
	// streams).
	var model string

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if string(payload) == "[DONE]" {
			break
		}

		var c chunk
		if err := json.Unmarshal(payload, &c); err != nil {
			continue // skip malformed frames; final usage frame may be malformed in cancelled streams
		}
		if c.Model != "" && model == "" {
			model = c.Model
		}

		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				if !send(providers.Event{Type: providers.EventText, Text: ch.Delta.Content}) {
					return
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := tools[tc.Index]
				if !ok {
					acc = &toolAccumulator{}
					tools[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
				}
			}
			if ch.FinishReason != "" {
				stopReason = mapStopReason(ch.FinishReason)
			}
		}

		if c.Usage != nil {
			u := &providers.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
				Model:        model,
			}
			if c.Usage.PromptTokensDetails != nil {
				u.CacheReadTokens = c.Usage.PromptTokensDetails.CachedTokens
			}
			usage = u
		}
	}
	if err := scanner.Err(); err != nil {
		send(providers.Event{Type: providers.EventError, Error: "stream read: " + err.Error()})
		return
	}

	// Emit accumulated tool calls in index order before the done event.
	// Indices may be non-contiguous (e.g. 0, 2 with a gap at 1) — legal per
	// the OpenAI spec — so we sort the actual keys rather than iterating
	// 0..len(tools), which would silently drop any index >= len(tools).
	indices := make([]int, 0, len(tools))
	for i := range tools {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		acc := tools[i]
		args := acc.args.String()
		if args == "" {
			args = "{}"
		}
		if !send(providers.Event{
			Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{
				ID:    acc.id,
				Name:  acc.name,
				Input: json.RawMessage(args),
			},
		}) {
			return
		}
	}

	send(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: usage})
}

// mapStopReason translates OpenAI's finish_reason vocabulary into our shared
// vocabulary. Both sides have "stop"/"end_turn", "tool_calls"/"tool_use", and
// "length"/"max_tokens"; we normalise on the Anthropic spelling because the
// agent loop already branches on that.
func mapStopReason(openaiReason string) string {
	switch openaiReason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return openaiReason
	}
}

// Probe checks reachability + auth by hitting GET /v1/models with the
// passed context's deadline. Reuses fetchModels' round-trip so a single
// HTTP call covers both health and the model-list payload — the
// caller decides which signal it needs.
func (d *Driver) Probe(ctx context.Context) error {
	_, err := d.fetchModels(ctx)
	return err
}

// ListModels returns the wire aliases the OpenAI-compatible endpoint
// currently serves. Used by the resolver to populate the Listed flag
// in ModelStatus. DeepSeek's /v1/models has the same response shape,
// so the deepseek wrapper inherits this method unchanged.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	return d.fetchModels(ctx)
}

// fetchModels is the shared GET /v1/models round-trip. OpenAI's
// response shape:
//
//	{"object": "list",
//	 "data": [{"id": "gpt-5.4", "object": "model", "created": ...},
//	          {"id": "gpt-4o-mini", ...},
//	          ...]}
//
// We surface only the IDs. OpenAI's response is small enough (a few
// dozen models for a typical org) that one page suffices.
func (d *Driver) fetchModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai /v1/models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("openai /v1/models: status %d (%s)", resp.StatusCode, string(body))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("openai /v1/models decode: %w", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		out = append(out, m.ID)
	}
	return out, nil
}
