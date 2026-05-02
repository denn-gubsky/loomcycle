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
		maxTokens = 4096
	}

	w := wireRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Tools:       req.Tools,
		Temperature: req.Temperature,
		Stream:      true, // we always stream
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

	var frame sseFrame
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			// dispatch frame
			if frame.event != "" || len(frame.data) > 0 {
				if !processFrame(frame, &current, &stopReason, &usage, send) {
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
	usage **providers.Usage,
	send func(providers.Event) bool,
) bool {
	switch f.event {
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
		*usage = &providers.Usage{
			InputTokens:         ev.Usage.InputTokens,
			OutputTokens:        ev.Usage.OutputTokens,
			CacheCreationTokens: ev.Usage.CacheCreationTokens,
			CacheReadTokens:     ev.Usage.CacheReadTokens,
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
