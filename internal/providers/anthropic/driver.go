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
	"log"
	"net/http"
	"strings"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Driver is the Anthropic Messages API implementation of providers.Provider.
type Driver struct {
	apiKey      string
	baseURL     string
	http        *http.Client
	idleTimeout time.Duration
}

// New constructs a Driver. baseURL may be empty for the default endpoint.
// streamOpts controls the per-stream timeouts; zero values resolve to
// streamhttp.Default*. When httpClient is nil, a fresh streaming client
// honoring streamOpts.HeaderTimeout is built.
func New(apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	streamOpts = streamOpts.Resolve()
	if httpClient == nil {
		httpClient = streamhttp.NewClient(streamOpts.HeaderTimeout)
	}
	return &Driver{apiKey: apiKey, baseURL: baseURL, http: httpClient, idleTimeout: streamOpts.IdleTimeout}
}

func (d *Driver) ID() string { return "anthropic" }

// resolveKey returns the API key to authenticate an inference request AND which
// credential scope it came from. A tenant/user credential named
// ANTHROPIC_API_KEY overrides the operator's host key (RFC AR); source is
// "operator" when none did. The source/scopeID ride the per-call Usage so the
// server can attribute spend (RFC AV). Model-availability probes (fetchModels)
// stay on the operator key — which models are reachable is an operator concern.
func (d *Driver) resolveKey(ctx context.Context) (key, source, scopeID string) {
	if r, ok := providers.ResolveCredentialFull(ctx, "ANTHROPIC_API_KEY"); ok {
		return r.Value, r.Scope, r.ScopeID
	}
	return d.apiKey, "operator", ""
}

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: true,
		ParallelToolCalls: true,
		Streaming:         true,
		MaxContextTokens:  200_000,
		SupportsThinking:  true,
		SupportsEffort:    true,
		SupportsVision:    true, // per-model refined by anthropicSupportsVision
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

	// streamCtx is a cancellable child of ctx. Its cancel is passed to
	// streamhttp.WrapBody so an idle body read can interrupt the
	// in-flight HTTP request. The streaming goroutine releases the ctx
	// via deferred cancel; the wrap may have called cancel earlier
	// (idempotent).
	streamCtx, cancelStream := context.WithCancel(ctx)

	// RFC AR/AV: resolve the key + its owning scope ONCE per call (not per retry
	// attempt) so the header uses it and the source rides the per-call Usage.
	apiKey, credSource, credScopeID := d.resolveKey(ctx)

	attempt := func(attemptCtx context.Context) (*http.Response, error) {
		// v0.10.0 OTEL: one loomcycle.provider.call span per attempt.
		// Retries surface as separate sibling spans so operators see
		// retry latency in the run timeline.
		spanCtx, span := lcotel.RecordProviderCall(attemptCtx, lcotel.ProviderCallAttrs{
			Provider: "anthropic",
			Model:    req.Model,
			Effort:   req.Effort,
		})
		defer span.End()
		// Build a fresh request each attempt — http.Request.Body is
		// consumed by Do, so a single Reader can't be reused.
		httpReq, err := http.NewRequestWithContext(spanCtx, "POST", d.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			lcotel.SetSpanError(span, err)
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("anthropic-version", apiVersion)
		resp, err := d.http.Do(httpReq)
		if err != nil {
			lcotel.SetSpanError(span, err)
		} else if resp != nil && resp.StatusCode >= 400 {
			lcotel.SetSpanErrorMessage(span, "http "+resp.Status)
		}
		return resp, err
	}

	resp, err := ratelimit.Do(streamCtx, ratelimit.Config{
		Provider:    "anthropic",
		ParseHeader: ratelimit.AnthropicRetryAfter,
		OnEvent:     req.OnEvent,
	}, attempt)
	if err != nil {
		cancelStream()
		// ctx errors aren't HTTP errors — propagate as-is so the loop's
		// errors.Is(ctx.Err()) checks aren't masked by the "http:" prefix.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancelStream()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	resp.Body = streamhttp.WrapBody(resp.Body, d.idleTimeout, cancelStream)

	out := make(chan providers.Event, 16)
	go func() {
		defer cancelStream()
		streamEvents(streamCtx, resp.Body, out, credSource, credScopeID)
	}()
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
	// Sampling knobs Anthropic's Messages API accepts. top_k is Anthropic-
	// native; frequency/presence_penalty + seed are NOT Anthropic params, so
	// they're intentionally absent here (dropped). Temperature + top_p are also
	// dropped when Thinking is attached (the API rejects temperature!=1 then).
	TopP   *float64 `json:"top_p,omitempty"`
	TopK   *int     `json:"top_k,omitempty"`
	Stop   []string `json:"stop_sequences,omitempty"`
	Stream bool     `json:"stream"`
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
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   string           `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Source    *wireImageSource `json:"source,omitempty"` // type == "image" (RFC AT)
	// Thinking + Signature carry an extended-thinking block replayed on a
	// tool-use continuation (type == "thinking"). Anthropic verifies Signature
	// against Thinking and 400s on a mismatch or a missing block, so both are
	// sent together and only when both are present.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// wireImageSource is Anthropic's inline base64 image source block:
// {"type":"base64","media_type":"image/png","data":"<base64>"}.
type wireImageSource struct {
	Type      string `json:"type"` // always "base64" (no URL form — RFC AT §6)
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
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
		Model:     req.Model,
		MaxTokens: maxTokens,
		// Flatten any top-level oneOf/anyOf/allOf in a tool's input_schema
		// — Anthropic rejects those (e.g. a Zod discriminatedUnion at the
		// root of an MCP tool). No-op for schemas without a top-level
		// combinator. See schema.go.
		Tools:       sanitizeAnthropicTools(req.Tools),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		TopK:        req.TopK,
		Stop:        req.Stop,
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

	// Anthropic rejects temperature/top_p != default when an extended-thinking
	// block is attached (the API requires temperature=1 with thinking). An
	// agent that sets BOTH effort and a custom temperature would hard-400, so
	// drop the sampling overrides for this call and log the conflict (mirrors
	// the "effort dropped" signal) — thinking wins, since the operator opted
	// into reasoning. Only fires for the misconfigured both-set case.
	if w.Thinking != nil && (w.Temperature != nil || w.TopP != nil) {
		log.Printf("anthropic: dropped temperature/top_p — incompatible with extended thinking (effort) on model %q; "+
			"set effort OR temperature, not both", req.Model)
		w.Temperature = nil
		w.TopP = nil
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

	// Refuse an image to a text-only Claude model before the call — a clear
	// error beats Anthropic's opaque 400 (RFC AT §4.4/§5.1). The loop's coarse
	// gate only knows the provider supports vision; per-model nuance lives here.
	if providers.RequestHasImage(req) && !anthropicSupportsVision(req.Model) {
		return nil, fmt.Errorf("anthropic model %q does not support image input", req.Model)
	}

	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role}
		// Replay the assistant turn's extended-thinking block FIRST. When
		// extended thinking is enabled and the turn used tools, Anthropic
		// requires the prior assistant message to start with its thinking
		// block, seal included — else the tool-use continuation 400s with "a
		// final assistant message must start with a thinking block". The loop
		// stamps Reasoning (thinking text) + ReasoningSignature onto the
		// Message from EventDone; both must be present to reconstruct a block
		// the API will accept (a bare/unsigned block 400s differently).
		if m.Role == "assistant" && m.Reasoning != "" && m.ReasoningSignature != "" {
			wm.Content = append(wm.Content, wireContentBlock{
				Type:      "thinking",
				Thinking:  m.Reasoning,
				Signature: m.ReasoningSignature,
			})
		}
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
	case "image":
		return wireContentBlock{Type: "image", Source: &wireImageSource{
			Type:      "base64",
			MediaType: c.MediaType,
			Data:      c.Data,
		}}
	default: // "text"
		return wireContentBlock{Type: "text", Text: c.Text}
	}
}

// anthropicSupportsVision reports whether the named Claude model accepts image
// input. Every modern Claude family (3 / 3.5 / 3.7 / 4.x) is multimodal; the
// genuinely text-only legacy families are claude-2 and claude-instant. An
// unknown model defaults to supported — a wrong guess surfaces as a provider
// 400, not a silent drop (RFC AT §7). Capabilities() takes no model arg, so
// this is the per-model affordance (mirrors anthropicEffortBudget).
func anthropicSupportsVision(model string) bool {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude-2"), strings.Contains(m, "claude-instant"):
		return false
	default:
		return true
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

func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, credSource, credScopeID string) {
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
	// Accumulate the extended-thinking block (text + its signature_delta seal)
	// so EventDone can carry them for the next-turn replay Anthropic requires
	// on tool-use continuations. Standard (non-interleaved) thinking emits one
	// block per assistant turn, so a single text buffer + signature suffices.
	var reasoning strings.Builder
	var signature string
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
				if !processFrame(frame, &current, &stopReason, &model, &usage, &reasoning, &signature, send) {
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
	// Final done event with stop_reason + usage + the accumulated thinking
	// block (Reasoning text + Signature) for the loop to stamp onto the
	// assistant Message — Anthropic replays it on the tool-use continuation.
	// RFC AV: tag the per-call usage with which credential scope paid.
	if usage != nil {
		usage.CredentialSource = credSource
		usage.CredentialScopeID = credScopeID
	}
	send(providers.Event{
		Type:               providers.EventDone,
		StopReason:         stopReason,
		Usage:              usage,
		Reasoning:          reasoning.String(),
		ReasoningSignature: signature,
	})
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
	reasoning *strings.Builder,
	signature *string,
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
				Signature   string `json:"signature"`
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
			// EventThinking so consumers can render the trace independently of
			// EventText, AND accumulated so EventDone can carry the full
			// thinking text for the next-turn replay (below).
			reasoning.WriteString(ev.Delta.Thinking)
			if !send(providers.Event{Type: providers.EventThinking, Text: ev.Delta.Thinking}) {
				return false
			}
		case "signature_delta":
			// The cryptographic seal of the thinking block. Not user-visible
			// content, but REQUIRED to replay the block on a tool-use
			// continuation — Anthropic 400s without it. Captured for EventDone.
			*signature = ev.Delta.Signature
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
