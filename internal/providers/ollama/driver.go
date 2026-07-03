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
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
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
	numGpu      int // 0 = omit (Ollama auto-detects GPU layers); >0 forces offload
	// ctxCache memoises each model's loaded context window read from
	// /api/ps (model name → ctxCacheEntry), so the gauge lookup costs one
	// cheap request per model, not per turn. Concurrent-safe (the Driver is
	// shared across runs).
	ctxCache sync.Map
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

// WithNumGpu sets the Ollama options.num_gpu that the driver includes on
// every chat request — the number of model layers offloaded to the GPU.
// Returns the same Driver for chaining at registration time:
//
//	pr.ollamaLocal = ollama.New(...).WithNumGpu(99)
//
// Default 0 = don't set, letting Ollama auto-detect how many layers fit on
// the GPU. The knob exists because that auto-detection underestimates VRAM
// on some setups (notably integrated/APU GPUs), silently running inference
// on the CPU. Forcing a high value (99 = "all layers") makes Ollama offload
// the whole model; Ollama clamps to the model's actual layer count, so an
// over-large value is safe. A literal 0 must NOT be sent — that would force
// CPU-only — which is why both this setter and the omitempty tag guard it.
//
// Not safe to call concurrently with Call(); intended for registration.
func (d *Driver) WithNumGpu(n int) *Driver {
	if n > 0 {
		d.numGpu = n
	}
	return d
}

// ctxCacheEntry caches a model's loaded context window (from /api/ps) with a
// short TTL so a model reloaded at a different num_ctx is eventually picked up.
type ctxCacheEntry struct {
	ctx int
	at  time.Time
}

const ctxCacheTTL = 5 * time.Minute

// contextWindow returns the model's effective input window to stamp on the
// usage event so the UI context gauge is truthful for local models.
//
//   - An explicit operator num_ctx (WithNumCtx / LOOMCYCLE_OLLAMA*_NUM_CTX)
//     wins — it's exact and is precisely what we send as options.num_ctx.
//   - Otherwise we ask Ollama what the model is ACTUALLY loaded with via
//     /api/ps. Ollama only publishes context_length once the model is in VRAM
//     (it's absent while loading), so this returns 0 ("unknown") until the
//     first turn after a load, then the real window. Cached per model.
//
// Called at the stream's done frame — the model is loaded by then — so the
// turn that loaded the model already reports the real window; later turns hit
// the cache. 0 leaves the gauge showing only the absolute used size, unchanged.
func (d *Driver) contextWindow(model string) int {
	if d.numCtx > 0 {
		return d.numCtx
	}
	if v, ok := d.ctxCache.Load(model); ok {
		if e := v.(ctxCacheEntry); e.ctx > 0 && time.Since(e.at) < ctxCacheTTL {
			return e.ctx
		}
	}
	n := d.queryLoadedContext(model)
	if n > 0 {
		d.ctxCache.Store(model, ctxCacheEntry{ctx: n, at: time.Now()})
	}
	return n
}

// queryLoadedContext reads the loaded model's context_length from Ollama's
// /api/ps. Best-effort with a short timeout: this only feeds the gauge, never
// correctness, so any failure (loading, network, old Ollama) returns 0.
func (d *Driver) queryLoadedContext(model string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "GET", d.baseURL+"/api/ps", nil)
	if err != nil {
		return 0
	}
	if d.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)
	}
	resp, err := d.http.Do(httpReq)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var body struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0
	}
	for _, m := range body.Models {
		if m.Name == model || m.Model == model {
			return m.ContextLength
		}
	}
	return 0
}

func (d *Driver) ID() string { return d.providerID }

// callKey returns the Bearer key for a hosted-ollama.com inference request: a
// tenant/user credential named OLLAMA_API_KEY overrides the operator host key
// when the run has one (RFC AR), else the host key. Only "ollama" (hosted)
// authenticates — "ollama-local" is unauthenticated local-network, so no
// override applies there.
func (d *Driver) callKey(ctx context.Context) string {
	if d.providerID != "ollama" {
		return d.apiKey
	}
	if k, ok := providers.ResolveCredential(ctx, "OLLAMA_API_KEY"); ok {
		return k
	}
	return d.apiKey
}

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: false,
		ParallelToolCalls: true, // model-dependent; we report the optimistic case
		Streaming:         true,
		// Static fallback only. The authoritative per-call window is set on
		// the usage event by the stream path (contextWindow → /api/ps reads
		// the model's ACTUAL loaded context); the loop prefers that and falls
		// back here. When the operator pins options.num_ctx (WithNumCtx /
		// LOOMCYCLE_OLLAMA*_NUM_CTX) that is the exact window; else 0 here
		// ("unknown") and the per-call /api/ps value fills it in once loaded.
		MaxContextTokens: d.numCtx,
		SupportsThinking: true,
		// The effort hint drives Ollama's top-level `think` flag (see
		// buildRequestBody): medium/high enable a reasoning model's
		// thinking trace, low disables it, empty leaves the model default.
		// Ollama populates message.thinking only when think=true, which is
		// then surfaced as EventThinking. SupportsEffort=true so the loop
		// forwards the hint rather than logging it as dropped. The model
		// must be thinking-capable (qwen3, gemma4, deepseek-r1, …).
		SupportsEffort: true,
		// Vision depends on the pulled model (llava, llama3.2-vision, …).
		// Report true and treat model choice as the operator's responsibility
		// (RFC AT §5.4); a non-vision model's failure surfaces via the existing
		// provider-fallback error rather than a silent drop.
		SupportsVision: true,
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
		// v0.10.0 OTEL: one loomcycle.provider.call span per attempt.
		// d.providerID is "ollama" (cloud) or "ollama-local" — the
		// resolver distinguishes the two; span attribute mirrors.
		spanCtx, span := lcotel.RecordProviderCall(attemptCtx, lcotel.ProviderCallAttrs{
			Provider: d.providerID,
			Model:    req.Model,
			Effort:   req.Effort,
		})
		defer span.End()
		httpReq, err := http.NewRequestWithContext(spanCtx, "POST", d.baseURL+"/api/chat", bytes.NewReader(body))
		if err != nil {
			lcotel.SetSpanError(span, err)
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/x-ndjson")
		if key := d.callKey(spanCtx); key != "" {
			httpReq.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := d.http.Do(httpReq)
		if err != nil {
			lcotel.SetSpanError(span, err)
		} else if resp != nil && resp.StatusCode >= 400 {
			lcotel.SetSpanErrorMessage(span, "http "+resp.Status)
		}
		return resp, err
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
		// maxCtxFn is evaluated lazily at the done frame (model loaded by
		// then) so the usage event carries the model's real loaded window.
		streamEvents(streamCtx, resp.Body, out, len(req.Tools) > 0, func() int { return d.contextWindow(req.Model) })
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
	// Think toggles a reasoning model's thinking trace via Ollama's /api/chat
	// `think` field. nil omits it (model default); set from the agent's effort
	// hint. Ollama populates message.thinking only when this is true, and
	// errors if the resolved model isn't thinking-capable.
	Think *bool `json:"think,omitempty"`
}

type wireOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"` // Ollama's name for max_tokens
	NumCtx      int      `json:"num_ctx,omitempty"`     // input-window size; 0 = Ollama server default
	NumGpu      int      `json:"num_gpu,omitempty"`     // GPU layers to offload; 0 = omit (a literal 0 forces CPU)
	// Ollama options sampling knobs. frequency/presence_penalty exist on some
	// models but vary; we plumb the broadly-supported set.
	TopP *float64 `json:"top_p,omitempty"`
	TopK *int     `json:"top_k,omitempty"`
	Seed *int     `json:"seed,omitempty"`
	Stop []string `json:"stop,omitempty"`
}

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
	// Images carries inline base64 image data on a user message (RFC AT).
	// Ollama's /api/chat takes images: [base64] alongside content; vision
	// depends on the pulled model (llava, llama3.2-vision, …).
	Images []string `json:"images,omitempty"`
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

	// Map the effort hint to Ollama's `think` flag: medium/high enable the
	// reasoning trace, low disables it, empty leaves the model default. The
	// model must be thinking-capable (qwen3, gemma4, deepseek-r1, …); Ollama
	// errors on `think` for models that can't reason, so this is operator
	// opt-in via effort.
	switch req.Effort {
	case "medium", "high":
		think := true
		w.Think = &think
	case "low":
		think := false
		w.Think = &think
	}
	// Opt-in diagnostic (LOOMCYCLE_OLLAMA_DEBUG_THINK=1): log exactly what
	// reaches the driver, so an operator debugging "no thinking trace" can
	// confirm whether the effort hint arrived and whether `think` was set on
	// the wire. Off by default (a per-request log line would otherwise be
	// noise). Non-secret — model name + effort only.
	if os.Getenv("LOOMCYCLE_OLLAMA_DEBUG_THINK") == "1" {
		log.Printf("ollama think-diag: provider=%s model=%q effort=%q think_set=%v",
			d.providerID, req.Model, req.Effort, w.Think != nil)
	}

	if req.Temperature != nil || req.MaxTokens > 0 || d.numCtx > 0 || d.numGpu > 0 ||
		req.TopP != nil || req.TopK != nil || req.Seed != nil || len(req.Stop) > 0 {
		w.Options = &wireOptions{
			Temperature: req.Temperature,
			NumPredict:  req.MaxTokens,
			NumCtx:      d.numCtx,
			NumGpu:      d.numGpu,
			TopP:        req.TopP,
			TopK:        req.TopK,
			Seed:        req.Seed,
			Stop:        req.Stop,
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

	// user role: split tool_result into role:"tool" entries; text + image
	// blocks form one user message (images attach to it as images: [base64]).
	var out []wireMessage
	var userText strings.Builder
	var images []string
	for _, c := range m.Content {
		switch c.Type {
		case "tool_result":
			out = append(out, wireMessage{Role: "tool", Content: c.Text})
		case "text":
			if userText.Len() > 0 {
				userText.WriteString("\n")
			}
			userText.WriteString(c.Text)
		case "image":
			images = append(images, c.Data)
		}
	}
	if userText.Len() > 0 || len(images) > 0 {
		out = append([]wireMessage{{Role: "user", Content: userText.String(), Images: images}}, out...)
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
	Model   string  `json:"model"`
	Message message `json:"message"`
	Done    bool    `json:"done"`
	// Error is Ollama's in-stream fault. /api/chat commits a 200 and then, on
	// a mid-generation failure (OOM, model unload, late context-overflow),
	// writes a final NDJSON line {"error":"..."} with no done:true. Captured so
	// streamEvents can surface it instead of ending as a silent clean stop.
	Error      string `json:"error"`
	DoneReason string `json:"done_reason"`

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

// maxCtxFn (optional) returns the model's effective context window; called
// once at the done frame to stamp usage.MaxContextTokens. nil → not stamped.
func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, wantTools bool, maxCtxFn func() int) {
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

	// coalesceBuf batches consecutive content deltas into phrase-sized
	// EventText emissions. Ollama (both local /api/chat and ollama.com
	// cloud) streams one token per chunk — pre-fix the events table
	// recorded one row per token, the SSE wire emitted one frame per
	// token, and the Web UI rendered one card per token. Mirrors the
	// openai driver's 64-byte coalesce (PR #28); we land it separately
	// here because deepseek-v4-pro served via the ollama-cloud
	// subscription path goes through THIS driver, not the openai/deepseek
	// pair, so PR #28's fix didn't reach it.
	//
	// Flush points: ≥64 bytes accumulated, newline in the current
	// delta (preserve paragraph breaks), before any tool_call emission
	// (preserves the "text precedes tool_call" ordering loop.go expects),
	// and end-of-stream / scanner-error / done frame.
	var coalesceBuf strings.Builder
	const textCoalesceMin = 64
	flushText := func() bool {
		if coalesceBuf.Len() == 0 {
			return true
		}
		s := coalesceBuf.String()
		coalesceBuf.Reset()
		return send(providers.Event{Type: providers.EventText, Text: s})
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var c chunk
		if err := json.Unmarshal(line, &c); err != nil {
			continue
		}
		// In-stream error frame ({"error":"..."} with no done:true). Flush any
		// text already delivered (like the scanner-error path), then surface an
		// EventError and stop — WITHOUT this the loop sees only a clean
		// EventDone{StopReason:""} and treats a failed generation as success
		// with truncated/empty output (no error, no fallback).
		if c.Error != "" {
			_ = flushText()
			send(providers.Event{Type: providers.EventError, Error: "ollama: " + c.Error})
			return
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
			coalesceBuf.WriteString(c.Message.Content)
			if coalesceBuf.Len() >= textCoalesceMin || strings.ContainsRune(c.Message.Content, '\n') {
				if !flushText() {
					return
				}
			}
		}
		if len(c.Message.ToolCalls) > 0 {
			// Flush buffered text BEFORE tool_call emissions so the loop's
			// "text precedes tool_call within an iteration" invariant holds
			// (loop.go:629 prepends iterText into the assistant block).
			if !flushText() {
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
				if maxCtxFn != nil {
					usage.MaxContextTokens = maxCtxFn()
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// Flush any buffered text before the error event so bytes the
		// wire delivered aren't silently dropped on mid-stream read
		// failure. Mirrors the openai driver's same-position flush.
		_ = flushText()
		send(providers.Event{Type: providers.EventError, Error: "stream read: " + err.Error()})
		return
	}

	// End-of-stream flush. Covers the common case where the final
	// content delta brought the buffer to <64 bytes and contained no
	// newline (e.g. a short final sentence). Without this, that tail
	// would be silently dropped.
	if !flushText() {
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
