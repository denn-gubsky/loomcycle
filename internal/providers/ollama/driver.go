// Package ollama implements the Provider interface for Ollama's /api/chat
// endpoint.
//
// Two things diverge from the cloud providers and are worth knowing:
//
//  1. Wire format is NDJSON (newline-delimited JSON), not SSE. The stream ends
//     when the body closes; the final line carries "done":true plus usage
//     counters in the eval-* fields.
//
//  2. Tool-use reliability depends on the model. Tool-tuned models (llama3.1+,
//     qwen2.5, mistral-large, ...) emit structured tool_calls correctly.
//     Non-tuned models silently ignore the "tools" field — no error, just no
//     tool_calls in the response. We trust the native API and document the
//     limitation rather than papering over it with prompt-engineering shims.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const (
	defaultBaseURL = "http://localhost:11434"
	defaultTimeout = 10 * time.Minute // local generation can be slow
)

// Driver speaks Ollama's /api/chat. No API key — local trust model.
type Driver struct {
	baseURL string
	http    *http.Client
}

// New constructs a Driver. baseURL may be empty for the default localhost endpoint.
func New(baseURL string, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Driver{baseURL: baseURL, http: httpClient}
}

func (d *Driver) ID() string { return "ollama" }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: false,
		ParallelToolCalls: true, // model-dependent; we report the optimistic case
		Streaming:         true,
		MaxContextTokens:  0, // varies wildly by model; 0 means "ask the model"
		SupportsThinking:  false,
	}
}

// Call sends the chat request and streams Events. The goroutine that reads
// the response closes the channel when the stream ends.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", d.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")

	resp, err := d.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	out := make(chan providers.Event, 16)
	go streamEvents(ctx, resp.Body, out)
	return out, nil
}

// --- request marshalling ---
//
// /api/chat takes:
//
//	{
//	  "model":   "llama3.1",
//	  "stream":  true,
//	  "messages":[
//	    {"role":"system","content":"..."},
//	    {"role":"user","content":"..."},
//	    {"role":"assistant","content":"...","tool_calls":[...]},
//	    {"role":"tool","content":"..."}            // result of a tool_use
//	  ],
//	  "tools":[ {"type":"function","function":{...}} ],
//	  "options":{"temperature":..., "num_predict":...}
//	}

type wireRequest struct {
	Model    string             `json:"model"`
	Stream   bool               `json:"stream"`
	Messages []wireMessage      `json:"messages"`
	Tools    []wireTool         `json:"tools,omitempty"`
	Options  *wireOptions       `json:"options,omitempty"`
}

type wireOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"` // Ollama's name for max_tokens
}

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	Function wireToolCallFn `json:"function"`
}

type wireToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // object, not string (unlike OpenAI)
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func buildRequestBody(req providers.Request) ([]byte, error) {
	w := wireRequest{
		Model:  req.Model,
		Stream: true,
	}

	if req.Temperature != nil || req.MaxTokens > 0 {
		w.Options = &wireOptions{Temperature: req.Temperature, NumPredict: req.MaxTokens}
	}

	// System blocks → one role:"system" message.
	if len(req.System) > 0 {
		var sys strings.Builder
		for _, sb := range req.System {
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(sb.Text)
		}
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: sys.String()})
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
// Ollama wire messages. The split rules match OpenAI: assistant messages
// combine text + tool_use blocks; tool_result blocks split into role:"tool".
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
					Function: wireToolCallFn{
						Name:      c.ToolName,
						Arguments: c.ToolInput,
					},
				})
			}
		}
		return []wireMessage{{Role: "assistant", Content: text.String(), ToolCalls: calls}}
	}

	// user role: split tool_result into role:"tool" entries.
	var out []wireMessage
	var userText strings.Builder
	for _, c := range m.Content {
		switch c.Type {
		case "tool_result":
			out = append(out, wireMessage{Role: "tool", Content: c.Text})
		case "text":
			if userText.Len() > 0 {
				userText.WriteString("\n")
			}
			userText.WriteString(c.Text)
		}
	}
	if userText.Len() > 0 {
		out = append([]wireMessage{{Role: "user", Content: userText.String()}}, out...)
	}
	return out
}

// --- streaming response parsing ---
//
// NDJSON frames look like:
//
//	{"model":"llama3.1","created_at":"...","message":{"role":"assistant","content":"hel"},"done":false}
//	{"model":"llama3.1","created_at":"...","message":{"role":"assistant","content":"lo"},"done":false}
//	{"model":"llama3.1","created_at":"...","message":{"role":"assistant","content":"","tool_calls":[...]},"done":true,"done_reason":"stop","prompt_eval_count":42,"eval_count":7}
//
// Ollama doesn't index-stream tool_calls (deltas) — they arrive whole on the
// final or near-final line. So no accumulator is needed.

type chunk struct {
	Model     string  `json:"model"`
	Message   message `json:"message"`
	Done      bool    `json:"done"`
	DoneReason string `json:"done_reason"`

	// Usage fields (only present on the final "done":true frame).
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

type message struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []chunkToolCall `json:"tool_calls"`
}

type chunkToolCall struct {
	Function chunkToolCallFn `json:"function"`
}

type chunkToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
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
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var stopReason string
	var usage *providers.Usage
	var model string
	// hadToolCalls tracks whether *any* frame emitted tool_calls. Ollama may
	// emit tool_calls on a non-final frame, then send a separate done:true
	// frame with an empty tool_calls array. We must remember the earlier
	// emission so the loop iterates instead of breaking on "end_turn".
	var hadToolCalls bool

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var c chunk
		if err := json.Unmarshal(line, &c); err != nil {
			continue
		}
		if model == "" && c.Model != "" {
			model = c.Model
		}

		if c.Message.Content != "" {
			if !send(providers.Event{Type: providers.EventText, Text: c.Message.Content}) {
				return
			}
		}
		for _, tc := range c.Message.ToolCalls {
			hadToolCalls = true
			args := tc.Function.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			if !send(providers.Event{
				Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{
					ID:    "", // Ollama doesn't issue tool_call IDs; loop assigns one
					Name:  tc.Function.Name,
					Input: args,
				},
			}) {
				return
			}
		}

		if c.Done {
			stopReason = mapStopReason(c.DoneReason, hadToolCalls)
			if c.PromptEvalCount > 0 || c.EvalCount > 0 {
				usage = &providers.Usage{
					InputTokens:  c.PromptEvalCount,
					OutputTokens: c.EvalCount,
					Model:        model,
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		send(providers.Event{Type: providers.EventError, Error: "stream read: " + err.Error()})
		return
	}

	send(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: usage})
}

// mapStopReason translates Ollama's done_reason into our shared vocabulary.
// Ollama uses "stop"/"length"; if any tool_calls were emitted on the final
// frame, that's the equivalent of OpenAI's "tool_calls" finish_reason and we
// surface "tool_use" so the loop runs another iteration.
func mapStopReason(ollamaReason string, hadToolCalls bool) string {
	if hadToolCalls {
		return "tool_use"
	}
	switch ollamaReason {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return ollamaReason
	}
}
