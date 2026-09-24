// Package providers defines the LLM provider abstraction.
//
// A Provider talks one provider's HTTP API and streams a unified Event channel
// back. The agent loop in internal/loop is provider-agnostic; everything
// provider-specific (auth, request shape, SSE framing, cache_control placement)
// lives behind this interface.
package providers

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/errkind"
)

// Provider is one LLM endpoint. Implementations are stateless across calls;
// per-call state lives in Request and the returned channel.
type Provider interface {
	ID() string
	Capabilities() Capabilities
	Call(ctx context.Context, req Request) (<-chan Event, error)

	// Probe is a lightweight reachability + auth check. Returns nil
	// iff the provider responds successfully (auth valid, network
	// reachable). Used by the resolver's startup probe and periodic
	// re-probe loop. Implementations should hit a cheap endpoint
	// (typically GET /v1/models or /api/tags), respect the passed
	// context's deadline, and return without retry — the caller
	// owns retry/backoff policy.
	//
	// Probe is allowed to share its work with ListModels: drivers
	// commonly implement Probe by calling ListModels and treating a
	// non-empty result as "healthy" (the network round-trip
	// already proves reachability + auth). The interface keeps
	// them separate so callers that only need health (no model
	// list) don't pay the marshalling cost.
	Probe(ctx context.Context) error

	// ListModels returns the wire aliases the provider currently
	// serves. Used by the resolver to populate the per-model
	// availability matrix (Listed flag in ModelStatus). The exact
	// format depends on the provider's models endpoint:
	//   - Anthropic / OpenAI / DeepSeek: /v1/models response data[].id
	//   - Ollama: /api/tags response models[].name
	//
	// Returns an empty slice (not nil) when the endpoint succeeds
	// with zero models — that's a different signal than "probe
	// failed" and the resolver treats it as "provider reachable
	// but every model offline".
	ListModels(ctx context.Context) ([]string, error)
}

// KeyedProvider is an OPTIONAL interface an LLM provider implements when it
// authenticates inference with an operator API key that a tenant can override
// via its own CredentialDef (RFC AR) — and that a restricted run (RFC AX)
// therefore must be able to key itself. KeyEnvName returns the well-known
// env-var NAME the driver's key resolution uses (the SAME literal its resolveKey
// passes to ResolveKeyOrOperator, e.g. "ANTHROPIC_API_KEY").
//
// RFC AX Layer-1 credential-aware routing tests, per candidate provider, whether
// the tenant/user has a CredentialDef for this name. A provider that needs no
// operator key (ollama-local / code-js / mock) does NOT implement this interface
// (type-assert miss → "" → always keyable): a restricted run may always route to
// a keyless provider. The hosted ollama driver returns "" for its ollama-local
// registration for the same reason.
type KeyedProvider interface {
	KeyEnvName() string
}

// ThinkingDowngrader is an OPTIONAL interface a Provider implements when its
// thinking-class models require provider-produced reasoning state
// (reasoning_content) on every assistant turn of the input history. DeepSeek's
// deepseek-reasoner / *-pro reject an assistant turn lacking it with HTTP 400
// "reasoning_content ... must be passed back to the API".
//
// On a cross-provider fallback the history carries assistant turns that have
// no such state — a different provider produced them, or the loop's reasoning
// strip zeroed them (incl. the deepseek→other→deepseek-reasoner bounce). The
// loop calls NonThinkingSibling on the fallback target and, if it downgrades,
// runs the remaining iterations on the non-thinking model rather than letting
// the request 400. Providers that tolerate a reasoning-less history (Anthropic,
// Gemini, OpenAI o-series) simply don't implement this interface.
type ThinkingDowngrader interface {
	// NonThinkingSibling returns the non-thinking model to use in place of a
	// thinking-class model that cannot consume a foreign/reasoning-stripped
	// history, and true; or ("", false) when model is not a thinking-class
	// model needing a downgrade.
	NonThinkingSibling(model string) (string, bool)
}

// Capabilities tells the loop what the provider can and can't do, so the loop
// can degrade gracefully instead of sending unsupported fields.
type Capabilities struct {
	NativePromptCache bool // Anthropic cache_control
	ParallelToolCalls bool
	Streaming         bool
	MaxContextTokens  int
	SupportsThinking  bool

	// SupportsVision signals that this provider can accept image content
	// blocks (RFC AT) on at least some of its models. The loop uses it as a
	// coarse pre-call gate: an image sent to a SupportsVision=false provider
	// (or a fallback that routed there) fails loudly with an EventError
	// instead of the image being silently dropped or the provider returning
	// an opaque 400. Per-model nuance (a legacy non-vision model on an
	// otherwise vision-capable provider) is enforced inside the driver via a
	// helper (e.g. anthropicSupportsVision(model)), mirroring how
	// SupportsEffort is coarse here but refined per-call in the driver.
	SupportsVision bool

	// SupportsToolChoice signals that this provider has a WIRE parameter for
	// constraining which tool the model may call (RFC DG). Coarse, like
	// SupportsVision: per-model and per-request nuance is refined inside the
	// driver, because the incompatibilities are not provider-wide — Anthropic
	// rejects a forced choice only when extended thinking is attached.
	//
	// ⚠️ FALSE IS NOT AN ERROR. Ollama has no such parameter on /api/chat or on
	// its OpenAI shim, and a run against it is not refused: the request is
	// dropped and the caller proceeds unforced. Forcing is an OPTIMISATION of a
	// contract the prompt already states, never the only thing holding it up —
	// the stateful loop re-prompts a model that answers the wrong way either
	// way. A design that refused instead would make every local-model agent
	// unrunnable to buy a guarantee it never had.
	SupportsToolChoice bool

	// SupportsStructuredOutput signals a wire parameter that constrains the
	// answer to a JSON schema (RFC DI). Coarse like SupportsToolChoice; a
	// driver whose support varies by model (or by whether tools ride along)
	// refines it through ModelStructuredOutputEnforcer. False is not an error:
	// the format is dropped from the request and the run reports it.
	SupportsStructuredOutput bool

	// StructuredOutputNative: the API also SHOWS the schema to the model
	// (Anthropic, OpenAI, Gemini build it into the prompt themselves). A
	// grammar-only backend (Ollama format, vLLM, llama.cpp) merely constrains
	// the tokens, so the model writes a well-formed object without knowing
	// what the fields mean; for those, and for every target that cannot
	// enforce the schema, the loop puts the schema in the system prompt.
	StructuredOutputNative bool

	// SupportsEffort signals that the driver translates Request.Effort
	// into a native wire parameter when set. Anthropic maps it to a
	// `thinking.budget_tokens` block; OpenAI to `reasoning_effort`;
	// DeepSeek (via the OpenAI wrapper) inherits OpenAI's behaviour;
	// Ollama is a no-op (no operator-controlled thinking budget today).
	//
	// SupportsEffort=true does NOT mean every model on this provider
	// honours the hint — haiku-4-5 and gpt-5.4-mini, for example, are
	// non-reasoning models that the provider's API will reject (or
	// silently ignore) the hint on. The driver decides per-call whether
	// to actually attach the wire param based on the model name. The
	// flag is purely informational — the loop uses it to log when an
	// agent declared effort but landed on a SupportsEffort=false
	// provider, so the operator sees "effort dropped" rather than
	// silently believing the agent thought hard.
	SupportsEffort bool

	// Local signals that this provider is a self-hosted / local-inference backend
	// (ollama-local, vllm, llamacpp) rather than a hosted frontier API. RFC CR
	// tier-routing (`context.mode: auto`) reads it to pick the retention mode:
	// local → reasoning-recap (L1, schema-free — safe for a weaker model), a
	// frontier provider → structured state (L2, which needs reliable structured
	// output). Purely a routing hint; false for every hosted/synthetic driver.
	Local bool

	// UnboundedIterations signals that this provider's loop turns are the
	// internal tool-dispatch steps of ONE logical run (not model reasoning
	// turns), so the loop's MaxIterations soft-cap does not apply — the run is
	// bounded by the provider's own wall-clock timeout instead. Set only by
	// the synthetic code-js provider (RFC J): a code-agent's run() may make an
	// arbitrary number of SEQUENTIAL tool calls, each one a loop turn, and
	// capping that at 16 is unusable. The provider enforces a run-level
	// deadline (LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS) so disabling the
	// iteration cap cannot produce an unbounded run. The loop keeps a high
	// hard ceiling as a pure runaway backstop. False for every LLM driver,
	// where MaxIterations remains the runaway-tool-use guard.
	UnboundedIterations bool

	// MetadataViaInput signals that this provider delivers the run's
	// non-secret metadata to the agent STRUCTURALLY as part of its input
	// (the code-js provider surfaces it as input.metadata /
	// input.payload_metadata), so the run-build path must NOT also serialize
	// metadata into prompt segments — for such a provider a user-role
	// metadata block would shadow the latest-user-text it reads as the
	// prompt. False for every LLM driver, where metadata IS delivered via
	// prompt segments (the only channel an LLM agent has). Set only by the
	// synthetic code-js provider today; the generalisation is so a future
	// structured-input provider doesn't have to be special-cased by id.
	MetadataViaInput bool
}

// ToolChoice is the provider-neutral form of "you must call a tool".
//
// Deliberately not a string: the named form carries a second field, and a
// stringly-typed "tool:emit_state" would put parsing in four drivers instead of
// the one place that builds it.
type ToolChoice struct {
	// Mode is "" (= auto, the zero value and today's behaviour), "auto",
	// "none", "required" (any tool, the model picks) or "tool" (a named one).
	Mode string
	// Name is the tool that must be called. Only meaningful with Mode "tool";
	// a driver ignores it otherwise rather than guessing.
	Name string
}

// ToolChoice modes. This is loomcycle's own vocabulary, not any provider's —
// each driver maps it, and two providers spell the same intent differently
// ("required" against {"type":"any"}).
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceTool     = "tool"
)

// Forces reports whether this choice constrains the model at all. auto and the
// zero value do not, so a driver skips the whole mapping for them and stays
// byte-identical to its pre-RFC-DG wire body.
func (tc ToolChoice) Forces() bool {
	switch tc.Mode {
	case ToolChoiceNone, ToolChoiceRequired:
		return true
	case ToolChoiceTool:
		return tc.Name != ""
	default:
		return false
	}
}

// ModelToolChoiceEnforcer is implemented by a driver whose ability to ENFORCE a
// tool choice depends on the model and on the effort hint that decides its
// thinking mode — which Capabilities, being per-provider, cannot express. The
// Anthropic driver drops a forced choice for models that refuse one and under
// manual (budgeted) thinking; this is how the loop learns it will.
type ModelToolChoiceEnforcer interface {
	EnforcesToolChoice(model, effort string, tc ToolChoice) bool
}

// EnforcesToolChoice reports whether a call with this choice will actually be
// held to it by (p, model, effort). A choice that constrains nothing is always
// enforced; otherwise a driver's per-model answer wins, then the coarse
// Capabilities bit.
func EnforcesToolChoice(p Provider, model, effort string, tc ToolChoice) bool {
	if !tc.Forces() {
		return true
	}
	if e, ok := p.(ModelToolChoiceEnforcer); ok {
		return e.EnforcesToolChoice(model, effort, tc)
	}
	return p.Capabilities().SupportsToolChoice
}

// OutputFormat is the provider-neutral answer schema (RFC DI). Each driver
// maps it to its own field — or leaves it off when it cannot apply it, which
// EnforcesStructuredOutput lets the loop learn ahead of time.
type OutputFormat struct {
	// Name labels the schema where a provider needs one (OpenAI).
	Name string
	// Schema is the JSON Schema, root type object.
	Schema json.RawMessage
}

// ModelStructuredOutputEnforcer is implemented by a driver whose ability to
// apply an OutputFormat depends on the model, or on whether the request also
// carries tools (Gemini combines the two only on its 3 series).
type ModelStructuredOutputEnforcer interface {
	EnforcesStructuredOutput(model string, hasTools bool) bool
}

// EnforcesStructuredOutput reports whether (p, model) will hold the answer to
// the schema: the driver's per-model answer when it has one, else the
// Capabilities bit.
func EnforcesStructuredOutput(p Provider, model string, hasTools bool) bool {
	if e, ok := p.(ModelStructuredOutputEnforcer); ok {
		return e.EnforcesStructuredOutput(model, hasTools)
	}
	return p.Capabilities().SupportsStructuredOutput
}

// Request is one round-trip to the provider. The loop builds a fresh Request
// for each iteration, appending the previous tool_result(s) to Messages.
type Request struct {
	Model       string         `json:"model"`
	System      []ContentBlock `json:"system,omitempty"`
	Messages    []Message      `json:"messages"`
	Tools       []ToolSpec     `json:"tools,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	Stream      bool           `json:"stream"`

	// MaxContextTokens is the per-agent context WINDOW the loop resolved for
	// this run (RFC CJ; per-run > per-agent), distinct from MaxTokens (output).
	// Translate-or-drop, like Effort: the Ollama driver applies it as
	// options.num_ctx (preferred over its construction-time num_ctx); every
	// other driver ignores it — the window there is model-fixed and the loop
	// handles the advertised-window budget itself. 0 = no per-agent override.
	MaxContextTokens int `json:"max_context_tokens,omitempty"`

	// Per-agent LLM sampling knobs (config.Sampling, resolved per-run >
	// per-agent and mapped onto these flat fields by the loop). Each driver
	// applies the ones its provider supports and DROPS the rest (the same
	// translate-or-drop contract as Effort) — nil/empty = provider default.
	// Anthropic also drops Temperature/TopP when it attaches a thinking block
	// (the API rejects temperature!=1 with thinking).
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`

	// Effort is the reasoning-effort hint: "low" / "medium" / "high"
	// or empty (= no hint, driver default). Drivers translate it to
	// their native parameter where supported (Anthropic
	// thinking.budget_tokens; OpenAI reasoning_effort; DeepSeek V4
	// thinking-mode toggle), silently ignored on models without a
	// reasoning surface (haiku-4-5, gpt-5.4-mini, etc.). The
	// translation lands in PR 3 of the resolve-matrix series; PR 1
	// adds the field but drivers ignore it.
	Effort string `json:"-"`

	// ToolChoice constrains WHETHER and WHICH tool the model may call (RFC DG).
	// The zero value is auto — today's behaviour, byte-identical — so an
	// unopted request is unchanged on every driver.
	//
	// Each driver translates it into its own shape (Anthropic tool_choice,
	// OpenAI tool_choice, Gemini toolConfig.functionCallingConfig) and a driver
	// with no such parameter DROPS it. Dropping is the documented outcome, not
	// a failure: see Capabilities.SupportsToolChoice.
	ToolChoice ToolChoice `json:"-"`
	// OutputFormat constrains the answer to a JSON schema (RFC DI). nil = free
	// text. A driver that cannot apply it sends nothing for it.
	OutputFormat *OutputFormat `json:"-"`

	// OnEvent, when set, is called for events that occur BEFORE the
	// response channel exists — most importantly, EventRetry frames
	// fired during a 429 retry sleep. Optional; the loop populates this
	// from RunOptions.OnEvent so adapter consumers see retry telemetry
	// live on the same SSE stream as the main response. Marshalling
	// callers should ignore (json:"-").
	//
	// Callback contract: the driver invokes OnEvent synchronously from
	// inside Call() (before the response channel exists). The callback
	// MUST NOT block — the driver is mid-retry-sleep and a slow callback
	// extends the rate-limit wait. SSE writes are fine; do not perform
	// network IO from here.
	OnEvent func(Event) `json:"-"`
}

// Message is one turn in the conversation.
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []ContentBlock `json:"content"`

	// Reasoning carries the assistant turn's reasoning trace when the
	// provider's model emits one. DeepSeek V4 Pro and the deepseek-
	// reasoner family return `reasoning_content` alongside `content`
	// when thinking mode is on; the API requires that string to be
	// echoed back on subsequent turns or the next request 400s with
	// "reasoning_content in the thinking mode must be passed back."
	//
	// Empty for non-thinking models / non-DeepSeek providers; the
	// `omitempty` keeps it out of the wire body for everyone else
	// (vanilla OpenAI ignores unknown fields anyway, but no point
	// sending bytes that mean nothing).
	Reasoning string `json:"reasoning_content,omitempty"`

	// ReasoningSignature is the cryptographic seal Anthropic returns for an
	// extended-thinking block (the signature_delta). When extended thinking is
	// enabled AND the turn uses tools, Anthropic requires the previous
	// assistant turn to be replayed with its thinking block INCLUDING this
	// signature, verbatim — otherwise the continuation 400s ("a final
	// assistant message must start with a thinking block"). The Anthropic
	// driver pairs it with Reasoning (the thinking text) to reconstruct that
	// block in buildRequestBody. Empty for non-Anthropic / non-thinking turns;
	// zeroed alongside Reasoning on a cross-provider fallback (a signature is
	// only valid for the model that produced it).
	ReasoningSignature string `json:"reasoning_signature,omitempty"`
}

// ContentBlock is one piece of message content. Type discriminates the union.
//
//   - "text"        : plain text. Text field set.
//   - "tool_use"    : assistant requests a tool call. ToolUseID, ToolName, ToolInput set.
//   - "tool_result" : user-side result of a previous tool_use. ToolUseID, Text or ToolResult set.
//
// Cacheable is an Anthropic-specific hint: when true and the provider's
// Capabilities().NativePromptCache is true, the driver places a cache_control
// breakpoint at the end of this block.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Cacheable bool            `json:"-"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolName  string          `json:"name,omitempty"`
	ToolInput json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// Image fields (Type == "image", RFC AT). MediaType is a whitelisted
	// image media type (image/png|jpeg|gif|webp); Data is the raw base64 of
	// the image bytes with NO "data:" prefix. Each driver serializes these
	// into its own wire form (Anthropic source block / OpenAI image_url
	// data-URI / Gemini inlineData / Ollama images[]). Empty for every
	// non-image block.
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

// RequestHasImage reports whether any message in the request carries an image
// content block (RFC AT). Drivers whose vision support is per-model (Anthropic,
// OpenAI) use it to refuse an image to a known text-only model with a clear
// error before the HTTP call, rather than letting the provider return an opaque
// 400.
func RequestHasImage(req Request) bool {
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Type == "image" {
				return true
			}
		}
	}
	return false
}

// ToolSpec describes one tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Help is the per-run instruction to read this tool's help, already
	// appended to Description. Kept apart as well so a surface that shortens
	// descriptions (the stateful loop's one-line tool list) can drop the long
	// description and still keep the instruction. Never sent on the wire.
	Help string `json:"-"`
}

// EventType discriminates the streamed Event union. The loop emits these on
// the channel returned by Call(); the HTTP layer forwards them as SSE.
type EventType string

const (
	EventStarted    EventType = "started"
	EventText       EventType = "text"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventUsage      EventType = "usage"
	EventDone       EventType = "done"
	EventError      EventType = "error"
	// EventRetry signals that a provider returned 429 and the driver is
	// about to sleep before retrying. Surfaced live during the retry sleep
	// (not after) so adapter consumers can show "waiting on rate limit"
	// to end users. The retry itself is invisible to the agent loop —
	// EventRetry is purely informational.
	EventRetry EventType = "retry"
	// EventThinking carries one chunk of the model's reasoning trace,
	// emitted live as the underlying provider streams it. Distinct from
	// EventText so consumers can render or hide reasoning independently
	// of the model's user-facing answer.
	//
	// Sources per provider:
	//   - Anthropic: thinking_delta deltas from extended-thinking blocks
	//   - OpenAI / DeepSeek: delta.reasoning_content fragments
	//     (DeepSeek V4 Pro / deepseek-reasoner / o-series / GPT-5)
	//   - Ollama: message.thinking field on each chunk
	//     (qwen3, deepseek-r1, hermes3, etc. — drivers that surface
	//     reasoning out-of-band from the user-visible content)
	//
	// EventThinking is purely informational on the loop side — the loop
	// does not echo it back to the next iteration. The
	// echo-the-trace-back contract (DeepSeek's "reasoning_content must
	// be passed back" requirement) is still served by EventDone.Reasoning,
	// which carries the full concatenated trace alongside the streaming
	// chunks. Adapters that only want the final trace can ignore
	// EventThinking and read EventDone.Reasoning; adapters that want
	// live progress should consume both.
	EventThinking EventType = "thinking"

	// EventProviderFallback signals a v0.8.2 runtime fallback fired
	// after a provider call returned a retryable error
	// (ErrorClassRetryable per internal/providers/errclass.go) and
	// the run's user_tier policy permitted the climb. The loop has
	// already swapped to a fresh (provider, model) on the next-in-
	// queue, re-resolved against the tier's candidate list with the
	// failed provider marked stalled. The next iteration uses the
	// new provider; this event is purely informational so adapters
	// can show "switched to %s after %s 429" without a separate API
	// call to inspect resolver state.
	//
	// The Fallback field carries the structured payload.
	EventProviderFallback EventType = "provider_fallback"

	// EventCacheInvalidated signals that a v0.8.2 runtime fallback
	// dropped a provider-specific cache (most notably Anthropic's
	// cache_control breakpoints) when switching to a different
	// provider. The cost retro view should treat this run's
	// downstream iterations as cache-cold for the new provider.
	// Purely informational; the loop continues unchanged.
	EventCacheInvalidated EventType = "cache_invalidated"

	// EventFallbackSuppressed signals that a retryable error would
	// have triggered a provider fallback, but the loop refused the
	// switch. The cause error propagates to the caller; the run
	// fails. Purely informational. Two refusal reasons emit it:
	//
	//  1. Pin-after-success: the run was already past its first
	//     successful turn AND the operator opted into "pin provider
	//     after first successful turn" (`LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS=1`).
	//     Cross-provider mid-conversation fallback exposes a growing
	//     surface of provider-specific transcript translation bugs
	//     (Anthropic cache_control, DeepSeek reasoning_content, gemini
	//     thoughtSignature, tool_call shape differences); pinning after
	//     first success closes that class of bug in exchange for dropping
	//     resilience to mid-conversation provider issues (same-provider
	//     rate-limit retry in internal/providers/ratelimit/ still covers
	//     transient errors within one provider).
	//  2. Vision mismatch (RFC AT §4.4): the run carries an image content
	//     block but the re-resolved fallback target does not support
	//     vision (e.g. DeepSeek's text endpoint). Switching would let the
	//     image part reach a provider that 400s on it ("unknown variant
	//     'image_url'"), which RFC AT §4.4 says must not happen — so the
	//     swap is refused before the call.
	//
	// The Text field carries a human-readable summary naming the failed
	// and (for reason 2) the refused target provider/model.
	EventFallbackSuppressed EventType = "fallback_suppressed"

	// EventReasoningInvalidated signals that a v0.8.x runtime
	// fallback switched to a provider that must not receive
	// reasoning_content produced by the prior provider. The loop
	// zeroed the Reasoning field on every assistant turn in the
	// conversation history before retrying on the new provider.
	//
	// Why: Message.Reasoning is a single string field with no
	// provenance (provider.go:130). The OpenAI driver's
	// flattenMessage unconditionally echoes it back as
	// reasoning_content on the wire. DeepSeek's API verifies that
	// any echoed reasoning_content matches what IT produced and
	// 400s otherwise ("reasoning_content in the thinking mode must
	// be passed back to the API"). Cross-provider echoes always
	// fail this check. The strip pass at fallback time prevents
	// the failure deterministically.
	//
	// Cost retros should treat this run's downstream iterations
	// as reasoning-cold on the new provider — any benefit of the
	// prior provider's chain-of-thought is discarded. Distinct
	// from EventCacheInvalidated: cache and reasoning are
	// orthogonal invalidation axes that may fire independently.
	//
	// The Text field carries a human-readable summary
	// ("cleared reasoning_content from N assistant turn(s) on
	// switch from <old> to <new>").
	EventReasoningInvalidated EventType = "reasoning_invalidated"

	// EventModelDowngraded signals that a cross-provider fallback landed on a
	// thinking-class model that cannot consume the fallback history, so the
	// loop swapped it for its non-thinking sibling on that leg (see
	// ThinkingDowngrader). DeepSeek's deepseek-reasoner / *-pro require
	// provider-produced reasoning_content echoed on every assistant turn and
	// 400 ("reasoning_content ... must be passed back") on a turn lacking it;
	// after a switch the history's assistant turns are all reasoning-less (a
	// foreign provider produced them, or the reasoning strip zeroed them), so
	// the thinking model is downgraded to a non-thinking model for this run's
	// remaining iterations. Purely informational; the Text field names the
	// old model, the new model, and the provider. Distinct from
	// EventReasoningInvalidated (which reports the strip itself) — both may
	// fire on the same switch.
	EventModelDowngraded EventType = "model_downgraded"

	// EventChannelPublish signals that the v0.8.4 Channel tool
	// successfully appended a message to a channel. Emitted from
	// inside the tool's Execute() via the ctx-attached event
	// emitter (see tools.WithEventEmitter). The Channel field
	// carries the structured payload — channel name, message id,
	// scope axis, byte size, optional payload preview (truncated
	// to 200 chars), and the trim count for overflow audits.
	//
	// All the same data exists in the surrounding tool_call /
	// tool_result envelope, but the typed event lets SSE consumers
	// build channel-activity dashboards by filtering on Type
	// without parsing the tool_result JSON.
	EventChannelPublish EventType = "channel_publish"

	// EventChannelDelivery fires once per message returned to a
	// subscriber. Emitted from inside the tool's Execute() via the
	// ctx-attached event emitter. Distinct from EventChannelPublish
	// because a single message can be delivered N times (across
	// replays via `from_cursor: cur_0`, or to multiple subscribers
	// on a broadcast-shape channel) — publish events count
	// production, delivery events count consumption.
	//
	// One event per message in the returned batch. For a long-poll
	// subscribe that returns 100 messages, expect 100
	// EventChannelDelivery events emitted in order.
	EventChannelDelivery EventType = "channel_delivery"

	// EventInterruptionPending signals that the v0.8.16 Interruption
	// tool just created a pending interrupt and the agent's loop is
	// about to block on a human (or other delivery surface) resolve.
	// Emitted from inside the tool's Execute() via the ctx-attached
	// event emitter (tools.EventEmitter). The Interruption field
	// carries the structured payload — interrupt_id, kind, question,
	// options, priority, expires_at — enough for the Web UI to render
	// without a separate fetch.
	//
	// Only `ask` ops emit this event; `notify` (fire-and-forget) and
	// `cancel` (terminating) don't block, so SSE consumers don't see
	// a corresponding "pending" event. There is intentionally NO
	// EventInterruptionResolved sibling event on the run's SSE stream
	// because the run is BLOCKED inside the Interruption tool when
	// the resolve arrives — there's no SSE writer pumping at that
	// moment. The external `_system/interrupts/resolved` channel
	// publishes the resolve notification for non-run consumers.
	EventInterruptionPending EventType = "interruption_pending"

	// EventHostWidened is the v0.8.17 audit event emitted by the loop
	// whenever a permitted Pre-hook's allow_hosts grant fires for a
	// specific tool call. Emitted ONCE per dispatched tool call that
	// the hook actually widened (not on every call — the common
	// "no widening" path is silent).
	//
	// The HostWidening field carries the structured payload: the
	// requesting tool_call_id + tool_name, the originating URL (so
	// operators can spot confused-deputy patterns where the model's
	// requested host equals the hook's grant), the granting
	// hook's owner + name, and the list of hosts added.
	//
	// The event is purely informational — it does NOT itself widen
	// anything; the widening already happened in the dispatcher and
	// the ctx-extras path. Persistence is via the standard
	// makeRecordingEmit path so the events table carries an audit
	// row for every grant.
	EventHostWidened EventType = "host_widened"

	// EventSteer is emitted by the loop when an operator-injected steering
	// message (internal/steer) is drained into the running conversation
	// mid-turn. The UserInput field carries the text + source. Named "steer"
	// (not "user_input") deliberately: "user_input" is an existing PERSISTED
	// transcript-event KIND ([]loop.PromptSegment); the SSE event is distinct
	// so the recording path doesn't conflate the two shapes (the runner
	// persists a user_input row separately for replay; this event stays
	// live-only).
	EventSteer EventType = "steer"

	// EventAwaitingInput is emitted by a persistent INTERACTIVE run when the
	// model ends its turn and the loop parks waiting for the operator's next
	// instruction (instead of terminating). The run resumes on the next
	// steering message or ends on Cancel. The AwaitingInput field carries the
	// turn the run parked at. The UI renders this as the terminal's idle
	// "waiting for input" state.
	EventAwaitingInput EventType = "awaiting_input"

	// EventAwaitingReview is emitted when a run armed for review finishes its
	// answer: instead of completing, it is held until an operator approves it
	// or rejects it (optionally with feedback it then revises from). Distinct
	// from awaiting_input: that run waits for more work, this one waits for a
	// verdict on work it considers done. Emitted again after a compaction while
	// held, so a live view that just rendered the compaction shows the run as
	// held again.
	EventAwaitingReview EventType = "awaiting_review"

	// EventSpawnChildStarted / EventSpawnChildResult are the RFC X Phase 3
	// "spawn ledger" — recorded on the PARENT run's transcript so a
	// snapshotted+restored fan-out parent (blocked in Agent.parallel_spawn)
	// can reconstruct the parallel_spawn tool_result it never got to write.
	// EventSpawnChildStarted is emitted as each child's run row is created
	// (carries the child's index + run_id); EventSpawnChildResult as each
	// child completes (carries the result). Both carry the parent's
	// tool_use_id so the resume reconcile matches them to the dangling
	// parallel_spawn tool_use. replayTranscript ignores both (default case),
	// so they never pollute the reconstructed conversation. Emitted ONLY when
	// LOOMCYCLE_RESUME_FANOUT is on.
	EventSpawnChildStarted EventType = "spawn_child_started"
	EventSpawnChildResult  EventType = "spawn_child_result"

	// EventPromptSnapshot records the prompt a run's FIRST model call received
	// (RFC DI): the system blocks and the run's input, as the loop assembled
	// them — after skills, memory injection, {{...}} expansion, metadata and any
	// loop-side additions. Emitted once per loop entry. Store-only like the
	// spawn ledger: persisted, never forwarded to live SSE / gRPC consumers.
	// replayTranscript ignores it (no case), so it never enters a conversation.
	EventPromptSnapshot EventType = "prompt_snapshot"

	// EventContextCompaction marks where an interactive run's conversation was
	// compacted: everything before it is replaced by a summary. The loop emits
	// it (persisted + forwarded) when it applies a steer.KindCompact control at
	// a park boundary; replayTranscript RESETS to the summary pair on replay, so
	// a crash-recovery/resume rebuild reconstructs the compacted form, not the
	// full history. The full transcript is retained (non-destructive audit).
	EventContextCompaction EventType = "context_compaction"

	// EventContextRecap marks where a run's fed history was distilled by L1
	// reasoning-recap (RFC CR `context.mode: recap`): the span before the kept
	// tail is replaced by a running progress recap, and the recent tool_use/
	// tool_result pairs are kept verbatim. Sibling of EventContextCompaction — the
	// loop emits it (persisted + forwarded) when it recaps at an iteration
	// boundary; replayTranscript RESETS to the recap form on replay so a resume
	// rebuild reconstructs the identical fed history. The full transcript is
	// retained (non-destructive audit).
	EventContextRecap EventType = "context_recap"

	// EventContextState marks one step of an L2 structured-execution-state run
	// (RFC CR `context.mode: stateful`): the model emitted a patch, the runtime
	// merged it into the state object Σ, and (unless done) will execute the next
	// action. Sibling of EventContextRecap — it carries the post-merge Σ so the
	// transcript is a full audit of how the state evolved, and a client can render
	// the live state. The reasoning is forwarded once here and then discarded (not
	// fed to the next step).
	EventContextState EventType = "context_state"

	// EventLimit carries a per-scope token-budget crossing (RFC AW): a soft
	// crossing (warn, run continues) or a hard crossing (a mid-run notice; the
	// run still finishes, but the NEXT run for the scope is refused at
	// admission). The Limit field carries the structured payload.
	//
	// Unlike EventUsage/EventThinking, EventLimit is SERVER-generated (the
	// admission gate + recordCallUsage increment path), never emitted by a
	// provider driver — so the loop's per-iteration event switch needs NO case
	// for it. It is forwarded to consumers + persisted as a transcript row
	// through the server's makeRecordingEmit path (soft-at-admission + in-flight
	// crossing), so the /run terminal + chat render a budget banner.
	EventLimit EventType = "limit"

	// EventCapabilityInert reports a tool the agent HOLDS but cannot use,
	// because the capability gate that tool reads grants nothing.
	//
	// SERVER-generated, like EventLimit and EventOverride, and emitted ONCE at
	// run start rather than per call: the condition is a property of the
	// definition, not of any one invocation, and repeating it every time the
	// model reached for the tool would bury it.
	//
	// It exists because `tools` and the capability gates are two independent
	// grants and the second silently voids the first. Before this, an agent
	// granted AgentDef with no agent_def_scopes discovered the problem by being
	// refused mid-task, and the operator saw "the agent didn't do it" rather
	// than "the agent could not".
	//
	// Only genuinely INERT grants are reported. memory_scopes, history_scope,
	// sql_scopes and evaluation_scopes now resolve to what the caller owns, so
	// an unset one is not inert and an event for it would be noise on every run.
	EventCapabilityInert EventType = "capability_inert"

	// EventHookDecision records what a tool-use hook did to a call: denied it,
	// rewrote its input or its output, added context, or failed. Emitted once
	// per decision, in chain order, beside the tool_call it concerns — before
	// this existed a hook's deny or rewrite was invisible, and the transcript
	// kept the model's ORIGINAL input for a call that ran with another. A hook
	// that passed the call through emits nothing.
	EventHookDecision EventType = "hook_decision"

	// EventContextDistillDeclined reports that context distillation was TRIED
	// and did nothing — the threshold was crossed, the gate fired, and the
	// distiller returned without shrinking anything.
	//
	// It exists because a decline used to be indistinguishable from never
	// having been attempted. A live chat climbed to the top of its window with
	// zero recap markers and zero errors, and telling "the threshold was never
	// crossed" from "it was crossed and the summarizer returned empty" required
	// reading the summarizer's source and counting event types in a raw
	// transcript. That is not a diagnosis an operator can make.
	//
	// A DISTINCT type rather than a flag on EventContextRecap: those markers are
	// consumed by replayTranscript to rebuild history, and a "did nothing"
	// variant would force every replay site to learn to skip some of them. This
	// type is inert on replay — the switch ignores it — which is the property
	// that makes it safe to add.
	//
	// Emitted at most once per (mode, reason) per run: a reason that is a
	// property of the CONFIGURATION would otherwise repeat every iteration and
	// bury itself, while a state-dependent one legitimately recurs.
	EventContextDistillDeclined EventType = "context_distill_declined"

	// EventContextExhausted reports that the footprint is at or above the
	// threshold where distillation was supposed to reclaim the window, and
	// nothing did.
	//
	// It is DISTINCT from a decline, and the distinction is the point. A
	// decline says "this path did nothing and here is why" — routine, and
	// sometimes correct. Exhaustion says "the window is not being reclaimed and
	// the run is heading for the provider's limit", which is a different
	// message to a different reader.
	//
	// The honest guarantee this event exists to keep: the window is either
	// reclaimed, or the run says clearly that it cannot be. It can always be
	// made impossible — keep_last_n can pin an entire conversation — so the
	// runtime cannot promise to reclaim. It can promise never to fail silently.
	//
	// LOOP-generated, like the declines, and carries every
	// tier's verdict so a reader can see what was tried rather than only that
	// it failed.
	EventContextExhausted EventType = "context_exhausted"

	// EventOverride records that a RUN's own configuration changed while it was
	// running — an operator retuned a parked chat (RFC DC P3/D8).
	//
	// It exists because an override is run STATE with a lifetime, not a request
	// parameter: it can change on turn 12, and a model that swaps mid-
	// conversation with no trace makes the transcript a misleading record of
	// what produced what. A reader seeing the answers get better after turn 12
	// should be able to see why.
	//
	// SERVER-generated, like EventLimit and for the same reason — the loop's
	// per-iteration switch needs no case for it. Carried to consumers and
	// persisted as a transcript row through makeRecordingEmit.
	//
	// It reports a CHANGE, not a setting. A run that starts with an override
	// emits nothing: there is nothing to explain until something moves.
	EventOverride EventType = "override"

	// EventTurnCancelled is emitted by the loop when an operator cancels the
	// CURRENT TURN of an interactive run (RFC BH) — the in-flight generation +
	// the tool calls it started are stopped, but the run is NOT terminated: it
	// parks at awaiting_input (an EventAwaitingInput follows) with its session +
	// transcript intact, and the operator's next message continues it. Distinct
	// from the run-level "cancelled" stop_reason (whole-run cancel, terminal) so a
	// UI can render "turn stopped, input re-enabled" vs "run ended". The
	// TurnCancelled field carries the optional operator reason + the turn index.
	// Server-persisted + forwarded via makeRecordingEmit, and auto-replayed on
	// re-attach (runEventToFrame's default round-trips it).
	EventTurnCancelled EventType = "turn_cancelled"
)

// Event is one streamed datum from a provider call (or, after the loop layer
// has wrapped it, from the loop itself).
type Event struct {
	Type    EventType `json:"type"`
	Text    string    `json:"text,omitempty"`
	ToolUse *ToolUse  `json:"tool_use,omitempty"`
	Usage   *Usage    `json:"usage,omitempty"`
	Error   string    `json:"error,omitempty"`
	// IsError flags a tool_result whose execution failed. Surviving the
	// persist+replay round-trip matters because a continuation that lost
	// the flag would re-feed the model a successful-looking result.
	IsError bool `json:"is_error,omitempty"`
	// Retry carries the retry telemetry on EventRetry. Nil otherwise.
	Retry *RetryInfo `json:"retry,omitempty"`

	// ErrorInfo classifies a TERMINAL run failure on EventError: what kind it
	// is, whether resending can succeed, and any backoff. Nil elsewhere, and
	// nil for a failure the runtime cannot categorise — there is deliberately
	// no fallback bucket, because a category that carries no decision is worse
	// than an absent field.
	//
	// It exists because an SSE consumer's only signal today is the event TYPE
	// plus an English string: once the stream is open the HTTP status is
	// already 200, so "retry in five seconds" and "your budget is gone" arrive
	// looking identical. The admission path never had this problem — it
	// answers before the stream opens and has always emitted a typed code and
	// Retry-After.
	ErrorInfo *errkind.Info `json:"error_info,omitempty"`

	// Fallback carries the structured payload on EventProviderFallback
	// (the v0.8.2 runtime provider switch). Nil on all other event
	// types. Adapters log/render the switch + the failing error class
	// so cost retros can attribute downstream tokens to the new
	// provider.
	Fallback *FallbackInfo `json:"fallback,omitempty"`

	// Channel carries the structured payload on EventChannelPublish
	// and EventChannelDelivery (the v0.8.4 typed audit events from
	// the Channel tool). Nil on all other event types. Same
	// payload shape for both event types so SSE consumers building
	// channel-activity dashboards can filter on Type and key the
	// row by (channel, message_id) without two parsers.
	Channel *ChannelEventInfo `json:"channel,omitempty"`

	// Interruption carries the structured payload on
	// EventInterruptionPending (v0.8.16). Nil on all other event
	// types. Renders directly into the Web UI's modal/sidebar
	// without a follow-up fetch.
	Interruption *InterruptionEventInfo `json:"interruption,omitempty"`

	// HostWidening carries the structured payload on EventHostWidened
	// (v0.8.17). Nil on all other event types. Operators audit
	// confused-deputy patterns by comparing HostWidening.URL's host
	// to the granted HostsAdded — if they're always identical, the
	// hook is probably echoing model input without independent
	// validation.
	HostWidening *HostWideningEventInfo `json:"host_widening,omitempty"`

	// UserInput carries the structured payload on EventSteer (an
	// operator-injected steering message drained mid-turn). Nil otherwise.
	UserInput *UserInputEventInfo `json:"user_input,omitempty"`

	// AwaitingInput carries the structured payload on EventAwaitingInput (a
	// persistent interactive run parked at end_turn). Nil otherwise.
	AwaitingInput *AwaitingInputEventInfo `json:"awaiting_input,omitempty"`

	// AwaitingReview carries the structured payload on EventAwaitingReview.
	// Nil otherwise.
	AwaitingReview *AwaitingReviewEventInfo `json:"awaiting_review,omitempty"`

	// TurnCancelled carries the structured payload on EventTurnCancelled (an
	// operator turn-cancel, RFC BH). Nil on all other event types.
	TurnCancelled *TurnCancelledEventInfo `json:"turn_cancelled,omitempty"`

	// SpawnChild carries the structured payload on EventSpawnChildStarted /
	// EventSpawnChildResult (RFC X Phase 3 spawn ledger). Nil otherwise.
	SpawnChild *SpawnChildEventInfo `json:"spawn_child,omitempty"`

	// PromptSnapshot carries the structured payload on EventPromptSnapshot.
	// Nil otherwise.
	PromptSnapshot *PromptSnapshotInfo `json:"prompt_snapshot,omitempty"`

	// ContextCompaction carries the structured payload on EventContextCompaction
	// (the conversation summary that replaces prior history). Nil otherwise.
	ContextCompaction *ContextCompactionEventInfo `json:"context_compaction,omitempty"`

	// ContextRecap carries the structured payload on EventContextRecap (the
	// running reasoning-recap that replaces prior history in L1 recap mode, RFC
	// CR). Nil otherwise.
	ContextRecap *ContextRecapEventInfo `json:"context_recap,omitempty"`

	// ContextState carries the structured payload on EventContextState (the
	// post-merge state Σ + the applied patch, RFC CR L2). Nil otherwise.
	ContextState *ContextStateEventInfo `json:"context_state,omitempty"`

	// Limit carries the structured payload on EventLimit (a per-scope
	// token-budget crossing, RFC AW). Nil on all other event types.
	Limit *LimitInfo `json:"limit,omitempty"`

	// CapabilityInert carries the structured payload on EventCapabilityInert.
	// Nil on all other event types.
	CapabilityInert *CapabilityInertInfo `json:"capability_inert,omitempty"`

	// HookDecision carries the structured payload on EventHookDecision. Nil
	// otherwise.
	HookDecision *HookDecisionInfo `json:"hook_decision,omitempty"`

	// ContextDistill carries the structured payload on
	// EventContextDistillDeclined. Nil on all other event types.
	ContextDistill *ContextDistillDeclinedInfo `json:"context_distill,omitempty"`

	// ContextExhausted carries the structured payload on
	// EventContextExhausted. Nil on all other event types.
	ContextExhausted *ContextExhaustedInfo `json:"context_exhausted,omitempty"`

	// Override carries the structured payload on EventOverride (a run's
	// configuration changed mid-run, RFC DC). Nil on all other event types.
	Override *OverrideInfo `json:"override,omitempty"`

	// StopReason is set on the final assistant Event of a provider call:
	// "end_turn" | "tool_use" | "max_tokens" | "stop_sequence".
	StopReason string `json:"stop_reason,omitempty"`

	// Reasoning carries the assistant turn's accumulated reasoning
	// trace (DeepSeek V4 Pro / deepseek-reasoner). Set on EventDone
	// when the response stream included `reasoning_content` deltas.
	// The loop reads this and stamps it onto the assistant Message
	// it appends to the conversation history so the next iteration
	// echoes it back to the API per DeepSeek's contract. Empty for
	// non-thinking models.
	Reasoning string `json:"reasoning,omitempty"`

	// ReasoningSignature is Anthropic's extended-thinking block signature
	// (signature_delta), set on EventDone alongside Reasoning. The loop stamps
	// it onto the assistant Message so the Anthropic driver can replay the
	// thinking block with its seal on the next (tool-use continuation) request.
	// Empty for non-Anthropic / non-thinking turns.
	ReasoningSignature string `json:"reasoning_signature,omitempty"`
}

// RetryInfo accompanies an EventRetry. Each field is set every time.
type RetryInfo struct {
	Provider string `json:"provider"`
	Attempt  int    `json:"attempt"`
	WaitMs   int64  `json:"wait_ms"`
	// Reason is one of the RetryReason* constants below. It's a
	// stable wire string — adapters string-match against it.
	Reason string `json:"reason"`
}

// RetryReason* are the values of RetryInfo.Reason. They're declared on
// the providers package (not on ratelimit) because they're part of the
// wire contract — adapters and SSE consumers depend on these strings.
// Do not change without bumping a major version.
const (
	RetryReasonHeader   = "retry-after header"
	RetryReasonSchedule = "exponential backoff"
)

// FallbackInfo accompanies an EventProviderFallback. Carries the
// structured switch context for log + UI rendering. Wire-stable; the
// field names are part of the v0.8.2+ contract.
type FallbackInfo struct {
	// FailedProvider + FailedModel — the pair the loop just stopped
	// using. The loop marks (FailedProvider, FailedModel) stalled in
	// the resolver matrix before re-resolving, so subsequent agent
	// runs in this loomcycle process skip them until the next
	// availability probe clears the stall.
	FailedProvider string `json:"failed_provider"`
	FailedModel    string `json:"failed_model"`
	// NewProvider + NewModel — the next-in-queue the resolver picked.
	// Empty + the loop emits EventError next when the resolver could
	// not find any non-stalled candidate (the user_tier's candidate
	// list was exhausted).
	NewProvider string `json:"new_provider,omitempty"`
	NewModel    string `json:"new_model,omitempty"`
	// Attempt is the cumulative fallback counter — 1 for the first
	// switch after the original provider failed, 2 for the second,
	// etc. Capped by the user_tier's MaxFallbackAttempts.
	Attempt int `json:"attempt"`
	// UserTier is the operator-declared tier name that authorised
	// this fallback ("default" / "free" / "low" / "medium" / "high").
	// Free tiers never produce this event — their FallbackOnError
	// is false and the loop propagates the original error instead.
	UserTier string `json:"user_tier"`
	// Reason is the error-class label that triggered the switch
	// ("retryable" most commonly; "deadline_exceeded" never — that
	// shape is non-retryable). Stable wire string.
	Reason string `json:"reason"`
	// CauseError is the original error message (truncated to ~200
	// chars to avoid 9 KB HTML bodies flooding the SSE wire). Useful
	// for operator diagnostics — they see "anthropic 429: rate
	// limit exceeded" alongside the structural switch info.
	CauseError string `json:"cause_error,omitempty"`
}

// ChannelEventInfo accompanies EventChannelPublish and
// EventChannelDelivery. Same payload for both event types so SSE
// consumers can build channel-activity dashboards by filtering on
// Type and keying by (Channel, MessageID).
//
// Wire-stable; field names part of the v0.8.4+ contract.
type ChannelEventInfo struct {
	// Channel is the operator-declared channel name.
	Channel string `json:"channel"`
	// MessageID is the per-message identifier (ULID-shaped string,
	// "msg_<16-hex unixNanos><8-hex rand>"). Sortable by publish
	// time; agents must not parse it.
	MessageID string `json:"message_id"`
	// Scope mirrors the operator-yaml `scope` for this channel —
	// "agent" / "user" / "global" — so dashboards can group by
	// isolation axis without an extra lookup.
	Scope string `json:"scope"`
	// ScopeID is the resolved scope_id at emit time (agent name
	// for scope=agent, user_id for scope=user, empty string for
	// scope=global). Lets the audit trail show "who actually
	// pub/sub'd this" without re-resolving from the run identity.
	ScopeID string `json:"scope_id,omitempty"`
	// PayloadBytes is the byte length of the JSON payload as
	// stored. Useful for size dashboards without echoing the
	// payload itself.
	PayloadBytes int `json:"payload_bytes"`
	// PayloadPreview is the first 200 characters of the JSON
	// payload, included on every event for operator visibility.
	// Larger payloads are truncated at 200 chars + "…"; agents and
	// adapters that need the full payload read it from the
	// tool_result envelope (which carries the untruncated JSON).
	// Empty when the payload size is zero.
	PayloadPreview string `json:"payload_preview,omitempty"`
	// DroppedOldest is the count of overflow-trimmed rows on a
	// publish (lossy-on-overflow per the v0.8.4 RFC). Always 0 on
	// EventChannelDelivery — delivery cannot trigger trim.
	DroppedOldest int `json:"dropped_oldest,omitempty"`
	// Cursor is the new committed cursor after a delivery
	// (auto-commit on subscribe = the message_id of the last in
	// the batch). Always set on EventChannelDelivery to the
	// MessageID of THIS event (since delivery events fire per
	// message in order). Empty on EventChannelPublish.
	Cursor string `json:"cursor,omitempty"`
}

// InterruptionEventInfo is the structured payload on
// EventInterruptionPending (v0.8.16). Carries enough for the Web UI
// or external dashboard to render the question without a follow-up
// fetch of the interrupt row.
type InterruptionEventInfo struct {
	// InterruptID is the row's primary key. Same shape as the
	// minter: "intr_<16hex unixNanos><8hex rand>".
	InterruptID string `json:"interrupt_id"`
	// Kind is the discriminator. v0.8.16 emits only "question";
	// future "pause" / "wait_until" / "approval" land as additive
	// values.
	Kind string `json:"kind"`
	// Question is the prompt text for kind=question. Empty for
	// future non-question kinds.
	Question string `json:"question,omitempty"`
	// Options is the JSON-encoded array of option strings for
	// kind=question (NULL/empty = free-text answer). Verbatim from
	// the interrupt row; the Web UI parses it.
	Options json.RawMessage `json:"options,omitempty"`
	// Context is the optional hint string the agent provides ("47
	// records pending"). Empty when the agent didn't pass context.
	Context string `json:"context,omitempty"`
	// Priority is "low" / "normal" / "high" — informational, drives
	// UI badge styling.
	Priority string `json:"priority"`
	// ExpiresAt is the absolute UTC timestamp at which the
	// interruption will time out. RFC3339. Empty when no timeout
	// was set.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// UserInputEventInfo is the structured payload on EventSteer — an
// operator-injected steering message (internal/steer) drained into the
// running conversation mid-turn. The Web UI renders it as an operator-message
// row in the live terminal.
type UserInputEventInfo struct {
	// Text is the operator's instruction (delivered to the model as a
	// user-role turn).
	Text string `json:"text"`
	// Source is "api" | "webui" — resolved at the auth boundary.
	Source string `json:"source,omitempty"`
	// SeenAt is when the loop drained the message. RFC3339Nano.
	SeenAt string `json:"seen_at,omitempty"`
}

// AwaitingInputEventInfo is the structured payload on EventAwaitingInput — a
// persistent interactive run parked at end_turn, waiting for the operator's
// next steering message (or Cancel).
type AwaitingInputEventInfo struct {
	// SinceTurn is the iteration index the run parked at. Informational —
	// lets the UI show "idle after N turns".
	SinceTurn int `json:"since_turn"`
}

// AwaitingReviewEventInfo is the structured payload on EventAwaitingReview.
type AwaitingReviewEventInfo struct {
	// SinceTurn is the iteration index the run was held at.
	SinceTurn int `json:"since_turn"`
	// Round counts the holds so far: 1 on the first answer, 2 after the first
	// rejection with feedback has been revised, and so on.
	Round int `json:"round"`
	// ExpiresAt (RFC 3339, UTC) is when the hold ends as rejected if nobody
	// rules on it. Empty when the run has no review deadline.
	ExpiresAt string `json:"expires_at,omitempty"`
	// HeldBy names the agent_stop hook ("<owner>/<name>") that held the answer.
	// Empty when review arming held it. Disarming review releases only the
	// latter.
	HeldBy string `json:"held_by,omitempty"`
}

// TurnCancelledEventInfo is the structured payload on EventTurnCancelled — the
// operator stopped the current turn of an interactive run (RFC BH). The run then
// parks at awaiting_input (an EventAwaitingInput follows).
type TurnCancelledEventInfo struct {
	// Reason is the operator's optional free-text reason (empty when none was
	// given). Non-secret; surfaced for the UI's "turn stopped" notice.
	Reason string `json:"reason,omitempty"`
	// SinceTurn is the iteration index the turn was cancelled at. Informational.
	SinceTurn int `json:"since_turn"`
}

// SpawnChildEventInfo is the structured payload on EventSpawnChildStarted /
// EventSpawnChildResult (RFC X Phase 3 spawn ledger), recorded on the PARENT
// run's transcript. ToolUseID + Index identify the child within the parent's
// parallel_spawn call; RunID is the child's run row (the started event's
// reason for being — so the resume reconcile can await + re-collect a child
// still pending at snapshot time). On EventSpawnChildResult, Ok/Output/Error
// carry the finished child's result (so a child that completed BEFORE the
// snapshot — whose run row isn't captured — still has its result in the
// parent's captured transcript).
// PromptSnapshotInfo is what a run's first model call was sent: the system
// blocks and the run's own input (the request's last user turn — for a
// continuation, the new message rather than the whole history). Image bytes
// are dropped (MediaType is kept, Data emptied): a snapshot is for reading what
// the model was asked, and a base64 image would make it as large as the image.
type PromptSnapshotInfo struct {
	System []ContentBlock `json:"system"`
	Input  []ContentBlock `json:"input"`
}

// NewPromptSnapshot builds the snapshot of a request.
func NewPromptSnapshot(system []ContentBlock, messages []Message) *PromptSnapshotInfo {
	snap := &PromptSnapshotInfo{System: withoutImageBytes(system), Input: []ContentBlock{}}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			snap.Input = withoutImageBytes(messages[i].Content)
			break
		}
	}
	return snap
}

func withoutImageBytes(blocks []ContentBlock) []ContentBlock {
	out := make([]ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		if out[i].Type == "image" {
			out[i].Data = ""
		}
	}
	return out
}

type SpawnChildEventInfo struct {
	ToolUseID string `json:"tool_use_id"`
	Index     int    `json:"index"`
	RunID     string `json:"run_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	// Result fields — set on EventSpawnChildResult only.
	Ok     bool   `json:"ok,omitempty"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	// State is a stateful child's final Σ (RFC CR D5), captured in the ledger so a
	// fan-out parent restored from a snapshot keeps each child's structured result,
	// not just its prose. Set for a child that completed before the snapshot; a
	// child re-dispatched across the snapshot returns its text only (its Σ is not
	// re-scanned from the transcript — a residual gap in the experimental
	// resume-fanout path).
	State map[string]any `json:"state,omitempty"`
}

// ContextCompactionEventInfo is the structured payload on EventContextCompaction
// (interactive context compaction). Summary is the model-generated recap that
// replaces the prior conversation; Before/AfterTokens are the rough token
// footprint before vs after, for the operator-facing "context compacted (N→M)"
// line. replayTranscript reads Summary to seed the compacted message pair.
type ContextCompactionEventInfo struct {
	Summary      string `json:"summary"`
	BeforeTokens int    `json:"before_tokens,omitempty"`
	AfterTokens  int    `json:"after_tokens,omitempty"`
	// KeepN / KeepFirst record how much recent history was kept verbatim and
	// whether the first user turn (the task) was pinned — replayTranscript reads
	// these to reconstruct the identical compacted form (keep last KeepN of the
	// accumulated messages + pin the first when KeepFirst).
	KeepN     int  `json:"keep_n,omitempty"`
	KeepFirst bool `json:"keep_first,omitempty"`
	// Trigger is "manual" | "auto" | "self" — which path fired the compaction.
	// Surfaced for metrics + the UI; not used by replay.
	Trigger string `json:"trigger,omitempty"`
	// MemoryBanked records that the discarded span was queued for consolidation
	// (RFC BL P3 `compaction.memory_flush`). Absent when the agent has not opted
	// in, which is the default. Additive and ignored by replay — banking already
	// happened when the compaction first ran, so a rebuild must not repeat it.
	MemoryBanked *MemoryBankedInfo `json:"memory_banked,omitempty"`
}

// ContextRecapEventInfo is the structured payload on EventContextRecap (RFC CR
// L1 reasoning-recap). Recap is the running progress note that replaces the span
// before the kept tail; Before/AfterTokens are the rough footprint before vs
// after, for the operator-facing "context recapped (N→M)" line. replayTranscript
// reads Recap + KeepN/KeepFirst to reconstruct the identical fed form.
type ContextRecapEventInfo struct {
	// Recap is the running progress recap that seeds the rebuilt history. On
	// replay it is restored verbatim AND becomes the loop's running-recap state,
	// so the NEXT recap folds into it (the cumulative O(1)-per-step property).
	Recap        string `json:"recap"`
	BeforeTokens int    `json:"before_tokens,omitempty"`
	AfterTokens  int    `json:"after_tokens,omitempty"`
	// KeepN / KeepFirst record how much recent history was kept verbatim and
	// whether the first user turn (the task) was pinned — replayTranscript reads
	// these to reconstruct the identical recapped form.
	KeepN     int  `json:"keep_n,omitempty"`
	KeepFirst bool `json:"keep_first,omitempty"`
	// Trigger is "auto" | "self" — which path fired the recap. Metrics/UI only.
	Trigger string `json:"trigger,omitempty"`
	// Reasoning echoes the R-layer policy that produced this ("recap" | "drop").
	// Informational; replay uses Recap directly.
	Reasoning string `json:"reasoning,omitempty"`
}

// ContextStateEventInfo is the structured payload on EventContextState (RFC CR L2
// structured execution state). State is the post-merge Σ; Patch is the merge-patch
// the model emitted this step (a null value deleted a key). Iter is the 0-based
// step index. Action names the tool the step will run next (empty when the run is
// finishing). Reasoning is the model's discarded step reasoning (carried once for
// audit, never fed forward).
type ContextStateEventInfo struct {
	State     map[string]any `json:"state"`
	Patch     map[string]any `json:"patch,omitempty"`
	Iter      int            `json:"iter"`
	Action    string         `json:"action,omitempty"`
	Reasoning string         `json:"reasoning,omitempty"`
	// ProposedSchema is a state_schema the model proposed this step (RFC CR L2
	// model-proposed→operator-adopted flow). Recorded on the transcript for the
	// operator to review; it is INERT — it does not change validation for this or
	// any run until an operator adopts it (by forking the agent def's
	// context.state_schema). Absent unless the model proposed one that differs
	// from the run's active schema.
	ProposedSchema map[string]any `json:"proposed_schema,omitempty"`
	// Evicted names the Σ keys structural compaction dropped this step, if any.
	//
	// On the per-step event rather than its own type because this is where an
	// operator already reads Σ — a state that shrank with no explanation beside
	// it is the silence this line exists to remove, and a separate event would
	// have to be correlated back to the step that caused it.
	Evicted []string `json:"evicted,omitempty"`
}

// MemoryBankedInfo is the outcome of a compaction's memory flush. It is on the
// transcript rather than only in a log because five consolidator runs proved a
// prose report is not evidence — an operator has to be able to read what actually
// happened out of the run.
type MemoryBankedInfo struct {
	// PendingID is the queue row, empty when nothing was banked.
	PendingID string `json:"pending_id,omitempty"`
	// Messages counts the conversational turns queued — tool traffic is excluded
	// before banking, so this is smaller than the span that was dropped.
	Messages int `json:"messages,omitempty"`
	// Error names why banking did not happen. Its presence is NOT a failed
	// compaction: the compaction completed either way, which is the invariant.
	Error string `json:"error,omitempty"`
}

// OverrideInfo is the structured payload on EventOverride (RFC DC — a run's own
// configuration changed while it was running).
//
// It names WHAT MOVED and what it moved to, because "the configuration changed"
// answers nothing for the reader who is trying to explain a change in
// behaviour. From/To on the routing pair specifically, since that is the change
// most likely to show up as different output.
//
// DISCLOSURE: every field here is operator-chosen configuration that the model
// already sees the effects of — a model name, a token budget. It carries no
// credential, no host from operator config, and no token suffix. A future field
// on this payload is a disclosure decision, not a formatting one: the event
// goes to the transcript, to SSE, to gRPC, and into snapshots.
type OverrideInfo struct {
	// Source says who changed it. "operator" is the only value today (the
	// steer endpoint); it is here so a later automatic retune is
	// distinguishable from a human one rather than indistinguishable.
	Source string `json:"source"`

	// FromModel / ToModel are the routing pair, formatted "provider/model".
	//
	// THEY ARE ALSO HOW THE TWO OVERRIDE EVENTS ARE TOLD APART, and a consumer
	// needs to: a single retune can produce both. The server emits one when the
	// operator acts, listing the keys the request set and carrying NO pair —
	// nothing has been re-resolved yet, so there is no honest "to" to report.
	// The loop emits one when the run adopts a routing change, and that one
	// always carries both halves.
	//
	// So: a pair present means "the run is now using this"; a pair absent means
	// "an operator asked for these fields". A reader that wants only the second
	// kind filters on FromModel == "".
	FromModel string `json:"from_model,omitempty"`
	ToModel   string `json:"to_model,omitempty"`

	// Fields lists the override keys this event is reporting.
	//
	// TWO SITES FILL IT IN, AND THEY KNOW DIFFERENT THINGS. The server emits one
	// event when the operator retunes, listing the keys the REQUEST actually set
	// — that is the event that shows a budget or tuning change which moved no
	// model. The loop emits one when a parked run wakes and its routing has
	// MOVED, and there the only key that moved is the routing itself, so it says
	// so and carries the FromModel/ToModel pair the server could not yet know.
	//
	// This comment used to promise the request's keys unconditionally, while the
	// only site filling it in was the loop's — which cannot see a request. A
	// consumer wrote a branch for "max_tokens changed" that could never run.
	Fields []string `json:"fields,omitempty"`
}

// LimitInfo is the structured payload on EventLimit (RFC AW — per-scope token
// budgets). It names which scope tripped, how hard, and where the scope stands
// against its ceiling, so a UI can render "tenant acme at 1.2M / 1M tokens this
// month" without a follow-up fetch. No secrets: Scope/ScopeID are a
// tenant/subject id (already non-secret, like user_id) and the counts are
// integers. Wire-stable; field names are part of the RFC AW contract.

// HookDecisionInfo is the payload on EventHookDecision.
type HookDecisionInfo struct {
	// Hook names it as "<owner>/<name>". A tenant sees the name of an operator
	// hook that acted on its call, never its callback.
	Hook  string `json:"hook"`
	Phase string `json:"phase"` // pre | post | post_failure | agent_start | agent_stop
	// ToolUseID and ToolName name the call a tool hook decided on; empty for
	// agent_start / agent_stop.
	ToolUseID string `json:"tool_use_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	// Decision: deny | rewrite_input | rewrite_output | context | block | hold
	// | unavailable.
	Decision string `json:"decision"`
	// FailMode is set for "unavailable": open (the call went ahead) or closed
	// (it was refused).
	FailMode string `json:"fail_mode,omitempty"`
	// Reason is the deny's or block's text (a block's is the user turn the
	// model was sent back with), a hold's reason, or why the hook was
	// unavailable. For a webhook that is a short category ("the hook returned
	// status 500", "the hook timed out") that never carries the callback URL or
	// its response body — those stay in the server log; for a code hook it is
	// the runner's message.
	Reason string `json:"reason,omitempty"`
	// UpdatedInput is the input the tool actually ran with, for rewrite_input.
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	// AdditionalContext is what a "context" decision appended to the result.
	AdditionalContext string `json:"additional_context,omitempty"`
}

// CapabilityInertInfo is the structured payload on EventCapabilityInert: one
// tool the agent holds and cannot use.
//
// It carries the FIX as well as the fact. A reader told only "AgentDef is
// inert" still has to work out which yaml key governs it, and the answer is not
// guessable from the tool name — which is most of why this failure was hard to
// act on.
type CapabilityInertInfo struct {
	// Tool is the granted tool, as named in the agent's `tools` list.
	Tool string `json:"tool"`
	// Gate is the yaml key that governs it, e.g. "agent_def_scopes".
	Gate string `json:"gate"`
	// Message is a human-readable line naming the tool, the gate and what to
	// set. Optional, but always populated by the runtime.
	Message string `json:"message,omitempty"`
}

// Distillation-decline reasons. Each names a DIFFERENT operator action, which
// is why this is an enumeration and not one "declined" flag — a reader told
// only that distillation declined has learned nothing they can act on.
const (
	// DistillDeclineSplitDeclined — CompactionSplit found nothing to summarize:
	// keep_last_n spans the whole conversation. Carries Messages and KeepLastN,
	// because those two numbers ARE the diagnosis. Action: lower keep_last_n.
	DistillDeclineSplitDeclined = "split_declined"
	// DistillDeclineEmptySummary — the summarizer returned no text and no
	// error. A thinking model on a small budget spends it reasoning and emits
	// nothing the accumulator collects. Action: raise recap_max_chars, or pick
	// an effort that makes the driver stop the model thinking.
	DistillDeclineEmptySummary = "empty_summary"
	// DistillDeclineSummarizeFailed — the summarize call errored. The existing
	// EventError is STILL emitted alongside this; terminal-error consumers
	// depend on it, so this reason adds a structured twin rather than replacing
	// a signal something already watches.
	DistillDeclineSummarizeFailed = "summarize_failed"
	// DistillDeclineNotSmaller — the distillation ran and produced something no
	// smaller than what it replaced, so it was refused. Carries both token
	// counts: "14230 -> 14334" is the whole explanation.
	DistillDeclineNotSmaller = "not_smaller"
	// Distillation-decline severities. Most declines are routine; one is not.
	//
	// The distinction is whether the WINDOW CAN STILL BE RECLAIMED by this
	// path. reasoning_keep is the operator's own instruction and not_smaller is
	// a correct refusal — both leave the mechanism healthy. split_declined
	// means keep_last_n pins the whole conversation, so this path will decline
	// identically every time and the window will keep filling.
	DistillSeverityInfo    = "info"
	DistillSeverityWarning = "warning"

	// DistillDeclineReasoningKeep — reasoning: keep asks for no distillation.
	// Not a fault; reported so that "nothing happened" is never silent, because
	// an operator who did not realise keep disables this needs to see it once.
	DistillDeclineReasoningKeep = "reasoning_keep"

	// DistillDeclineDeniedByHook — a pre_compact hook refused the compaction.
	// Message carries the hook's reason. Action: that hook's owner decides.
	DistillDeclineDeniedByHook = "denied_by_hook"
)

// ContextDistillDeclinedInfo is the payload on EventContextDistillDeclined.
//
// The numeric fields are populated per reason rather than always: a reader
// should be able to act on the event without a second lookup, and which numbers
// are the evidence depends on why it declined.
type ContextDistillDeclinedInfo struct {
	// Mode is the distillation path that declined: "recap" | "compaction".
	// Which one matters because they read DIFFERENT config keys, and an
	// operator editing the wrong block is the failure one subsystem over.
	Mode string `json:"mode"`
	// Trigger is "auto" | "self" — the threshold fired, or the agent asked.
	Trigger string `json:"trigger,omitempty"`
	// Reason is one of the DistillDecline* constants.
	Reason string `json:"reason"`
	// UsedTokens / WindowTokens are the footprint that opened the gate. They
	// say how urgent the decline is: declining at 40% is housekeeping,
	// declining at 99% is the run about to fail.
	UsedTokens   int `json:"used_tokens,omitempty"`
	WindowTokens int `json:"window_tokens,omitempty"`
	// Messages / KeepLastN are the split_declined diagnosis: this many messages
	// in hand, this many pinned by policy, so nothing was left to summarize.
	Messages  int `json:"messages,omitempty"`
	KeepLastN int `json:"keep_last_n,omitempty"`
	// BeforeTokens / AfterTokens are the not_smaller evidence — what the
	// distillation would have replaced, and what it produced.
	BeforeTokens int `json:"before_tokens,omitempty"`
	AfterTokens  int `json:"after_tokens,omitempty"`
	// Severity is DistillSeverityInfo or DistillSeverityWarning — whether the
	// window can still be reclaimed by this path.
	//
	// It also joins the once-per-run dedup key, so a condition that ESCALATES
	// is reported again rather than suppressed by its own earlier, quieter
	// self. A decline that was informational at 60% and is a warning at 90% is
	// two different messages to an operator.
	Severity string `json:"severity,omitempty"`
	// Message is a human-readable line naming the condition and the fix.
	// Optional, but always populated by the runtime.
	//
	// ⚠️ When it names keep_last_n it must QUALIFY which one — recap reads
	// context.keep_last_n, compaction reads compaction.keep_last_n, and an
	// operator sent to the wrong block edits a setting that was not the
	// problem. See declineSplitMessage.
	Message string `json:"message,omitempty"`
}

// ContextExhaustedInfo is the payload on EventContextExhausted: the footprint
// that was not reclaimed, and what each tier said when asked.
type ContextExhaustedInfo struct {
	// UsedTokens / WindowTokens are why this is urgent rather than tidy.
	UsedTokens   int `json:"used_tokens"`
	WindowTokens int `json:"window_tokens"`
	// UsedPct is precomputed because every consumer wants it and a window of 0
	// (an unknown ceiling) makes the division a trap.
	UsedPct int `json:"used_pct,omitempty"`
	// Verdicts is what each distillation tier answered, in the order tried.
	// A reader needs to know the mechanism RAN and refused, not merely that the
	// window is full — those call for opposite next moves.
	Verdicts []ContextTierVerdict `json:"verdicts,omitempty"`
	// Message is a human-readable line naming the condition and what would
	// change it.
	Message string `json:"message,omitempty"`
}

// ContextTierVerdict is one tier's answer inside ContextExhaustedInfo.
type ContextTierVerdict struct {
	// Mode is the tier that was asked: "recap" | "compaction" | "stateful".
	Mode string `json:"mode"`
	// Reason is a DistillDecline* constant, or "" when the tier was not
	// reachable at all for this run.
	Reason string `json:"reason,omitempty"`
	// Message is that tier's own explanation, carried verbatim so the
	// exhaustion report does not paraphrase away the fix it named.
	Message string `json:"message,omitempty"`
}

type LimitInfo struct {
	// Scope is which axis tripped: "operator" | "tenant" | "user".
	Scope string `json:"scope"`
	// ScopeID is the tripped scope's id — the tenant id for scope=tenant, the
	// user subject for scope=user, "" for the operator-global scope.
	ScopeID string `json:"scope_id,omitempty"`
	// Severity is "soft" (warn, run continues) or "hard" (this run finishes but
	// the next is refused at admission).
	Severity string `json:"severity"`
	// Window is the budget window; "month" (calendar month, UTC) in Phase 1.
	Window string `json:"window"`
	// Used is the scope's month-to-date token total at the crossing.
	Used int64 `json:"used"`
	// Limit is the tier that was crossed (the soft or hard ceiling).
	Limit int64 `json:"limit"`
	// Message is a human-readable banner string. Optional.
	Message string `json:"message,omitempty"`
}

// HostWideningEventInfo is the structured payload on EventHostWidened
// (v0.8.17). Emitted once per dispatched tool call that a permitted
// Pre-hook widened. Operators correlate (ToolCallID, URL, HostsAdded)
// to detect confused-deputy patterns where the hook is just echoing
// the model's requested host without independent validation — those
// cases show URL.Host == HostsAdded[0] for every event from one owner.
type HostWideningEventInfo struct {
	// ToolCallID is the loop-issued or provider-issued tool_use id
	// (the same id that appears on the surrounding EventToolCall /
	// EventToolResult). Lets a UI thread the audit row to its tool
	// call.
	ToolCallID string `json:"tool_call_id"`
	// ToolName is "HTTP" / "WebFetch" / etc — whichever tool this
	// widening applies to. Carried explicitly so an operator can
	// filter audit logs by tool without joining to events of other
	// types.
	ToolName string `json:"tool_name"`
	// URL is the originating URL the model asked the tool to fetch.
	// Recorded verbatim (not normalised) so operators can spot
	// patterns where a hook echoes a model-supplied URL's host.
	// CAREFUL: the URL itself may carry tokens or sensitive query
	// params; operators should redact in downstream log forwarders.
	URL string `json:"url"`
	// HookOwner is the registered hook's Owner UID — the app that
	// the operator yaml opted in to via hooks.permit_host_widen.owners.
	HookOwner string `json:"hook_owner"`
	// HookName is the hook's Name (the Owner+Name identity). Same
	// hook can register multiple Names; this discriminates the
	// specific one that contributed.
	HookName string `json:"hook_name"`
	// HostsAdded is the deduplicated list of hostnames the
	// dispatcher accumulated for THIS tool call. Matches the
	// PreOutcome.AllowHosts value. Leading-dot entries appear here
	// verbatim (they're semantic — suffix-match opt-in).
	HostsAdded []string `json:"hosts_added"`
}

// ToolUse is the model's request to invoke a tool.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Usage is one provider call's token accounting.
type Usage struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_input_tokens,omitempty"`
	Model               string `json:"model,omitempty"`

	// Provider is the provider ID that ACTUALLY served the call.
	// May differ from the agent's yaml-configured provider when the
	// v0.8.2 runtime-fallback path switched mid-run (e.g.,
	// anthropic-oauth-dev → ollama after a 429). Surfaced so post-
	// run analysis can quantify how often fallback routed runs to
	// the secondary provider.
	//
	// Populated by the loop at iteration success time from
	// opts.Provider.ID() — which tryProviderFallback mutates in
	// place when fallback engages, so this naturally captures the
	// post-fallback identity.
	Provider string `json:"provider,omitempty"`

	// MaxContextTokens is the serving model's context-window ceiling,
	// surfaced so a UI can render a "context used / max" gauge without
	// hard-coding a per-model table. 0 = unknown (e.g. Ollama, which
	// defers the window to the model). Set by the loop at emit time from
	// opts.Provider.Capabilities().MaxContextTokens — additive + optional,
	// so older consumers and the run-final totalUsage (which leaves it 0)
	// are unaffected.
	MaxContextTokens int `json:"max_context_tokens,omitempty"`

	// --- RFC AV: per-call usage attribution (all additive, omitempty). ---

	// CredentialSource names which key paid for this call: "operator" (the
	// host key), or "tenant" / "user" when an RFC AR override fired at that
	// scope. Empty ⇒ operator (the default for every call with no override).
	// The driver stamps it from the same resolve it uses to pick the key, so
	// there is no extra credential-store read. CredentialScopeID is the owning
	// subject/tenant id of an override ("" for operator / tenant scope). These
	// let the server attribute spend to the operator vs the tenant/user.
	CredentialSource  string `json:"credential_source,omitempty"`
	CredentialScopeID string `json:"credential_scope_id,omitempty"`

	// ProviderReportedCost is the provider's / gateway's OWN cost figure for
	// this call, when the response carries one (OpenRouter-style `usage.cost`);
	// 0/absent ⇒ the server prices the call from its pricing table. When set it
	// is authoritative (never re-priced). ProviderCostCurrency pairs with it.
	ProviderReportedCost float64 `json:"provider_reported_cost,omitempty"`
	ProviderCostCurrency string  `json:"provider_cost_currency,omitempty"`
}
