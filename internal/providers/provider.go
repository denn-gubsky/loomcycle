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

	// EventContextCompaction marks where an interactive run's conversation was
	// compacted: everything before it is replaced by a summary. The loop emits
	// it (persisted + forwarded) when it applies a steer.KindCompact control at
	// a park boundary; replayTranscript RESETS to the summary pair on replay, so
	// a crash-recovery/resume rebuild reconstructs the compacted form, not the
	// full history. The full transcript is retained (non-destructive audit).
	EventContextCompaction EventType = "context_compaction"

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

	// TurnCancelled carries the structured payload on EventTurnCancelled (an
	// operator turn-cancel, RFC BH). Nil on all other event types.
	TurnCancelled *TurnCancelledEventInfo `json:"turn_cancelled,omitempty"`

	// SpawnChild carries the structured payload on EventSpawnChildStarted /
	// EventSpawnChildResult (RFC X Phase 3 spawn ledger). Nil otherwise.
	SpawnChild *SpawnChildEventInfo `json:"spawn_child,omitempty"`

	// ContextCompaction carries the structured payload on EventContextCompaction
	// (the conversation summary that replaces prior history). Nil otherwise.
	ContextCompaction *ContextCompactionEventInfo `json:"context_compaction,omitempty"`

	// Limit carries the structured payload on EventLimit (a per-scope
	// token-budget crossing, RFC AW). Nil on all other event types.
	Limit *LimitInfo `json:"limit,omitempty"`

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
type SpawnChildEventInfo struct {
	ToolUseID string `json:"tool_use_id"`
	Index     int    `json:"index"`
	RunID     string `json:"run_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	// Result fields — set on EventSpawnChildResult only.
	Ok     bool   `json:"ok,omitempty"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
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
}

// LimitInfo is the structured payload on EventLimit (RFC AW — per-scope token
// budgets). It names which scope tripped, how hard, and where the scope stands
// against its ceiling, so a UI can render "tenant acme at 1.2M / 1M tokens this
// month" without a follow-up fetch. No secrets: Scope/ScopeID are a
// tenant/subject id (already non-secret, like user_id) and the counts are
// integers. Wire-stable; field names are part of the RFC AW contract.
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
