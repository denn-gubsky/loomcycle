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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

const (
	defaultBaseURL = "http://localhost:11434"
)

// Driver speaks Ollama's /api/chat. Single struct serves two registrations:
//
//   - providerID = "ollama"       → hosted ollama.com (Bearer auth via apiKey)
//   - providerID = "ollama-local" → local-network Ollama (apiKey empty,
//     local trust model)
//
// The wire shape is identical; only the auth header and base URL default
// differ. main.go constructs one Driver per registration.
type Driver struct {
	providerID  string
	apiKey      string // empty for ollama-local; Bearer token for hosted
	baseURL     string
	http        *http.Client
	idleTimeout time.Duration
	numCtx      int // 0 = omit (Ollama server default applies)
}

// New constructs a Driver.
//
//   - providerID names this registration (e.g. "ollama" or "ollama-local").
//     Empty defaults to "ollama" for back-compat with any caller outside
//     main.go's resolver wiring.
//   - apiKey is the Bearer token for the hosted ollama.com endpoint;
//     leave empty for local Ollama (no Authorization header is sent).
//   - baseURL may be empty for the default localhost endpoint.
//
// streamOpts controls per-stream timeouts. Local generation can be very
// slow on first-token (model warmup, large context); callers passing zero
// values get the streamhttp defaults — usually fine, but operators on
// cold-start sensitive deployments may want to bump HeaderTimeout via
// LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_SECONDS.
func New(providerID, apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if providerID == "" {
		providerID = "ollama"
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	streamOpts = streamOpts.Resolve()
	if httpClient == nil {
		httpClient = streamhttp.NewClient(streamOpts.HeaderTimeout)
	}
	return &Driver{providerID: providerID, apiKey: apiKey, baseURL: baseURL, http: httpClient, idleTimeout: streamOpts.IdleTimeout}
}

// WithNumCtx sets the Ollama options.num_ctx that the driver includes
// on every chat request. Returns the same Driver for chaining at
// registration time:
//
//	pr.ollamaLocal = ollama.New(...).WithNumCtx(32768)
//
// Default 0 = don't set, which means the Ollama server falls back to
// the model's Modelfile PARAMETER num_ctx, or 4096 if the Modelfile
// doesn't specify. The 4096 ceiling is a documented Ollama default
// and the load-bearing reason this knob exists: without an explicit
// num_ctx, Ollama silently truncates the prompt at 4096 tokens with
// no error returned (the request just produces a partial completion).
// Caught live 2026-05-15: employer-profiler against
// ollama-local/glm-4.7-flash:q4_K_M produced 190 output tokens with
// stop_reason empty (not "end_turn") at 4797 input tokens — exactly
// the truncation signature.
//
// Operators wanting per-model overrides can rely on the Modelfile's
// PARAMETER num_ctx; the driver's num_ctx wins when both are set
// (Ollama treats request options as overrides). Setting a value
// larger than the model can handle is safe — Ollama clamps to the
// trained max for that architecture.
//
// Not safe to call concurrently with Call(); intended for registration.
func (d *Driver) WithNumCtx(n int) *Driver {
	if n > 0 {
		d.numCtx = n
	}
	return d
}

func (d *Driver) ID() string { return d.providerID }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: false,
		ParallelToolCalls: true, // model-dependent; we report the optimistic case
		Streaming:         true,
		MaxContextTokens:  0, // varies wildly by model; 0 means "ask the model"
		SupportsThinking:  false,
		// Ollama has no operator-controlled thinking-budget knob today.
		// Reasoning models (qwen3, deepseek-r1, hermes3) decide whether
		// to think based on their own defaults; the message.thinking
		// field is now surfaced as EventThinking so adapters can render
		// or hide the trace, but loomcycle has no input-side hint that
		// would dial it up or down. SupportsEffort=false signals to the
		// loop that an Ollama-routed agent's effort hint is dropped, so
		// the loop logs once per Run for operator visibility.
		SupportsEffort: false,
	}
}

// Call sends the chat request and streams Events. The goroutine that reads
// the response closes the channel when the stream ends.
// 429 retry: Ollama OSS doesn't rate-limit (no 429 expected on a local
// server). Ollama Cloud may emit a standard Retry-After; we handle it
// defensively. Same body-bytes-preserved retry as the cloud providers.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := d.buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)

	attempt := func(attemptCtx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(attemptCtx, "POST", d.baseURL+"/api/chat", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/x-ndjson")
		if d.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+d.apiKey)
		}
		return d.http.Do(req)
	}

	resp, err := ratelimit.Do(streamCtx, ratelimit.Config{
		Provider:    d.providerID,
		ParseHeader: ratelimit.OllamaRetryAfter,
		OnEvent:     req.OnEvent,
	}, attempt)
	if err != nil {
		cancelStream()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancelStream()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s %d: %s", d.providerID, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	resp.Body = streamhttp.WrapBody(resp.Body, d.idleTimeout, cancelStream)

	out := make(chan providers.Event, 16)
	go func() {
		defer cancelStream()
		streamEvents(streamCtx, resp.Body, out, len(req.Tools) > 0)
	}()
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
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Options  *wireOptions  `json:"options,omitempty"`
}

type wireOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"` // Ollama's name for max_tokens
	NumCtx      int      `json:"num_ctx,omitempty"`     // input-window size; 0 = Ollama server default
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

func (d *Driver) buildRequestBody(req providers.Request) ([]byte, error) {
	w := wireRequest{
		Model:  req.Model,
		Stream: true,
	}

	if req.Temperature != nil || req.MaxTokens > 0 || d.numCtx > 0 {
		w.Options = &wireOptions{
			Temperature: req.Temperature,
			NumPredict:  req.MaxTokens,
			NumCtx:      d.numCtx,
		}
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
	Model      string  `json:"model"`
	Message    message `json:"message"`
	Done       bool    `json:"done"`
	DoneReason string  `json:"done_reason"`

	// Usage fields (only present on the final "done":true frame).
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Thinking carries the model's reasoning trace for thinking-mode
	// models (qwen3, deepseek-r1, hermes3, etc.). Surfaced live as
	// EventThinking — distinct from Content so consumers can render or
	// hide reasoning independently. Pre-EventThinking, this field was
	// silently dropped because the driver only consumed Content.
	Thinking  string          `json:"thinking"`
	ToolCalls []chunkToolCall `json:"tool_calls"`
}

type chunkToolCall struct {
	Function chunkToolCallFn `json:"function"`
}

type chunkToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, wantTools bool) {
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
	var rawDoneReason string // pre-mapStopReason; needed if we re-evaluate after recovery
	var usage *providers.Usage
	var model string
	// hadToolCalls tracks whether *any* frame emitted tool_calls. Ollama may
	// emit tool_calls on a non-final frame, then send a separate done:true
	// frame with an empty tool_calls array. We must remember the earlier
	// emission so the loop iterates instead of breaking on "end_turn".
	var hadToolCalls bool
	// textBuf accumulates message.content across the stream. Used only by
	// the post-stream qwen3 tool-call-as-text recovery path (gated on
	// wantTools && !hadToolCalls). Non-tool flows still stream text live;
	// this buffer just mirrors what was streamed so we can re-parse it
	// at end-of-stream without buffering the user's view of progress.
	var textBuf strings.Builder

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

		if c.Message.Thinking != "" {
			if !send(providers.Event{Type: providers.EventThinking, Text: c.Message.Thinking}) {
				return
			}
		}
		if c.Message.Content != "" {
			textBuf.WriteString(c.Message.Content)
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
			rawDoneReason = c.DoneReason
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

	// qwen3 tool-call-as-text recovery. Empirically, qwen3:14b (and
	// related Ollama-served reasoning models) sometimes lose tool-call
	// discipline across iterations: the first iteration uses the
	// structured `tool_calls` envelope correctly, but subsequent
	// iterations emit the next tool call as `content` text — a JSON
	// payload like `{"name":"foo","arguments":{...}}`. The loop then
	// terminates with the JSON-as-text as the final assistant turn,
	// the consumer sees a tool-call JSON dump where it expected an
	// answer, and the run completes with garbage output.
	//
	// When this happens (wantTools=true, no structured tool_calls
	// arrived, and the buffered text content parses cleanly as one
	// or more tool-call objects), we synthesise EventToolCall events
	// at the tail of the stream. The loop's history record retains
	// the original streamed text (so the transcript's audit trail is
	// honest about what the model emitted), but the synthesised tool
	// calls let the loop iterate instead of terminating. The next
	// iteration typically produces a clean answer.
	//
	// Recovery is gated on wantTools=true so non-tool flows that
	// happen to emit JSON-shaped text (e.g. an agent whose final
	// answer IS a JSON object — ats-filter, injection-judge) don't
	// get false-positive tool calls synthesised.
	if wantTools && !hadToolCalls && textBuf.Len() > 0 {
		if recovered := tryParseToolCallsFromText(textBuf.String()); len(recovered) > 0 {
			for _, tu := range recovered {
				if !send(providers.Event{Type: providers.EventToolCall, ToolUse: tu}) {
					return
				}
			}
			hadToolCalls = true
			// Recompute stopReason now that we have tool calls. Ollama's
			// own done_reason was "stop" (the model thought it was
			// finished); we know better.
			stopReason = mapStopReason(rawDoneReason, true)
		}
	}

	send(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: usage})
}

// tryParseToolCallsFromText attempts to parse the raw text content as
// one or more Ollama-shaped tool-call objects. Two shapes covered:
//
//  1. JSON-shape (PR #26):
//     {"name":"...","arguments":{...}}
//     or an array of such objects, optionally wrapped in a ```json fence.
//
//  2. Markdown-bracket shape (this function's fallback path, v0.7.x):
//     [tool_use: <name>]\n{"...": ...}
//     [tool_use: <name> {"...": ...}]
//     [tool_use: <name>]
//     Some chat templates produce this form instead of structured
//     tool_calls — observed on a few hermes / mistral fine-tunes.
//
// Returns the parsed ToolUse list when either shape matches, nil
// otherwise. Strict matching prevents false positives from text that
// happens to look JSON-ish or contains the literal phrase "tool_use" in
// prose: we require the ENTIRE trimmed content to deserialise into the
// tool-call shape.
func tryParseToolCallsFromText(text string) []*providers.ToolUse {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil
	}
	// Strip a single markdown fence pair if present. qwen3's chat
	// template sometimes wraps tool-call output in ```json ... ```.
	if strings.HasPrefix(s, "```") {
		// Drop the opening fence (may be ``` or ```json or ```\n).
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		} else {
			s = strings.TrimPrefix(s, "```")
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}

	type rawCall struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}

	// Try array first — qwen3 occasionally batches multiple calls.
	if strings.HasPrefix(s, "[") && !strings.HasPrefix(s, "[tool_use:") {
		var arr []rawCall
		if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
			out := make([]*providers.ToolUse, 0, len(arr))
			for _, r := range arr {
				if r.Name == "" {
					return nil // any malformed entry → bail; treat as prose
				}
				args := r.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				out = append(out, &providers.ToolUse{Name: r.Name, Input: args})
			}
			return out
		}
		return nil
	}

	// Try the markdown-bracket shape. Falls through to the JSON-object
	// parse below when the text doesn't start with the bracket marker,
	// so prose containing the word "tool_use" mid-paragraph never trips
	// this path.
	if strings.HasPrefix(s, "[tool_use:") {
		if call := parseMarkdownToolCall(s); call != nil {
			return []*providers.ToolUse{call}
		}
		return nil
	}

	// Try single JSON object.
	var r rawCall
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil
	}
	if r.Name == "" {
		return nil
	}
	args := r.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return []*providers.ToolUse{{Name: r.Name, Input: args}}
}

// parseMarkdownToolCall recognises the bracketed-markdown tool-call
// form. Caller has already verified s starts with "[tool_use:" and
// trimmed surrounding whitespace. Returns nil on any malformation —
// the caller treats nil as "this isn't a tool call, leave it as text".
//
// Three shapes accepted:
//
//	[tool_use: name]                   → name, default args {}
//	[tool_use: name {args}]            → name + inline args
//	[tool_use: name]\n{args}           → name + post-bracket args
//
// In all cases the ENTIRE input must be consumed: any trailing prose
// after the args (or after the bracket when args are absent) is a
// disqualifier. Same strict-match contract as the JSON parser.
func parseMarkdownToolCall(s string) *providers.ToolUse {
	const marker = "[tool_use:"
	closeIdx := strings.IndexByte(s, ']')
	if closeIdx < 0 {
		return nil
	}
	inside := strings.TrimSpace(s[len(marker):closeIdx])
	if inside == "" {
		return nil
	}
	after := strings.TrimSpace(s[closeIdx+1:])

	// Split inside into name + optional inline args at the first
	// whitespace or '{'. Inline args, when present, MUST start with '{'.
	var name string
	var inlineArgs string
	if cut := strings.IndexAny(inside, " \t\n{"); cut >= 0 {
		name = strings.TrimSpace(inside[:cut])
		inlineArgs = strings.TrimSpace(inside[cut:])
	} else {
		name = inside
	}
	if !looksLikeIdentifier(name) {
		return nil
	}

	// Decide which args source applies. At most one of inlineArgs /
	// after may be non-empty; both populated is a malformation we
	// reject (the model produced something we can't unambiguously
	// interpret).
	switch {
	case inlineArgs != "" && after != "":
		return nil
	case inlineArgs != "":
		if !strings.HasPrefix(inlineArgs, "{") {
			return nil
		}
		if !isValidJSONObject(inlineArgs) {
			return nil
		}
		return &providers.ToolUse{Name: name, Input: json.RawMessage(inlineArgs)}
	case after != "":
		if !strings.HasPrefix(after, "{") {
			return nil
		}
		if !isValidJSONObject(after) {
			return nil
		}
		return &providers.ToolUse{Name: name, Input: json.RawMessage(after)}
	default:
		// Bracket form with no args at all → default to {}.
		return &providers.ToolUse{Name: name, Input: json.RawMessage("{}")}
	}
}

// looksLikeIdentifier validates the tool name as Anthropic / OpenAI's
// shared format ([A-Za-z_][A-Za-z0-9_-]*). Same regex the dispatcher
// applies on registration; rejecting here prevents the synthesised
// EventToolCall from carrying a name the dispatcher would refuse anyway.
func looksLikeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '_':
		case i > 0 && (c == '-' || (c >= '0' && c <= '9')):
		default:
			return false
		}
	}
	return true
}

// isValidJSONObject confirms s parses as a JSON object (not just any
// JSON value). Tool-call args must be an object shape per Anthropic /
// OpenAI's tool input contract.
func isValidJSONObject(s string) bool {
	var probe map[string]any
	return json.Unmarshal([]byte(s), &probe) == nil
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

// Probe checks reachability via GET /api/tags (no auth required —
// Ollama's local trust model). Returns nil iff the response is 200 OK
// with parseable JSON. Reuses fetchTags so a single round-trip can
// also surface the model list when ListModels is the next call (the
// resolver typically does both at once during a probe sweep).
func (d *Driver) Probe(ctx context.Context) error {
	_, err := d.fetchTags(ctx)
	return err
}

// ListModels returns the names of models pulled on this Ollama server
// (the `models[].name` array from /api/tags). These are the wire
// aliases the resolver matches against (e.g. "qwen3:14b",
// "gemma4:9b") — same strings agent yaml uses in its tier candidate
// list.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	return d.fetchTags(ctx)
}

// fetchTags is the shared GET /api/tags round-trip. Ollama's response
// shape:
//
//	{"models": [
//	  {"name": "qwen3:14b", "modified_at": "...", "size": 9276198565,
//	   "digest": "...", "details": {...}},
//	  ...
//	]}
//
// Unlike Anthropic / OpenAI, Ollama may legitimately return an empty
// `models` array (operator hasn't pulled any models yet). The
// resolver treats that as "provider reachable, every candidate
// stalled until something gets pulled" — distinct from probe failure.
func (d *Driver) fetchTags(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	if d.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+d.apiKey)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama /api/tags: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ollama /api/tags: status %d (%s)", resp.StatusCode, string(body))
	}
	var doc struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("ollama /api/tags decode: %w", err)
	}
	out := make([]string, 0, len(doc.Models))
	for _, m := range doc.Models {
		out = append(out, m.Name)
	}
	return out, nil
}
