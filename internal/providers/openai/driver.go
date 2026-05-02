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
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
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
	}
}

// Call sends the request and returns a channel of Events. The goroutine that
// reads the response closes the channel when the stream ends.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", d.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if d.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)
	}

	resp, err := d.http.Do(httpReq)
	if err != nil {
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
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
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
