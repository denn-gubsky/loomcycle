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

	// defaultNumCtx is Ollama's documented default context window — what the
	// server serves when neither the request nor the model's Modelfile pins
	// options.num_ctx. The driver uses it ONLY as the conservative floor to
	// advertise when the real window is unknown. Under-claiming costs an early
	// compaction; over-claiming costs a silently truncated prompt, which is the
	// failure this driver keeps re-learning. Never sent on the wire — see
	// resolveContext.
	defaultNumCtx = 4096
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
	// keyEnvName is the env-var NAME whose tenant/user credential overrides the
	// host key (RFC AR/AX), and whether this registration is keyable at all: ""
	// means keyless (ollama-local) so resolveKey bypasses the RFC AX backstop.
	// New() derives it from providerID ("ollama"→OLLAMA_API_KEY, else ""); a
	// config-declared api_key_env re-points it via SetKeyEnvName so a custom-id
	// ollama-driver provider resolves tenant overrides under its OWN var.
	keyEnvName string
	// capsPatch is an optional operator override applied inside Capabilities()
	// (RFC BF). Nil = advertise the driver defaults. ID() already comes from
	// providerID, so no separate id field is needed here.
	capsPatch *providers.CapabilityPatch
	// ctxCache memoises each model's loaded context window read from
	// /api/ps (model name → ctxCacheEntry), so the lookup costs one cheap
	// request per model, not per turn. Concurrent-safe (the Driver is
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
	// Only the hosted "ollama" registration has an operator key to protect;
	// "ollama-local" (and any other id) is keyless by default until a
	// config-declared api_key_env re-points it via SetKeyEnvName.
	keyEnvName := ""
	if providerID == "ollama" {
		keyEnvName = "OLLAMA_API_KEY"
	}
	return &Driver{providerID: providerID, apiKey: apiKey, baseURL: baseURL, http: httpClient, idleTimeout: streamOpts.IdleTimeout, keyEnvName: keyEnvName}
}

// SetKeyEnvName overrides the env-var name whose tenant/user credential shadows
// the host key (RFC AR). New() derives it from providerID; the RFC BF registry
// factory forwards a config-declared api_key_env through here so a custom-id
// ollama-driver provider's tenant overrides resolve under the SAME var the host
// key was read from. A non-empty name also makes the provider keyable (RFC AX).
func (d *Driver) SetKeyEnvName(name string) { d.keyEnvName = name }

// WithNumCtx sets the Ollama options.num_ctx that the driver includes
// on every chat request. Returns the same Driver for chaining at
// registration time:
//
//	pr.ollamaLocal = ollama.New(...).WithNumCtx(32768)
//
// Default 0 hands the decision to resolveContext, which sends the window
// Ollama reports the model is loaded with (/api/ps) and, only when even that
// is unknown, omits num_ctx so the Ollama server falls back to the model's
// Modelfile PARAMETER num_ctx, or 4096 if the Modelfile doesn't specify.
// The 4096 ceiling is a documented Ollama default and the load-bearing
// reason this knob exists: without an explicit num_ctx, Ollama silently
// truncates the prompt at 4096 tokens with no error returned (the request
// just produces a partial completion). Caught live 2026-05-15:
// employer-profiler against ollama-local/glm-4.7-flash:q4_K_M produced 190
// output tokens with stop_reason empty (not "end_turn") at 4797 input
// tokens — exactly the truncation signature.
//
// Pinning this is also the escape hatch from the /api/ps-derived window: a
// pin is sent verbatim and /api/ps is never consulted, so an operator who
// wants a smaller (or larger) window than the loaded one names it here.
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

// ctxWindow is ONE call's context window, resolved once in Call:
//
//   - send is what goes on the wire as options.num_ctx (0 = omit the field).
//   - advertise is what the loop, the UI gauge and the compaction trigger are
//     told the window is.
//
// The two live on one value, resolved at one place, because they used to
// diverge: the driver advertised whatever /api/ps reported while sending NO
// num_ctx at all, so it claimed a window the request never asked for. Ollama
// answers an over-window prompt by silently dropping the overflow, and
// autocompact_at_pct is a percentage OF the advertised window — so an
// over-claim both truncates the prompt and disables the mechanism that would
// have prevented it. Never introduce a second resolution point.
type ctxWindow struct {
	send      int
	advertise int
	source    string // "pinned" | "loaded" | "assumed" — names the number's origin in the diagnostic
}

// resolveContext resolves the window for one call. Three cases, in order:
//
//   - An explicit operator num_ctx (WithNumCtx / LOOMCYCLE_OLLAMA*_NUM_CTX)
//     wins: it is exact, and it is sent verbatim. Unchanged behaviour.
//   - Otherwise ask Ollama what the model is ACTUALLY loaded with via /api/ps
//     and send THAT back as options.num_ctx. Echoing the loaded window is a
//     no-op for the running instance (it is already loaded at that size), and
//     it makes the number we advertise the number we asked for. It also makes
//     the cache self-fulfilling rather than stale-wrong: a cached value that no
//     longer matches is re-imposed by the request instead of misreported.
//   - Neither available (model not in VRAM yet, a DIFFERENT model loaded — the
//     /api/ps lookup is name-matched so that reads as unknown — old Ollama,
//     unreachable): send NOTHING and advertise Ollama's documented default.
//     Sending defaultNumCtx here would be actively harmful: it would force a
//     4096-token load on a deployment whose Modelfile (or Ollama's own
//     auto-sizing) would have picked something larger, and since the next turn
//     reads that back from /api/ps the shrink would then pin itself. Omitting
//     keeps today's behaviour and 4096 is a floor we cannot under-deliver on,
//     so the advertised window is pessimistic but never a lie. One turn later
//     /api/ps reports the real window and the exact case above takes over.
func (d *Driver) resolveContext(req providers.Request) ctxWindow {
	// RFC CJ: a per-agent/per-run window resolved by the loop wins over the
	// construction-time num_ctx (per-provider options / env). Ollama caps it at
	// the model's trained context, so an over-large value is safe.
	if req.MaxContextTokens > 0 {
		return ctxWindow{send: req.MaxContextTokens, advertise: req.MaxContextTokens, source: "request"}
	}
	if d.numCtx > 0 {
		return ctxWindow{send: d.numCtx, advertise: d.numCtx, source: "pinned"}
	}
	if n := d.loadedContext(req.Model); n > 0 {
		return ctxWindow{send: n, advertise: n, source: "loaded"}
	}
	return ctxWindow{send: 0, advertise: defaultNumCtx, source: "assumed"}
}

// loadedContext returns the window the model is currently loaded with, read
// from /api/ps and memoised per model with a short TTL so a model reloaded at
// a different num_ctx is eventually picked up. 0 = unknown.
func (d *Driver) loadedContext(model string) int {
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
// /api/ps. Best-effort with a short timeout: any failure (loading, network,
// old Ollama) returns 0, which resolveContext reads as "unknown" and answers
// with the conservative floor. This value is now on the REQUEST path, not
// gauge-only as it was when introduced — it is sent back as options.num_ctx so
// what we advertise is what we ask for. A wrong answer therefore costs an
// unnecessary model reload at the reported size, not just a wrong gauge; that
// is the price of the two numbers being the same number.
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

// approxBytesPerToken is the usual rough tokens ≈ bytes/4 heuristic. It backs
// the over-window diagnostic ONLY: nothing is refused, trimmed or routed on the
// strength of it, so being off by a third is fine — an order of magnitude is
// what the warning is looking for.
const approxBytesPerToken = 4

// estimatePromptTokens approximates the prompt Ollama is about to see. Counts
// what the driver actually serializes into the prompt — system blocks, message
// text, tool results, and the tool schemas (a tool-heavy agent's schemas are
// often the bulk of it, and are exactly what an over-window truncation eats
// first). Image bytes are excluded: a base64 payload is orders of magnitude
// larger than the tokens a vision model charges for it, so counting it would
// fire the warning on every image request.
func estimatePromptTokens(req providers.Request) int {
	n := 0
	for _, sb := range req.System {
		n += len(sb.Text)
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			n += len(c.Text) + len(c.ToolName) + len(c.ToolInput)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n / approxBytesPerToken
}

// warnIfPromptOverWindow logs when the prompt will not fit the window this call
// actually gets. Ollama does not error on an over-window prompt — it drops the
// overflow and the model answers from a truncated view, which reads downstream
// as a confidently wrong answer rather than a failure (2026-05-15: an agent
// that could no longer see the tail of its own tool schema). A log line is the
// only signal an operator gets, so it names the three things needed to act: the
// prompt size, the window plus where that number came from, and the knob that
// raises it. Fires per call by design — every over-window turn is truncated,
// not just the first.
func (d *Driver) warnIfPromptOverWindow(req providers.Request, w ctxWindow) {
	est := estimatePromptTokens(req)
	if est <= w.advertise {
		return
	}
	log.Printf("ollama: prompt ~%d tokens exceeds the %d-token context window (%s) for model %q on %s — Ollama silently truncates the overflow; raise the window with %s",
		est, w.advertise, w.source, req.Model, d.providerID, d.numCtxSetting())
}

// numCtxSetting names the knob that pins this registration's num_ctx, for the
// diagnostic above. The two built-in ids have env vars; a config-declared
// ollama-driver provider is pinned through its own providers: entry.
func (d *Driver) numCtxSetting() string {
	switch d.providerID {
	case "ollama-local":
		return "LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX"
	case "ollama":
		return "LOOMCYCLE_OLLAMA_NUM_CTX"
	default:
		return "providers." + d.providerID + ".options.num_ctx"
	}
}

func (d *Driver) ID() string { return d.providerID }

// resolveKey returns the Bearer key to authenticate an inference request AND
// which credential scope it came from. On hosted "ollama" a tenant/user
// credential named OLLAMA_API_KEY overrides the operator's host key (RFC AR),
// and a RESTRICTED run with no override gets ErrOperatorKeyForbidden instead of
// the host key (RFC AX) — Call aborts on it. "ollama-local" is unauthenticated
// local-network: it has NO operator key to protect, so it is NEVER restricted —
// it returns its (empty) key directly, bypassing the RFC AX backstop. The
// source/scopeID ride the per-call Usage so the server can attribute spend
// (RFC AV). Model-availability probes (queryLoadedContext) stay on the operator
// key.
func (d *Driver) resolveKey(ctx context.Context) (key, source, scopeID string, err error) {
	if d.keyEnvName != "" {
		return providers.ResolveKeyOrOperator(ctx, d.keyEnvName, d.apiKey)
	}
	return d.apiKey, "operator", "", nil
}

// KeyEnvName reports the env-var name whose tenant/user credential can key this
// provider (RFC AX Layer-1 routing). Empty for a keyless registration
// ("ollama-local" by default) so it is always keyable (a restricted run may
// always route to a keyless local endpoint). A config-declared api_key_env
// (via SetKeyEnvName) makes a custom-id ollama provider keyable under its own var.
func (d *Driver) KeyEnvName() string { return d.keyEnvName }

// staticContextWindow is the model-agnostic window for Capabilities(). It
// cannot consult /api/ps (no model in hand), so an unpinned driver reports the
// conservative floor — the same floor resolveContext advertises when the real
// window is unknown, so the two never disagree about what "unknown" means.
func (d *Driver) staticContextWindow() int {
	if d.numCtx > 0 {
		return d.numCtx
	}
	return defaultNumCtx
}

func (d *Driver) Capabilities() providers.Capabilities {
	return d.capsPatch.Apply(providers.Capabilities{
		NativePromptCache: false,
		ParallelToolCalls: true, // model-dependent; we report the optimistic case
		Streaming:         true,
		// RFC CR tier-routing: an "ollama-local" registration is a self-hosted
		// backend; the hosted "ollama" (ollama.com, Bearer) is not.
		Local: d.providerID == "ollama-local",
		// Static fallback only — the authoritative per-call window rides the
		// usage event (resolveContext, which also puts it on the wire), and the
		// loop prefers that. An operator pin is the exact window; otherwise this
		// is model-agnostic, so it reports Ollama's documented default rather
		// than 0. 0 meant "unknown" to this driver but reads as "no cap" to
		// every consumer, which is the over-claim in its worst form: no gauge,
		// and no compaction ceiling at all.
		MaxContextTokens: d.staticContextWindow(),
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
	})
}

// Call sends the chat request and streams Events. The goroutine that reads
// the response closes the channel when the stream ends.
// 429 retry: Ollama OSS doesn't rate-limit (no 429 expected on a local
// server). Ollama Cloud may emit a standard Retry-After; we handle it
// defensively. Same body-bytes-preserved retry as the cloud providers.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	// Resolve the window ONCE per call and use that one value for both the
	// wire and the usage stamp: resolving twice (e.g. again at the done frame)
	// would let the cache TTL expire mid-generation and re-open the gap this
	// closes.
	window := d.resolveContext(req)
	d.warnIfPromptOverWindow(req, window)

	body, err := d.buildRequestBody(req, window.send)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)

	// RFC AR/AV: resolve the key + its owning scope ONCE per call (not per retry
	// attempt) so the header uses it and the source rides the per-call Usage.
	// RFC AX: a restricted hosted-ollama run with no override refuses here
	// (never the host key); ollama-local never refuses (no key to protect).
	apiKey, credSource, credScopeID, err := d.resolveKey(ctx)
	if err != nil {
		cancelStream()
		return nil, err
	}

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
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
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
		streamEvents(streamCtx, resp.Body, out, len(req.Tools) > 0, window.advertise, credSource, credScopeID)
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

// buildRequestBody marshals the chat request. numCtx is the window resolved by
// resolveContext for THIS call (0 = omit options.num_ctx and let Ollama pick);
// it is a parameter rather than a d.numCtx read so the value on the wire is
// provably the same value Call advertises.
func (d *Driver) buildRequestBody(req providers.Request, numCtx int) ([]byte, error) {
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

	if req.Temperature != nil || req.MaxTokens > 0 || numCtx > 0 || d.numGpu > 0 ||
		req.TopP != nil || req.TopK != nil || req.Seed != nil || len(req.Stop) > 0 {
		w.Options = &wireOptions{
			Temperature: req.Temperature,
			NumPredict:  req.MaxTokens,
			NumCtx:      numCtx,
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

// maxCtx is the window the caller resolved for this call (ctxWindow.advertise)
// and stamped on usage.MaxContextTokens; 0 → not stamped.
func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, wantTools bool, maxCtx int, credSource, credScopeID string) {
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
				usage.MaxContextTokens = maxCtx
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

	// RFC AV: tag the per-call usage with which credential scope paid.
	if usage != nil {
		usage.CredentialSource = credSource
		usage.CredentialScopeID = credScopeID
	}
	send(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: usage})
}

// tryParseToolCallsFromText attempts to parse the raw text content as
// one or more Ollama-shaped tool-call objects. Shapes covered:
//
//  1. JSON-shape (PR #26):
//     {"name":"...","arguments":{...}}
//     or an array of such objects, optionally wrapped in a ```json fence.
//
//  2. OpenAI-nested envelope:
//     {"type":"function","function":{"name":"...","arguments":{...}}}
//     (arguments may be an object OR a JSON-encoded string). Small local
//     models copy this envelope from their training when they emit a call as
//     text instead of via native tool_calls.
//
//  3. Any of the above wrapped in a single <tool_call>/<tool_result>/
//     <function_call> tag. qwen3 on Ollama copies whatever call/result FRAMING
//     it sees in the system prompt — including loomcycle's own injected
//     <tool-result …> reference blocks — and emits its call inside that tag
//     rather than as structured tool_calls. Ollama's extractor expects
//     <tool_call>, so it recovers nothing; without this, the run silently ends
//     with the call sitting in the text as an un-executed <tool_result> block.
//
//  4. Markdown-bracket shape (v0.7.x fallback):
//     [tool_use: <name>]\n{"...": ...} / [tool_use: <name> {...}] / [tool_use: <name>]
//     Observed on a few hermes / mistral fine-tunes.
//
// Returns the parsed ToolUse list when a shape matches, nil otherwise. Strict
// matching prevents false positives from text that happens to look JSON-ish or
// contains the literal phrase "tool_use" in prose: we require the ENTIRE trimmed
// content (after peeling one wrapper tag / fence) to deserialise into a
// tool-call shape.
func tryParseToolCallsFromText(text string) []*providers.ToolUse {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil
	}
	// Peel ONE tool-call wrapper tag before anything else, so a fenced or bare
	// JSON body inside <tool_result>…</tool_result> is reached (shape 3).
	s = stripToolWrapperTag(s)
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

	// Try array first — qwen3 occasionally batches multiple calls.
	if strings.HasPrefix(s, "[") && !strings.HasPrefix(s, "[tool_use:") {
		var arr []rawToolCall
		if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
			out := make([]*providers.ToolUse, 0, len(arr))
			for _, r := range arr {
				tu := r.toToolUse()
				if tu == nil {
					return nil // any malformed entry → bail; treat as prose
				}
				out = append(out, tu)
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

	// Try single JSON object (flat or OpenAI-nested).
	var r rawToolCall
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil
	}
	if tu := r.toToolUse(); tu != nil {
		return []*providers.ToolUse{tu}
	}
	return nil
}

// rawToolCall is the union of the flat and OpenAI-nested tool-call shapes a
// text-emitting model produces. toToolUse normalises whichever is present.
type rawToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// OpenAI-style nesting: {"type":"function","function":{"name":…,"arguments":…}}.
	Function *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// toToolUse normalises a rawToolCall to a ToolUse, preferring the nested
// `function` envelope when present. Returns nil when no name is set (the caller
// treats nil as "this isn't a tool call, leave it as text").
func (r rawToolCall) toToolUse() *providers.ToolUse {
	name, args := r.Name, r.Arguments
	if r.Function != nil && r.Function.Name != "" {
		name, args = r.Function.Name, r.Function.Arguments
	}
	if name == "" {
		return nil
	}
	return &providers.ToolUse{Name: name, Input: normalizeToolArgs(args)}
}

// normalizeToolArgs returns a JSON object for a call's arguments. Absent → "{}".
// A JSON-STRING (the OpenAI wire form often double-encodes arguments, e.g.
// "{\"q\":\"x\"}") is unquoted to the object it encodes so the dispatcher sees an
// object either way; anything else passes through untouched.
func normalizeToolArgs(args json.RawMessage) json.RawMessage {
	a := bytes.TrimSpace(args)
	if len(a) == 0 {
		return json.RawMessage("{}")
	}
	if a[0] == '"' {
		var str string
		if err := json.Unmarshal(a, &str); err == nil {
			if t := strings.TrimSpace(str); t != "" {
				return json.RawMessage(t)
			}
			return json.RawMessage("{}")
		}
	}
	return a
}

// toolWrapperTags are the call/result framing tags a text-emitting model may put
// around its call. Underscore and hyphen spellings both occur; loomcycle's own
// injected reference blocks use <tool-result …>.
var toolWrapperTags = []string{"tool_call", "tool-call", "tool_result", "tool-result", "function_call", "function-call"}

// stripToolWrapperTag removes ONE surrounding wrapper tag (with or without
// attributes) and returns the trimmed inner text; returns s unchanged when it is
// not wrapped. The plural <tool_calls> is deliberately NOT matched (the attribute
// guard rejects the trailing "s"), so a genuine tool_calls container is left for
// the JSON parser rather than mis-peeled.
func stripToolWrapperTag(s string) string {
	lower := strings.ToLower(s)
	for _, tag := range toolWrapperTags {
		if !strings.HasPrefix(lower, "<"+tag) {
			continue
		}
		gt := strings.IndexByte(s, '>')
		if gt < 0 {
			continue
		}
		// Between the tag name and '>' must be nothing, a self-close, or an
		// attribute run (leading space) — otherwise "<tool_calls>" would match
		// the "tool_call" tag.
		if after := s[len("<"+tag):gt]; after != "" && after != "/" && !strings.HasPrefix(after, " ") {
			continue
		}
		inner := s[gt+1:]
		if li := strings.LastIndex(strings.ToLower(inner), "</"+tag+">"); li >= 0 {
			inner = inner[:li]
		}
		return strings.TrimSpace(inner)
	}
	return s
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
