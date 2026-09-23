// Package loop runs the model→tool_use→tool_result→model cycle.
//
// One Run() call drives one agent run to completion. It calls the provider,
// streams events to the caller, dispatches tool_use to the dispatcher, sends
// tool_result back to the provider on the next iteration, and stops when the
// model signals end_turn (or hits MaxIterations).
package loop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/recall"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// PromptSegment mirrors the shape used by jobs-search-agent so the TS adapter
// is a 1:1 wrapper. A segment is a system or user message composed of typed
// content blocks (trusted-text or untrusted-block; both flatten to provider
// content blocks at request time).
type PromptSegment struct {
	Role    string               `json:"role"` // "system" | "user"
	Content []PromptContentBlock `json:"content"`
}

// PromptContentBlock is the typed content union the caller sends in.
//   - "trusted-text"     : text the loop trusts; goes through verbatim.
//   - "untrusted-block"  : text from an external source; wrapped in <untrusted>
//     tags before being sent to the model.
//   - "image"            : inline image input (RFC AT); valid only in a
//     user-role segment, capability-gated per provider/model.
type PromptContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Cacheable bool   `json:"cacheable,omitempty"`
	Kind      string `json:"kind,omitempty"` // for untrusted-block: e.g. "web_content", "uploaded_cv"

	// Image fields (Type == "image", RFC AT). MediaType is one of the
	// whitelisted image media types (image/png|jpeg|gif|webp); Data is the
	// base64 of the image bytes with NO "data:" prefix (the driver builds any
	// data-URI form internally). There is deliberately no URL form — accepting
	// a URL would make loomcycle fetch arbitrary hosts (SSRF); see RFC AT §6.
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

// codeJSProviderID is the synthetic replay provider's ID — must match
// internal/providers/codejs.providerID. The loop can't import codejs (cycle),
// so the constant is mirrored here; it gates the RFC-Z context-plugin exemption
// (code-js replays locally, so outbound redaction is both pointless and would
// trip replay divergence).
const codeJSProviderID = "code-js"

// RunOptions is one Run() invocation.
type RunOptions struct {
	Provider      providers.Provider
	Model         string
	Tools         []tools.Tool
	Dispatcher    *tools.Dispatcher
	Segments      []PromptSegment
	OnEvent       func(providers.Event) // streaming hook (called from loop goroutine)
	MaxIterations int                   // safety cap; default 16
	// UnboundedIterations lifts the MaxIterations soft-cap for THIS run even
	// on an LLM provider (the 1<<20 hard backstop still applies). Distinct
	// from Provider.Capabilities().UnboundedIterations (code-js only); set
	// from the agent's config.AgentDef.UnboundedIterations for interactive
	// terminal-driven runs.
	UnboundedIterations bool

	// SteerQueue, when non-nil, is the receive side of this run's operator
	// steering queue (internal/steer). At the TOP of each iteration the loop
	// drains it (non-blocking) and appends each Message as a user-role turn
	// before the next provider call. Nil disables steering.
	SteerQueue <-chan steer.Message
	// OnSteer fires once per drained steering message, AFTER it's appended to
	// the conversation, so the runner can persist a user_input transcript
	// event + emit the EventSteer SSE event. Same "cheap, loop-goroutine,
	// tolerate failures" contract as OnHeartbeat.
	OnSteer func(steer.Message)

	// BankCompactedSpan, when non-nil, banks the span a compaction is about to
	// DISCARD onto the consolidation queue, so the pass can extract durable facts
	// from it later (RFC BL P3). It receives the messages being dropped and
	// returns a correlation id for the transcript marker.
	//
	// A CALLBACK rather than a store handle because the loop is deliberately
	// store-free: resolving the agent's memory scope, its tenant, and whether
	// `compaction.memory_flush` is even set all live where the store already
	// does. Nil therefore covers every "do not bank" case at once — the flag is
	// off, the agent has no writable memory scope, or there is no store — and the
	// loop does not have to know which.
	//
	// Errors are for the marker only. Compaction exists to keep a run alive;
	// banking is strictly secondary and must never be able to fail it.
	BankCompactedSpan func(ctx context.Context, dropped []providers.Message) (string, error)

	// Interactive makes this a PERSISTENT run: instead of terminating when the
	// model ends its turn, the loop parks on SteerQueue waiting for the
	// operator's next instruction (resuming on it, ending only on Cancel).
	// Requires SteerQueue. An interactive run with no explicit MaxIterations is
	// automatically unbounded (Run lifts the 16-turn soft cap — see the
	// interactiveUnbounded note in Run), so an always-on terminal works without
	// also setting UnboundedIterations; the 1<<20 hard ceiling + Cancel still
	// bound it. Set an explicit MaxIterations to cap an interactive session.
	Interactive bool

	// InteractiveNow, when non-nil, is consulted at each PARK DECISION instead of
	// the static Interactive flag — so a run that started non-interactive can be
	// PROMOTED while it is already running, and park at its next end_turn rather
	// than finishing.
	//
	// WHY A CALLBACK AND NOT A MUTABLE FIELD. The decision has to be read at the
	// boundary, not captured: the whole point is that it changes after the loop
	// started, from another goroutine and possibly another replica. A bool the
	// server writes would be a data race; this is a read the server owns.
	//
	// It does NOT override the two start-time uses of Interactive — the
	// unbounded-iteration lift and the auto context-mode choice. Promoting a run
	// so an operator can correct it should not also silently remove its iteration
	// bound or rewrite how its history is kept; max_iterations is separately
	// overridable for the caller who wants that.
	//
	// nil = use the static flag, which is every run that was never retuned.
	InteractiveNow func(ctx context.Context) bool

	// ReResolveOnOperatorTurn, when non-nil, is consulted each time a PARKED run
	// receives its operator's next message — and only then.
	//
	// It exists because a parked run can be RETUNED: an operator changes the
	// model on a chat that is sitting waiting, and the next turn has to honour
	// it. RunOptions.Provider is a resolved provider held for the run's
	// lifetime, and parking happens INSIDE the loop, so without this the run
	// would keep using whatever it resolved to when it started, however long it
	// waits and whatever the operator changes.
	//
	// It returns changed=false when nothing moved, which is the overwhelmingly
	// common case — an ordinary conversational turn costs one store read, and
	// the provider is swapped only when the answer actually differs.
	//
	// An error is logged and ignored rather than failing the turn: a run that
	// cannot re-read its own configuration should continue on the settings it
	// has, not stop mid-conversation.
	ReResolveOnOperatorTurn func(ctx context.Context) (provider providers.Provider, model, effort string, changed bool, err error)

	// StartParked makes an interactive run park BEFORE its first model call
	// instead of after it.
	//
	// It exists for resume. A run that was idle awaiting the operator when it
	// paused has a conversation ending on an ASSISTANT turn, and there is no
	// pending turn for the model to answer — re-entering the loop normally
	// would send the provider a trailing assistant message. So such a run used
	// to be refused and marked failed, which is a poor answer for the chat
	// someone was in the middle of.
	//
	// Parking first restores the run to exactly what it was doing when it
	// paused: waiting. The operator's next turn continues it, through the same
	// steer queue and the same park code every live interactive run uses.
	//
	// Requires Interactive + SteerQueue; ignored otherwise, because a
	// non-interactive run has no operator to wait for.
	StartParked bool

	// ArmTurnCancel, when non-nil, makes THIS run turn-cancellable (RFC BH). At
	// the start of each turn the loop calls it with the turn's CancelCauseFunc to
	// register the run's currently-armed per-turn cancel token; the returned
	// disarm is called at the turn boundary. Firing the token (cause
	// ErrTurnCancelled, with the RUN ctx still alive) stops the current turn's
	// model call + tool dispatch and parks the run at awaiting_input instead of
	// terminating it. nil = not turn-cancellable — the loop behaves EXACTLY as it
	// did before (the wrapping turn ctx is never fired). Wired for interactive
	// runs only; the server passes a closure that arms its turncancel.Registry.
	ArmTurnCancel func(turnCancel context.CancelCauseFunc) (disarm func())

	// PauseGate, when non-nil, cooperatively quiesces this run for the
	// runtime-wide pause/snapshot protocol (RFC X / F41). At the TOP of each
	// iteration the loop asks PauseRequested(); if true it calls Park, which
	// persists pause_state='paused', blocks until resume (or ctx cancel), then
	// restores 'running'. nil disables pausing (direct loop callers / tests).
	PauseGate PauseGate

	// InitialState is the Σ a RESUMED stateful run starts from (RFC DH P2).
	//
	// ⚠️ A STATEFUL RUN'S HISTORY IS NOT ITS MESSAGES. PriorMessages carries a
	// replayed transcript, which is exactly what stateful mode exists to not
	// have — the model is fed only (Σ, observation). A resumed run handed only
	// PriorMessages therefore starts from an EMPTY Σ and cheerfully continues a
	// conversation whose every established fact it has forgotten, which is a
	// worse failure than refusing to resume: it looks like it worked.
	//
	// nil = start empty, which is correct for a fresh run.
	InitialState map[string]any

	// InitialObservation, when non-empty, is a stateful run's FIRST observation
	// verbatim, in place of one rendered from PriorMessages + Segments. Ignored
	// by the append/recap loop.
	//
	// ⚠️ THE OTHER HALF OF A STATEFUL RESUME. InitialState restores what the run
	// knew; this restores what it was looking at — the result of the action it
	// chose before it paused, or the operator's message a continuation carries.
	// Without it the first observation was rendered from a replayed transcript:
	// every operator message and every answer so far, relabelled "Task:", and
	// unbounded — the history stateful mode exists not to feed back.
	InitialObservation string

	// PriorMessages is the conversation history to prepend before the
	// caller's new Segments. Used by the continuation endpoint to replay
	// a session's prior turns. Empty for a fresh run (the v0.2 case).
	PriorMessages []providers.Message

	// OnHeartbeat fires once at the start of each iteration (after the
	// previous iteration's events have all drained). The HTTP server
	// uses this to update runs.last_heartbeat_at — foundation for the
	// v0.4.x sweeper that detects crashed runs by stale heartbeats.
	// Optional; nil disables. Called from the loop goroutine,
	// synchronously, so implementations should be cheap (single
	// UPDATE) and tolerate ctx cancellation gracefully.
	OnHeartbeat func()

	// MaxTokens caps per-iteration assistant output. Zero = let the
	// driver pick its default (4096 in the anthropic driver, which
	// is far below modern haiku/sonnet output ceilings of 8k–64k).
	// Agents that emit large structured output (verdicts JSON for
	// big batches, long-form rewrites) need to set this explicitly
	// or they truncate mid-output. The HTTP server populates this
	// from cfg.Agents[X].MaxTokens at request time.
	MaxTokens int

	// MaxContextTokens is the resolved per-agent context WINDOW (RFC CJ; per-run
	// > per-agent, merged by the caller), distinct from MaxTokens (output). The
	// loop passes it to the driver (Ollama → options.num_ctx) AND uses it as the
	// effective window for the auto-compaction threshold + the gauge, clamped to
	// the provider's real max (a cloud budget can only lower). 0 = no override.
	MaxContextTokens int

	// Effort is the reasoning-effort hint plumbed through to the
	// driver. One of "low" / "medium" / "high" or empty (= no hint).
	// Per-driver translation lands in PR 3 of the resolve-matrix
	// series — Anthropic maps to thinking.budget_tokens, OpenAI to
	// reasoning_effort, DeepSeek to its V4 thinking-mode toggle,
	// Ollama no-op. PR 1 plumbs it through providers.Request
	// unchanged; drivers in PR 1 ignore the field entirely.
	Effort string

	// Sampling carries the resolved per-agent LLM sampling params (per-run >
	// per-agent already merged by the caller). The loop maps it onto the flat
	// providers.Request fields; each driver applies what it supports. nil =
	// provider defaults.
	Sampling *config.Sampling

	// Compaction carries the resolved per-agent compaction settings (already
	// merged: per-run/per-spawn > parent-inherited > child def). When Enabled
	// and the provider reports a context window, the loop auto-compacts at a
	// top-of-iteration boundary once used/window crosses AutoCompactAtPct. nil =
	// no auto-compaction (manual + Context op=compact still work). Defaults are
	// applied at use-time (see config.CompactionDefault*).
	Compaction *config.Compaction

	// Context carries the resolved per-agent layered-context / retention settings
	// (already merged: per-run > parent-inherited > child def; RFC CR). When
	// Mode=="recap" and the provider reports a context window, the loop distils the
	// fed history by reasoning-recap at a top-of-iteration boundary once used/window
	// crosses AutoRecapAtPct — replacing the append+compaction path for the run.
	// nil / Mode=="append" = today's behavior. Defaults applied at use-time (see
	// config.ContextDefault*).
	Context *config.Context

	// RecallIndex is the run-scoped index recall-augmented distillation harvests
	// into. When non-nil (the server builds it only when context.recall is set AND
	// an embedder is configured), each distillation — compaction, recap, stateful
	// — embeds the span it evicts into this index, and the loop stamps it on ctx so
	// the Recall builtin can query it. nil = recall off (byte-identical to before).
	//
	// Deliberately NOT set on the resume path: a resumed run replays its past
	// distillations through applyCompactSummary, which never harvests, so leaving
	// this nil there avoids double-indexing. Like BankCompactedSpan, a callback-free
	// handle the loop only ever reads.
	RecallIndex *recall.Index

	// ContextPlugins is the runtime-wide context-transform chain (RFC Z / F43):
	// fast, built-in transforms applied to a COPY of the outbound request each
	// turn (e.g. secret redaction), in order. nil/empty = no chain. Built once
	// by the server and shared read-only. Skipped for the synthetic code-js
	// provider (local replay; redacting its bytes would trip replay divergence).
	ContextPlugins []contextplugin.Plugin

	// ToolParallelism caps how many tool_calls from a single
	// assistant turn run concurrently. Zero = use the package
	// default (8). Models like Anthropic and DeepSeek often emit
	// 2-5 tool_calls per turn; the Agent built-in tool turns each
	// of those into a full sub-agent run, so a serial dispatch
	// (the pre-2026-05-09 default) was forcing fan-outs of 3
	// sub-agents to run back-to-back instead of in parallel.
	//
	// Set to 1 to force serial dispatch (debug / determinism).
	// Setting it higher than the number of pending tool_calls is
	// harmless — the bound is never hit. Per-iteration; the loop
	// has no global cap on aggregate concurrency across runs (the
	// HTTP server's MAX_CONCURRENT_RUNS slot bounds the run tree
	// already).
	ToolParallelism int

	// AgentName is the operator-config key for the running agent
	// (e.g. "qa-agent", "company-researcher"). Threaded through so
	// the Hooks dispatcher can filter tool-use hooks by agent
	// selector. Empty string is fine for direct loop callers that
	// don't go through the agent yaml; the hooks dispatcher then
	// only fires hooks with `agents: ["*"]`.
	AgentName string

	// CodeBody is the inline code-js orchestrator source (RFC J),
	// resolved from the agent's AgentDef by the caller. Threaded onto
	// providers.RunMeta so the code-js provider runs it instead of
	// reading agent_code/<name>/index.js. Empty for every LLM agent
	// and for filesystem-backed code agents.
	CodeBody string

	// Metadata / PayloadMetadata are the run's NON-SECRET structured
	// metadata (repo name, review policy, …). The caller resolves them from
	// the trigger (WebHook/Schedule def, or a /v1/runs body). Stamped onto
	// providers.RunMeta for code-js agents; for LLM agents the run-build path
	// serialises them into prompt segments (trusted-text + untrusted-block).
	// Never secrets — those stay on the RunIdentity credentials path.
	Metadata        map[string]any
	PayloadMetadata map[string]any

	// RunTimeoutSeconds is the effective per-run/per-agent code-js wall-clock
	// budget override (per-run wins over per-agent; resolved by the caller).
	// 0 ⇒ the code-js provider's global default. Stamped onto RunMeta; LLM
	// drivers ignore it.
	RunTimeoutSeconds int

	// UserTier is the v0.8.2 user-facing-tier policy name applied
	// to this run. Informational on the loop side — appears on
	// store.Run.UserTier + agent-loop log lines so cost/compliance
	// queries can facet by tier. The actual fallback behaviour is
	// driven by FallbackPolicy below (also set by the HTTP server
	// from the operator's user_tiers yaml). Empty when no user_tier
	// was supplied / configured.
	UserTier string

	// FallbackPolicy controls the v0.8.2 runtime fallback path —
	// the loop switches to the next-in-queue provider when a call
	// returns a retryable error (rate limit, 5xx, network, stream-
	// idle). Zero value = disabled (preserves v0.7.x error-out
	// semantics). When Enabled is true, ReResolve MUST be non-nil.
	FallbackPolicy FallbackPolicy

	// ReResolve is the runtime-fallback callback. Called when a
	// provider call returns a retryable error AND FallbackPolicy.
	// Enabled AND attempts < MaxAttempts. The callback marks the
	// failed (provider, model) stalled in the resolver, picks the
	// next-in-queue, and returns the new Provider + Model + Effort.
	//
	// Returning an error from ReResolve means the resolver couldn't
	// find another candidate — the loop then surfaces the original
	// error to the caller (no further fallback attempts on this run).
	//
	// Nil disables runtime fallback regardless of FallbackPolicy.
	// Both fields are set together by the HTTP server / gRPC server
	// when the operator's user_tier policy enables fallback.
	ReResolve func(ctx context.Context, failedProvider, failedModel string, cause error) (provider providers.Provider, model string, effort string, err error)

	// Hooks is the tool-use hook dispatcher. Optional; nil disables
	// all hook invocation (the loop runs tool dispatch directly,
	// preserving pre-v0.7.x behaviour). When non-nil, each
	// concurrently-dispatched tool_call has its Pre chain invoked
	// before executeTool and its Post chain invoked after, per the
	// fail-mode / chain-order contract on hooks.Dispatcher.
	Hooks *hooks.Dispatcher

	// MarkStalled is the resolver feedback hook: the loop calls it
	// when this iteration's provider call surfaced an error that
	// suggests the (provider, model) pair is broken. The resolver
	// flips its Stalled flag for that pair, the next probe sweep
	// either revives it (if /v1/models still lists the model and
	// the endpoint is healthy) or confirms the stall.
	//
	// Optional: nil disables stall feedback. When non-nil, the
	// loop calls it on:
	//   - non-context errors returned by Provider.Call (driver
	//     gave up after retries; the request never opened a stream)
	//   - EventError frames in the response stream (driver opened a
	//     stream but the provider then 5xx'd or the model 404'd
	//     mid-iteration)
	//
	// The loop intentionally over-reports rather than under-reports:
	// the cure for a false-positive stall is cheap (next probe
	// clears it within ResolveProbeInterval), the cost of a missed
	// stall is misleading 503s pinned on a recovered provider until
	// the periodic probe catches up. PR 2 keeps the discrimination
	// simple; tighten in follow-ups if over-reporting becomes
	// observable noise.
	MarkStalled func(provider, model, reason string)

	// ClearStall is the resolver-recovery hook companion to
	// MarkStalled. The loop calls it once per iteration that
	// completes WITHOUT a provider/stream error against the
	// current (provider, model) — the most direct possible
	// evidence the pair is healthy now. Clears any prior
	// process-lifetime stall flag the matrix may still be
	// holding from an older transient failure.
	//
	// Optional: nil disables success feedback (preserves the
	// previous behavior where stalls only cleared on the next
	// periodic probe). Addressed 2026-05-15: with N=2 candidate
	// tiers, a stall surviving past a successful call on the
	// same pair was collapsing the cascade between probes.
	ClearStall func(provider, model string)

	// MarkRateLimited is the resolver-feedback hook for 429
	// rate-limit responses that exhausted the driver's internal
	// retry budget. Distinct from MarkStalled: rate-limit is
	// transient ("slow down for a moment"), not "the model is
	// broken for the probe interval".
	//
	// The loop calls this — instead of MarkStalled — when the
	// surfaced error is a 429. The matrix records a time-bound
	// cooldown that self-recovers without waiting for the next
	// probe; subsequent Resolve calls during the cooldown either
	// fall through to the next tier candidate (when fallback is
	// configured) or surface a clean tier-unavailable error.
	//
	// retryAfter is the duration to mark the model unavailable.
	// Pass 0 to use the resolver's default (30s). When/if a future
	// version threads provider Retry-After headers up from the
	// driver, pass the parsed duration here.
	//
	// Optional: nil disables rate-limit feedback (the model gets
	// retried on every subsequent run until the rate window
	// resets organically — worse for the next caller's latency
	// but no worse than pre-v0.12.7).
	//
	// Background: the v0.12.7 x1000 load test (2026-05-26)
	// discovered that treating 429 like 5xx in MarkStalled
	// poisoned the matrix for the full 15-min probe interval
	// after a single rate-limit storm. PR #235 patched this
	// with a skip-on-429 guard at the MarkStalled call site;
	// MarkRateLimited is the proper structural fix.
	MarkRateLimited func(provider, model string, retryAfter time.Duration)

	// MaxSameProviderRetries caps retryable-error retries against the
	// CURRENT (provider, model) before MarkRateLimited cools the
	// matrix entry and ReResolve escalates to a different provider.
	// Real providers' 429s often clear within seconds (much shorter
	// than the 30s MarkRateLimited cooldown), so retrying the same
	// pair 1-3 times with exponential backoff often recovers the
	// run cheaper than escalating — AND it's the only resilience
	// path for single-provider configurations where ReResolve has
	// no fallback target.
	//
	// 0 (default, v0.12.x behaviour) — the FIRST retryable error
	// fires MarkRateLimited and propagates / falls back immediately.
	// 1-3 — retry the same pair with 100ms / 300ms / 900ms backoff
	// before MarkRateLimited / ReResolve. Capped internally at 5 to
	// avoid pathological retry storms.
	//
	// Applies to BOTH paths:
	//   - Call() returns error before the stream opens (driver
	//     refused the request, network issue, etc.)
	//   - EventError frame in-stream (driver opened the stream then
	//     surfaced a retryable error)
	//
	// Non-retryable errors (400 / 401 / 403 / 422 / context.Canceled)
	// are NOT retried regardless of this setting — they're surfaced
	// immediately so the caller can see the real cause.
	//
	// Sourced from cfg.UserTiers[req.user_tier].RetryAttempts on the
	// HTTP layer. The HTTP layer caps at 5 before passing to the loop.
	MaxSameProviderRetries int
}

// PauseGate is the loop's seam into the runtime pause/quiesce protocol
// (internal/pause). The server builds a per-run implementation wrapping the
// pause Manager + store; the loop stays free of pause/store imports. Both
// methods are no-ops behind a nil RunOptions.PauseGate.
type PauseGate interface {
	// PauseRequested reports whether a runtime pause is in effect. Cheap +
	// non-blocking; the loop calls it at the top of every iteration.
	PauseRequested() bool
	// Park persists this run's pause_state='paused', blocks until the runtime
	// resumes (or ctx is cancelled), then restores 'running'. Returns ctx.Err()
	// if cancelled while parked (the loop then exits cleanly). A no-op return
	// (nil, no block) is valid if the pause was already lifted (lost race).
	Park(ctx context.Context) error
}

// FallbackPolicy controls the v0.8.2 runtime fallback path. The HTTP
// server builds this from cfg.UserTiers[req.user_tier]; the gRPC
// server does the same. Zero value disables fallback (preserves
// v0.7.x error-out semantics).
//
// Per-tier policy split: free tiers ship with Enabled=false (a 429
// becomes a hard error so cost caps hold); paid tiers ship with
// Enabled=true and MaxAttempts=3 (the cumulative cap across providers
// — a run that climbs Anthropic → DeepSeek → Gemini consumes all 3).
type FallbackPolicy struct {
	// Enabled gates the fallback path. False = ReResolve is never
	// consulted, errors propagate as in v0.7.x.
	Enabled bool

	// MaxAttempts is the cumulative cap on provider switches per
	// run. The original provider doesn't count — only the SWITCHES.
	// A run that successfully cascaded Anthropic → DeepSeek (1
	// switch) → Gemini (2 switches) consumed 2 attempts; the 3rd
	// would be allowed when MaxAttempts >= 3. Zero falls back to
	// the package default of 3.
	MaxAttempts int

	// UserTierName is the operator-declared tier name that
	// authorised this fallback policy. Informational — appears on
	// the EventProviderFallback payload so log + UI consumers can
	// attribute the switch to a specific tier.
	UserTierName string

	// PinAfterSuccess, when true, suppresses provider fallback on
	// retryable errors AFTER the run has completed at least one
	// successful turn (assistant message appended to the
	// conversation history). The initial turn can still fall back,
	// so a stale-probe initial pick doesn't kill the run. Once a
	// provider has touched the transcript, the run stays on it.
	//
	// Why: cross-provider mid-conversation fallback exposes a
	// growing surface of provider-specific transcript translation
	// bugs (Anthropic cache_control, DeepSeek reasoning_content +
	// thinking-mode validation, gemini thoughtSignature, tool_call
	// shape differences). Each requires its own translation
	// layer. Pinning closes the entire class of bug in exchange
	// for dropping resilience to MID-CONVERSATION provider issues —
	// same-provider rate-limit retry (internal/providers/ratelimit/)
	// still covers transient errors within one provider.
	//
	// Sourced from cfg.Env.FallbackPinAfterSuccess (env:
	// LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS). Default OFF in v0.8.x;
	// plan to flip default-on in v0.9.x.
	//
	// When pinning suppresses a fallback, the loop emits
	// EventFallbackSuppressed so operators can attribute the
	// failure to the policy rather than thinking the resolver
	// misbehaved.
	PinAfterSuccess bool
}

// defaultMaxFallbackAttempts is the cumulative cap when the operator
// yaml leaves MaxAttempts at 0. Three attempts gets a run from a
// stalled Anthropic through two more providers — enough to recover
// from a single-provider outage without consuming dozens of attempts
// against a deeper backbone-wide issue.
const defaultMaxFallbackAttempts = 3

// maxSameProviderRetriesCap is the hard ceiling on
// RunOptions.MaxSameProviderRetries. Operators can set higher in
// yaml but the loop clamps to this — pathological retry counts
// would cause a single retryable error to absorb minutes of
// backoff before propagating. Five attempts × the 100/300/900/2700/
// 8100ms backoff schedule = ~12s total worst case, the longest
// stretch we're willing to delay a single iteration's error reply.
const maxSameProviderRetriesCap = 5

// maxIterationsHardCeiling bounds the loop turns of an UnboundedIterations
// provider (code-js), which is otherwise exempt from MaxIterations. It is NOT
// the real bound — the provider's run-level wall-clock timeout terminates the
// run first — only a defense-in-depth backstop against a provider that never
// settles or errors. Sized far above any realistic sequential-tool-call count.
const maxIterationsHardCeiling = 1 << 20

// sameProviderRetryBackoff returns the duration to sleep before the
// nth retry of the same (provider, model). Exponential: 100ms,
// 300ms, 900ms, 2.7s, 8.1s — 3× per attempt. Most provider 429s
// resolve in seconds; 100ms / 300ms catches the fast-clearing
// transients, 900ms catches a typical Anthropic burst-window
// release.
func sameProviderRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// 100 * 3^(attempt-1) ms, in time.Duration
	d := 100 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 3
	}
	return d
}

// fallbackOutcome enumerates what tryProviderFallback decided after
// classifying the error and consulting policy + budget.
type fallbackOutcome int

const (
	// fallbackOutcomeNotEligible — the error class wasn't retryable,
	// the policy was disabled, or the budget was exhausted. Caller
	// propagates the original error via the existing error path.
	fallbackOutcomeNotEligible fallbackOutcome = iota

	// fallbackOutcomeSwitched — the loop's opts.Provider /
	// opts.Model / opts.Effort were swapped in place. Caller
	// re-runs the current iteration body against the new provider
	// (typically via `continue` on the outer loop).
	fallbackOutcomeSwitched

	// fallbackOutcomeReResolveFailed — fallback was eligible AND the
	// caller invoked ReResolve, but no replacement candidate was
	// available (resolver exhausted the user_tier's candidate list).
	// Caller propagates the original error (the fallback path is
	// terminal for this run).
	fallbackOutcomeReResolveFailed
)

// tryProviderFallback classifies the error, consults the run's
// FallbackPolicy + cumulative attempts counter, and (if all three
// permit) calls ReResolve to swap to the next provider. On success
// it mutates *opts in place — caller continues the iteration body
// against the new provider as if nothing happened.
//
// The mutation is intentional: opts is a value-receiver in Run, so
// the swap is local to this loop invocation. The caller of Run
// passed providers + initial state and doesn't re-read them.
//
// Emits EventProviderFallback on every successful switch. Emits
// EventCacheInvalidated when an Anthropic provider is swapped out
// (the only provider with operator-controlled cache_control
// breakpoints today; gemini-implicit-cache and others don't surface
// a knob, so swap-away isn't a meaningful invalidation event).
// Emits EventReasoningInvalidated when the strip pass cleared any
// assistant-turn Reasoning field on switching providers — see the
// in-body comment for the cross-provider thinking-content rationale.
//
// `messages` is the in-flight conversation history. On a successful
// switch, every assistant turn's Reasoning field is zeroed in place.
// Slice-element write is required (not a range-copy) so the caller's
// slice sees the update.
func tryProviderFallback(
	ctx context.Context,
	opts *RunOptions,
	attempts *int,
	cause error,
	emit func(providers.Event),
	messages []providers.Message,
	firstTurnSucceeded bool,
) fallbackOutcome {
	if !opts.FallbackPolicy.Enabled || opts.ReResolve == nil {
		return fallbackOutcomeNotEligible
	}
	cls := providers.ClassifyError(cause)
	// ErrorClassDeprecated is treated like Retryable for fallback
	// purposes (mark stalled + re-resolve to next candidate) — the
	// downstream effect on the resolver matrix is the same. The
	// difference is operator-visible: EventProviderFallback's Reason
	// surfaces "deprecated" so the operator can distinguish a
	// retired-model event from a transient 5xx.
	if cls != providers.ErrorClassRetryable && cls != providers.ErrorClassDeprecated {
		return fallbackOutcomeNotEligible
	}
	maxAttempts := opts.FallbackPolicy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxFallbackAttempts
	}
	if *attempts >= maxAttempts {
		return fallbackOutcomeNotEligible
	}
	// PinAfterSuccess: once a turn has succeeded, the conversation
	// transcript has provider-specific state (tool_call format,
	// possibly reasoning content, possibly Anthropic cache_control
	// breakpoints). Cross-provider fallback past this point opens
	// the same class of translation bugs the v0.8.12 reasoning
	// strip was a point fix for. Suppress and emit a typed event
	// so operators can attribute the failure to the policy.
	if opts.FallbackPolicy.PinAfterSuccess && firstTurnSucceeded {
		emit(providers.Event{
			Type: providers.EventFallbackSuppressed,
			Text: fmt.Sprintf("fallback from %s suppressed: provider pinned after first successful turn (LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS=1); run will fail with cause: %s", opts.Provider.ID(), truncateError(cause)),
		})
		return fallbackOutcomeNotEligible
	}
	// Ctx already cancelled? Don't fall back — caller signalled
	// abandon, classify would have caught explicit ctx.Canceled but
	// race-stops can have the loop here with a fresh-looking error.
	if ctx.Err() != nil {
		return fallbackOutcomeNotEligible
	}

	failedProvider := opts.Provider.ID()
	failedModel := opts.Model
	newProvider, newModel, newEffort, rerr := opts.ReResolve(ctx, failedProvider, failedModel, cause)
	if rerr != nil {
		return fallbackOutcomeReResolveFailed
	}

	// RFC AT §4.4: if this run carries an image content block, the fallback
	// target must also accept image input. The initial-call gate before the
	// main loop only checks the FIRST resolved provider; without this re-check,
	// a mid-run swap to a text-only provider (e.g. DeepSeek, which wraps the
	// OpenAI driver and overrides SupportsVision=false) would let the image
	// part reach the upstream and the provider would 400 with a raw
	// "unknown variant 'image_url'" — exactly what RFC AT §4.4 says must NOT
	// happen. Fail loudly here instead of leaking a request that's structurally
	// invalid for the target. Skip *attempts++ + EventProviderFallback because
	// no switch actually happens; emit EventFallbackSuppressed so the operator
	// sees the refused candidate.
	if messagesHaveImage(messages) && !newProvider.Capabilities().SupportsVision {
		emit(providers.Event{
			Type: providers.EventFallbackSuppressed,
			Text: fmt.Sprintf("fallback from %s/%s to %s/%s suppressed: run carries an image but the fallback target does not support vision (RFC AT §4.4); run will fail with cause: %s", failedProvider, failedModel, newProvider.ID(), newModel, truncateError(cause)),
		})
		return fallbackOutcomeNotEligible
	}

	*attempts++

	emit(providers.Event{
		Type: providers.EventProviderFallback,
		Fallback: &providers.FallbackInfo{
			FailedProvider: failedProvider,
			FailedModel:    failedModel,
			NewProvider:    newProvider.ID(),
			NewModel:       newModel,
			Attempt:        *attempts,
			UserTier:       opts.FallbackPolicy.UserTierName,
			Reason:         cls.String(),
			CauseError:     truncateError(cause),
		},
	})

	// Cache-loss event when switching AWAY from a native-prompt-cache provider
	// (Anthropic today) to one without it. The cache_control breakpoints in the
	// system block (and on system_prompt segments) live only on providers that
	// advertise NativePromptCache; a switch off one drops that state so downstream
	// iterations run cache-cold. Keyed on Capabilities().NativePromptCache rather
	// than the literal id "anthropic" (RFC BF P2a) so an operator who names their
	// Anthropic provider anything still gets the event, and a hypothetical second
	// native-cache provider is covered without a code change. Switches INTO a
	// native-cache provider don't emit — the new provider has no cache to lose.
	if opts.Provider.Capabilities().NativePromptCache && !newProvider.Capabilities().NativePromptCache {
		emit(providers.Event{
			Type: providers.EventCacheInvalidated,
			Text: fmt.Sprintf("native prompt-cache breakpoints lost on switch from %s to %s; this run's downstream iterations will be cache-cold", failedProvider, newProvider.ID()),
		})
	}

	// Reasoning strip on cross-provider switch. `Message.Reasoning`
	// is a single string field with no provenance. The OpenAI driver
	// (which also backs DeepSeek) echoes it back as `reasoning_content`
	// on the wire. DeepSeek's API verifies that any echoed
	// reasoning_content matches what IT produced and 400s on mismatch
	// with "The reasoning_content in the thinking mode must be passed
	// back to the API." Cross-provider echoes always fail this check
	// because the content originated from a different provider.
	//
	// Production bug (2026-05-13): a tier=low run on gemini-2.5-flash
	// fell back to deepseek-v4-flash mid-conversation after a gemini
	// 503; the conversation carried Reasoning-populated assistant
	// turns into the deepseek request and deepseek 400'd. Fixed by
	// zeroing the field at fallback time so the new provider gets
	// a clean history.
	//
	// Safe across all current providers: Anthropic uses content blocks
	// for extended_thinking (not the Reasoning string field), Gemini's
	// driver doesn't write Reasoning today, OpenAI o-series tolerates
	// missing reasoning_content (treats as no prior thinking).
	stripped := 0
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].Reasoning != "" {
			messages[i].Reasoning = ""
			// Zero the Anthropic thinking-block signature too — it is only
			// valid for the model that produced it, so it must never ride to a
			// different provider (the new provider ignores the field, but the
			// deepseek→other→anthropic bounce must not replay a stale seal).
			messages[i].ReasoningSignature = ""
			stripped++
		}
	}
	if stripped > 0 {
		emit(providers.Event{
			Type: providers.EventReasoningInvalidated,
			Text: fmt.Sprintf("cleared reasoning_content from %d assistant turn(s) on switch from %s to %s; cross-provider echo would 400", stripped, failedProvider, newProvider.ID()),
		})
	}

	// Thinking-model downgrade on cross-provider switch (exp7 R2). A DeepSeek-
	// family thinking model (deepseek-reasoner / *-pro) requires provider-
	// produced reasoning_content on every assistant turn and 400s
	// ("reasoning_content ... must be passed back") on a turn lacking it. After
	// this switch the history's assistant turns are all reasoning-less — a
	// foreign provider produced them (Anthropic/Gemini never set Reasoning), or
	// the strip above zeroed them (the deepseek→other→deepseek-reasoner bounce).
	// The strip can't fix this (it removes reasoning, can't synthesise it), so
	// downgrade to the non-thinking sibling for the remaining iterations rather
	// than let the request 400. No assistant turn yet ⇒ nothing to satisfy ⇒ no
	// downgrade (a fresh history is fine for a thinking model).
	if dg, ok := newProvider.(providers.ThinkingDowngrader); ok && hasReasoningLessAssistantTurn(messages) {
		if sibling, downgraded := dg.NonThinkingSibling(newModel); downgraded {
			emit(providers.Event{
				Type: providers.EventModelDowngraded,
				Text: fmt.Sprintf("downgraded %s to non-thinking %s on switch to %s (dropped the effort hint): the fallback history carries assistant turns without reasoning_content, which the thinking model would reject", newModel, sibling, newProvider.ID()),
			})
			newModel = sibling
			// Also drop the effort hint. A "non-thinking sibling" is only actually
			// non-thinking if the request doesn't ALSO carry a reasoning_effort
			// that turns thinking back ON. DeepSeek's V4 line is hybrid: the driver
			// maps Request.Effort → reasoning_effort (openai driver), and
			// reasoning_effort re-enables thinking mode REGARDLESS of the
			// -flash/-pro model name. Without this, the downgraded flash request
			// still runs in thinking mode and 400s on the (reasoning-less,
			// just-stripped) history with "reasoning_content ... must be passed
			// back" — silently defeating the downgrade. Production 2026-07-01: an
			// ollama-local qwen3.6 crash fell back to deepseek-v4-pro, the loop
			// downgraded it to deepseek-v4-flash, and the call STILL 400'd because
			// effort=high (inherited from the qwen3.6 thinking run) kept thinking on.
			newEffort = ""
		}
	}

	opts.Provider = newProvider
	opts.Model = newModel
	opts.Effort = newEffort
	return fallbackOutcomeSwitched
}

// hasReasoningLessAssistantTurn reports whether the history contains an
// assistant turn with no reasoning_content. A DeepSeek-family thinking model
// rejects such a turn; after a cross-provider switch every assistant turn is
// reasoning-less (a foreign provider produced it, or the strip zeroed it).
func hasReasoningLessAssistantTurn(messages []providers.Message) bool {
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].Reasoning == "" {
			return true
		}
	}
	return false
}

// truncateError clips long error strings to 200 chars so a peer's
// 9 KB HTML 500 page doesn't flood the SSE wire on EventProviderFallback.
// Same shape as the lazy-MCP resolver's summariseErr in v0.8.1.
func truncateError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const maxLen = 200
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…(truncated)"
}

// RunResult is the terminal state after a Run.
type RunResult struct {
	StopReason string
	FinalText  string // concatenated text from the last assistant turn
	Iterations int
	Usage      providers.Usage // sum across iterations
	// State is the final structured execution state Σ of an L2 stateful run (RFC
	// CR `context.mode: stateful`); nil for append/recap runs.
	State map[string]any
	// ProposedSchema is the LAST state_schema the model proposed during a stateful
	// run (RFC CR model-proposed→operator-adopted), when it differs from the run's
	// active schema. Inert — surfaced for an operator to review + adopt (by forking
	// the agent def's context.state_schema). nil when nothing was proposed.
	ProposedSchema map[string]any
}

// ErrTurnCancelled is the cancel cause the server fires on a run's armed per-turn
// token (RFC BH). The loop catches it — the RUN ctx is still alive — and parks
// the interactive run instead of terminating (a whole-run cancel cause, e.g.
// cancel.ErrCancelledByAPI, is NOT this and still terminates). Callers wrap it
// with an operator reason via TurnCancelCause; errors.Is still matches.
var ErrTurnCancelled = errors.New("turn cancelled by operator")

// turnCancel wraps ErrTurnCancelled with an operator reason. It Unwraps to the
// sentinel so errors.Is(cause, ErrTurnCancelled) holds; reasonFromCause pulls the
// reason back out for the EventTurnCancelled payload.
type turnCancel struct{ reason string }

func (t *turnCancel) Error() string { return ErrTurnCancelled.Error() + ": " + t.reason }
func (t *turnCancel) Unwrap() error { return ErrTurnCancelled }

// TurnCancelCause builds the cancel cause the server fires on the per-turn token.
// reason == "" returns the bare sentinel; a non-empty reason is carried through
// to EventTurnCancelled. It lives here (not in internal/turncancel) so the leaf
// registry stays free of the loop package — the server imports loop already.
func TurnCancelCause(reason string) error {
	if reason == "" {
		return ErrTurnCancelled
	}
	return &turnCancel{reason: reason}
}

// reasonFromCause returns the operator reason carried by a TurnCancelCause, or ""
// for the bare sentinel / any other error.
func reasonFromCause(cause error) string {
	var t *turnCancel
	if errors.As(cause, &t) {
		return t.reason
	}
	return ""
}

// cancelledToolResults synthesizes an error-shaped tool_result for every pending
// tool_use a turn-cancelled turn started but never dispatched, so the next model
// call sees a well-formed assistant(tool_use)/user(tool_result) pairing — a
// dangling tool_use 400s Anthropic (RFC BH §9.1). Shape + IsError match how
// executePendingTools reports a ctx-cancelled tool, so a cancel mid-generation
// and a cancel mid-dispatch leave byte-comparable history.
func cancelledToolResults(pending []providers.ToolUse) []providers.ContentBlock {
	out := make([]providers.ContentBlock, len(pending))
	for i, tu := range pending {
		out[i] = providers.ContentBlock{
			Type:      "tool_result",
			ToolUseID: tu.ID,
			ToolName:  tu.Name,
			Text:      "cancelled by operator",
			IsError:   true,
		}
	}
	return out
}

// finishTurnCancel completes an operator-cancelled turn: it emits the
// turn_cancelled marker, disarms the turn-cancel token (the run is no longer
// mid-turn, so a racing cancel 409s), and parks an interactive run at
// awaiting_input. It returns the updated messages, the refreshed footprint, and
// resumed=true when the operator's next message arrives (the caller continues the
// loop) or false to terminate — ctx cancelled during the park, or a
// non-interactive run (the handler 409s that case, so it is only a safety net:
// stopping a non-interactive run's only turn would terminate it).
func finishTurnCancel(ctx context.Context, opts *RunOptions, messages []providers.Message, sinceTurn, lastCtxTokens, preambleTokens int, reason string, emit func(providers.Event), disarm func()) ([]providers.Message, int, bool) {
	emit(providers.Event{Type: providers.EventTurnCancelled,
		TurnCancelled: &providers.TurnCancelledEventInfo{Reason: reason, SinceTurn: sinceTurn}})
	disarm()
	if opts.interactiveAtBoundary(ctx) && opts.SteerQueue != nil {
		return parkForOperatorTurn(ctx, opts, messages, sinceTurn, lastCtxTokens, preambleTokens, emit)
	}
	return messages, lastCtxTokens, false
}

// Run drives the agent loop to completion.
// parkHeartbeatInterval is how often a parked interactive run pulses
// OnHeartbeat while idle, so the staleness sweeper doesn't reap it (the
// per-iteration heartbeat is suspended during the block). Matches the
// interruption tool's blocked-heartbeat cadence. A var (not const) so tests
// can lower it; not an operator knob.
// DefaultMaxIterations is the loop bound an agent gets when it names none.
//
// Exported because it is now reported to callers as the answer to "what will
// this run actually use", and a report that restated the number would be free to
// drift from the loop that enforces it. One constant, two readers.
const DefaultMaxIterations = 16

// ⚠️ A PACKAGE-LEVEL VAR THAT TESTS MUTATE. A test lowering it to keep itself
// fast is only safe while no OTHER goroutine is inside parkForInput, which
// reads it — and a test that starts an interactive run without AWAITING it
// leaves exactly such a goroutine behind. The race then surfaces in whichever
// innocent test runs next and writes the var, which is a long way from the test
// that actually leaked. Await your runs.
var parkHeartbeatInterval = 30 * time.Second

// parkForInput blocks a persistent interactive run until an operator steering
// message arrives or ctx is cancelled, ticking OnHeartbeat meanwhile so the
// idle run isn't reaped. Returns (msg, true) on input; (zero, false) on
// cancel or a closed queue.
func parkForInput(ctx context.Context, q <-chan steer.Message, heartbeat func()) (steer.Message, bool) {
	t := time.NewTicker(parkHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case m, ok := <-q:
			return m, ok
		case <-t.C:
			if heartbeat != nil {
				heartbeat()
			}
		case <-ctx.Done():
			return steer.Message{}, false
		}
	}
}

// parkForOperatorTurn parks a persistent interactive run at awaiting_input until
// the operator's next real message. It emits EventAwaitingInput, then blocks on
// the steer queue, handling a steer.KindCompact control inline (replace history
// with the summary pair + re-park; compaction is not itself new input) exactly
// like the terminal park it was extracted from. Returns the updated messages,
// the refreshed context footprint (changed only when a compaction ran), and
// whether a real operator turn arrived — false ⇒ ctx cancelled / queue closed ⇒
// the caller terminates the run.
func parkForOperatorTurn(ctx context.Context, opts *RunOptions, messages []providers.Message, sinceTurn, lastCtxTokens, preambleTokens int, emit func(providers.Event)) ([]providers.Message, int, bool) {
	emit(providers.Event{Type: providers.EventAwaitingInput,
		AwaitingInput: &providers.AwaitingInputEventInfo{SinceTurn: sinceTurn}})
	for {
		m, resumed := parkForInput(ctx, opts.SteerQueue, opts.OnHeartbeat)
		if !resumed {
			return messages, lastCtxTokens, false // ctx cancelled (or queue closed) → terminate
		}
		if m.Kind == steer.KindCompact {
			messages = applyCompactSummary(messages, m.Text, m.KeepN, m.KeepFirst, emit)
			// Refresh the footprint so the next operator turn's op=self reports the
			// compacted size, not the stale pre-compaction value. Without this a
			// parked run that compacted kept reporting its old ~full context
			// (used_tokens / used_pct) until the next real turn's usage landed.
			lastCtxTokens = estimatePromptTokens(preambleTokens, messages)
			continue // re-park: wait for the operator's actual next turn
		}
		messages = append(messages, providers.Message{
			Role:    "user",
			Content: []providers.ContentBlock{{Type: "text", Text: m.Text}},
		})
		if opts.OnSteer != nil {
			opts.OnSteer(m)
		}
		// RFC DC P3: a real operator turn is the ONE moment a parked run's
		// routing may have been retuned while it waited. Done here rather than
		// at each of the three park call sites so there is one place it can be
		// forgotten from, and none where it can disagree.
		reResolveForOperatorTurn(ctx, opts, emit)
		return messages, lastCtxTokens, true
	}
}

// reResolveForOperatorTurn swaps the run's live provider/model/effort when the
// operator retuned it while it was parked.
//
// It mutates opts through the same seam tryProviderFallback uses — that path
// already replaces these three mid-run, so a retune reaches an existing
// mutation point from an operator trigger instead of a failure.
//
// Silent on no-op, loud on change: an unchanged run is byte-identical to before
// this existed, and a changed one says so on the transcript, because a model
// that swaps mid-conversation with no trace makes the transcript a misleading
// record of what produced what.
func reResolveForOperatorTurn(ctx context.Context, opts *RunOptions, emit func(providers.Event)) {
	if opts.ReResolveOnOperatorTurn == nil {
		return
	}
	provider, model, effort, changed, err := opts.ReResolveOnOperatorTurn(ctx)
	if err != nil {
		// Continue on the settings we have. A run that cannot re-read its own
		// configuration should not stop mid-conversation over it.
		log.Printf("loop: could not re-resolve routing for the operator's turn: %v", err)
		return
	}
	if !changed || provider == nil {
		return
	}
	from := ""
	if opts.Provider != nil {
		from = opts.Provider.ID() + "/" + opts.Model
	}
	to := provider.ID() + "/" + model
	opts.Provider, opts.Model, opts.Effort = provider, model, effort

	// EventOverride, NOT EventProviderFallback. A fallback means the runtime
	// moved the run because something failed; this means a person chose to. A
	// reader who cannot tell them apart will read a deliberate retune as an
	// outage — and the two want opposite responses.
	emit(providers.Event{
		Type: providers.EventOverride,
		Text: "routing changed by operator: " + from + " → " + to,
		Override: &providers.OverrideInfo{
			Source:    "operator",
			FromModel: from,
			ToModel:   to,
			// Routing is what THIS event reports — the server emits a separate
			// one at retune time listing the keys the request set. See
			// OverrideInfo.Fields.
			Fields: []string{"model"},
		},
	})
}

// CompactionMessages builds the replacement conversation for a compaction: an
// optional pinned task (kept verbatim), the summary of the middle span, and the
// kept recent tail. Shape: [user(<pinned?> + <summary>), assistant(ack)] ++ keptTail.
// It always starts on a user turn; keptTail must be snapped to a clean user-turn
// boundary (compactionSplit) so the whole sequence alternates and never orphans a
// tool_use/tool_result. The system prompt is separate (re-derived) and untouched.
func CompactionMessages(pinnedTask, summary string, keptTail []providers.Message) []providers.Message {
	var b strings.Builder
	if strings.TrimSpace(pinnedTask) != "" {
		b.WriteString("[Original task — preserved verbatim:]\n")
		b.WriteString(pinnedTask)
		b.WriteString("\n\n")
	}
	b.WriteString("[Conversation so far, compacted to a summary — treat it as established context for everything before this point:]\n\n")
	b.WriteString(summary)
	out := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: b.String()}}},
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "text",
			Text: "Understood — I'll continue from the summary above."}}},
	}
	return append(out, keptTail...)
}

// isFreshUserTurn reports whether m is a user message that STARTS a turn (a real
// input), not a tool_result-carrying user turn. Cutting a kept tail to start here
// never orphans a preceding assistant(tool_use) — its tool_result stays in the
// summarized span.
func isFreshUserTurn(m providers.Message) bool {
	if m.Role != "user" {
		return false
	}
	return len(m.Content) == 0 || m.Content[0].Type != "tool_result"
}

// compactionSplit decides what to summarize vs keep: summarize msgs[firstIdx:cut],
// keep msgs[cut:]. firstIdx is 1 when keepFirst pins a leading user turn. cut snaps
// to a clean user-turn boundary that keeps AT LEAST keepLastN messages (snapping
// back to a boundary keeps a few more rather than splitting a tool cycle);
// keepLastN<=0 keeps none (cut=len). ok=false when nothing is worth summarizing.
func CompactionSplit(msgs []providers.Message, keepLastN int, keepFirst bool) (firstIdx, cut int, ok bool) {
	n := len(msgs)
	firstIdx = 0
	if keepFirst && n > 0 && isFreshUserTurn(msgs[0]) {
		firstIdx = 1
	}
	if keepLastN <= 0 {
		cut = n
	} else {
		target := n - keepLastN
		if target <= firstIdx {
			return firstIdx, firstIdx, false // keep-N spans the whole tail → nothing to summarize
		}
		cut = -1
		for i := target; i > firstIdx; i-- {
			if isFreshUserTurn(msgs[i]) {
				cut = i
				break
			}
		}
		if cut < 0 {
			cut = n // no clean boundary in range → summarize everything after firstIdx
		}
	}
	return firstIdx, cut, cut > firstIdx
}

// compactionKeptTailBudgetPct caps the kept-verbatim tail at this percent of the
// provider's reported context window. Without it a compaction can "succeed" yet
// still overflow: keep_last_n snaps to a huge tool-result tail whose size alone
// approaches (or exceeds) the window, so the next request overflows anyway — a
// slow local model timed out prefilling a post-compaction context that was
// STILL over its 131k window. Estimate-based (chars/4, which overcounts dense
// JSON/code), so it errs toward keeping LESS — the safe direction for a slow
// local model's prefill cost. The other ~half of the window is left for the
// summary, the next turn, and the model's response.
const compactionKeptTailBudgetPct = 50

// capKeptTailToWindow advances `cut` forward — dropping the OLDEST kept-verbatim
// turns into the summarized span, snapping to fresh-user-turn boundaries — until
// the kept tail (msgs[cut:]) fits within budget estimated tokens. budget<=0
// (window unknown, e.g. a provider that doesn't report one) → no cap. Never
// splits a turn or truncates content: if the tail collapses to a single
// over-budget turn (one giant tool_result) it stays, since dropping it would
// lose the agent's most recent context entirely — better over-budget-by-one-turn
// than empty. Returns the (possibly larger) cut; cut only ever increases, so the
// summarized span [firstIdx:cut] stays non-empty.
// splitOrCutToWindow is CompactionSplit with one override: when keep_last_n
// pins the whole conversation AND the pinned tail alone will not fit the
// window, the WINDOW WINS and the tail is cut to fit.
//
// ⚠️ THIS CLOSES A DELIBERATE DEFERRAL, and the backstop is what forced it. The
// declined-split early return never reached capKeptTailToWindow, so a run whose
// kept tail alone exceeded the window had no escape at all — keep_last_n could
// veto every distillation path and the run climbed to the provider's limit.
//
// A backstop that keep_last_n can veto is not a backstop. So the precedence is
// now explicit: keep_last_n is a PREFERENCE about how much to keep verbatim,
// and the window is a HARD LIMIT. A preference does not override a limit.
//
// It only overrides when the tail genuinely does not fit. A short conversation
// that keep_last_n spans is still declined — there is nothing to reclaim there,
// and cutting it would discard context for no gain.
func splitOrCutToWindow(msgs []providers.Message, keepLastN int, keepFirst bool, budget int) (firstIdx, cut int, ok bool) {
	firstIdx, cut, ok = CompactionSplit(msgs, keepLastN, keepFirst)
	if ok || budget <= 0 {
		return firstIdx, cut, ok
	}
	// The split declined: keep_last_n pins everything from firstIdx on. If that
	// fits the window there is nothing wrong — decline stands.
	if estimateMessageTokens(msgs[firstIdx:]) <= budget {
		return firstIdx, cut, false
	}
	// It does not fit. Cut forward until it does; if that leaves anything to
	// summarize, the distillation proceeds after all.
	forced := capKeptTailToWindow(msgs, firstIdx, budget)
	if forced <= firstIdx {
		return firstIdx, cut, false // irreducible — genuinely nothing to do
	}
	return firstIdx, forced, true
}

func capKeptTailToWindow(msgs []providers.Message, cut, budget int) int {
	if budget <= 0 {
		return cut
	}
	for cut < len(msgs) && estimateMessageTokens(msgs[cut:]) > budget {
		next := -1
		for i := cut + 1; i < len(msgs); i++ {
			if isFreshUserTurn(msgs[i]) {
				next = i
				break
			}
		}
		if next < 0 {
			break // remaining tail is one irreducible chunk — keep it rather than empty
		}
		cut = next
	}
	return cut
}

// messageText concatenates a message's text blocks (used to pin the task verbatim).
func messageText(m providers.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// estimateMessageTokens is a cheap chars/4 heuristic over message text + tool I/O
// — for the operator-facing before/after readout, not for billing.
// effectiveWindow resolves the context-window ceiling the distillation gate
// measures against, from whatever is known at the call site.
//
// EXTRACTED rather than duplicated. The seed at run start and the per-turn
// update need the identical answer, and the one thing that must not happen is
// the two disagreeing about how full the window is — a gate that fires on one
// formula and a gauge that reports another is a worse diagnostic than neither.
//
// `reported` is a per-CALL window when a driver supplies one (Ollama reads the
// model's actually-loaded context from /api/ps), 0 before any call has
// returned. The static capability is the fallback, and a per-agent budget can
// only LOWER the result — never enlarge a fixed cloud window.
func effectiveWindow(reported int, opts RunOptions) int {
	w := reported
	if w == 0 {
		w = opts.Provider.Capabilities().MaxContextTokens
	}
	if opts.MaxContextTokens > 0 && (w == 0 || opts.MaxContextTokens < w) {
		w = opts.MaxContextTokens
	}
	return w
}

// estimatePreambleTokens estimates the part of a request that is NOT the
// conversation: the system prompt and the tool catalogue.
//
// ⚠️ THIS IS THE HALF THE FOOTPRINT USED TO OMIT, and omitting it is not a
// rounding error — it decides the outcome. A chat/local agent on a 2048-token
// window measured 2 tokens of conversation against a real request of 3340: the
// gate stayed shut while the run sent 163% of its window, because the estimate
// counted three lines of chat and the preamble it ignored was the whole prompt.
//
// The worse the ratio of preamble to conversation, the further wrong it is —
// so a small window with a large system prompt and a wide tool catalogue, which
// is exactly the local-inference shape, is where it fails hardest.
func estimatePreambleTokens(system []providers.ContentBlock, toolSpecs []providers.ToolSpec) int {
	chars := 0
	for _, c := range system {
		chars += len(c.Text)
	}
	for _, ts := range toolSpecs {
		// Name + description + schema all ride the wire and all get billed.
		chars += len(ts.Name) + len(ts.Description) + len(ts.InputSchema)
	}
	return chars / 4
}

// estimatePromptTokens estimates what the provider will bill for the NEXT
// request: preamble + conversation.
//
// It exists so the seed and the post-distillation refreshes measure the same
// quantity the per-turn update does (InputTokens + cache, which the provider
// counts over the whole request). A gate driven by one number while the gauge
// reports another is a worse diagnostic than neither — the mistake this
// function's absence made, one field away from the window formula that was
// extracted to prevent exactly it.
func estimatePromptTokens(preambleTokens int, msgs []providers.Message) int {
	return preambleTokens + estimateMessageTokens(msgs)
}

func estimateMessageTokens(msgs []providers.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			chars += len(c.Text) + len(c.ToolInput)
		}
	}
	return chars / 4
}

// compactionPrompt builds the summarization system prompt for a target percentage
// (10..50). Shared by the loop (auto/self) and the server (manual) via Summarize.
func compactionPrompt(targetPct int) string {
	if targetPct < 10 || targetPct > 50 {
		targetPct = config.CompactionDefaultTargetPct
	}
	return fmt.Sprintf("You are compacting a conversation to free up the model's context window. "+
		"Produce a single concise summary — aim for roughly %d%% of the original length — that "+
		"preserves the user's goals and constraints, decisions made, facts and values established, "+
		"tool results that still matter, and any open threads or next steps. Write it as durable "+
		"context the assistant can rely on to continue. Output ONLY the summary prose — no preamble.", targetPct)
}

// RecapMaxChars bounds the History op=recap summary. A recap is a chat-list
// subtitle a human skims — a couple of lines beside the chat's title — so the
// length has to be a property of the TEXT, not something each surface clips off
// whatever came back.
const RecapMaxChars = 256

// recapMaxTokens caps the recap call so a model that ignores the length
// instruction still can't bill (or return) a paragraph. Generous next to
// RecapMaxChars (~64 tokens at 4 chars/token) so a compliant model finishes its
// sentence well inside the cap rather than being cut off mid-word.
//
// ⚠️ RAISED TO distillMinOutputTokens IN PRACTICE, and the trade is deliberate.
// 160 tokens is a tight cost guard against a verbose model and a guaranteed
// EMPTY RESULT against a reasoning one, which spends the whole cap thinking
// before it writes — the History recap would then be blank on exactly the local
// models it is cheapest to run. A looser cap costs tokens only when a model
// ignores the instruction; the tight one costs the feature.
const recapMaxTokens = 160

// recapPrompt is the op=recap counterpart to compactionPrompt. A recap is NOT a
// compaction: nothing is being freed and no assistant has to resume from it, so
// compactionPrompt's "roughly N% of the original length" — which on a long chat
// means several paragraphs of durable context — is the wrong target entirely.
// Ask for the two-sentence gist a human reads in a list.
func recapPrompt() string {
	return fmt.Sprintf("Summarize this conversation in at most two sentences, under %d characters, "+
		"to label it in a list of chats: what the user is working on and where it stands. "+
		"Write plain prose — no preamble, no bullet points, no markdown, no heading. "+
		"Output ONLY the summary.", RecapMaxChars)
}

// Summarize makes ONE provider call to compact msgs into a summary string.
// Shared by the loop (auto + self-compact) and the server (manual + terminal
// compaction). model may be a cheaper same-provider model.
func Summarize(ctx context.Context, provider providers.Provider, model string, msgs []providers.Message, targetPct int) (string, error) {
	return summarizeWith(ctx, provider, model, msgs, compactionPrompt(targetPct), "Conversation to compact:", 0)
}

// Recap makes ONE provider call to produce the SHORT chat summary behind History
// op=recap, which the server persists to sessions.summary for the chat list.
// Same mechanics as Summarize, different prompt and a token cap — see
// recapPrompt for why a recap can't just reuse the compaction summary.
func Recap(ctx context.Context, provider providers.Provider, model string, msgs []providers.Message) (string, error) {
	return summarizeWith(ctx, provider, model, msgs, recapPrompt(), "Conversation to summarize:", recapMaxTokens)
}

// summarizeWith is the shared body of Summarize and Recap. The conversation is
// flattened into a single user message (role-tagged) so the call is valid
// regardless of how msgs alternates, and the model clearly sees "summarize
// this". NO tools are offered → the summary call cannot re-enter the tool /
// compaction machinery. maxTokens 0 leaves the provider default.
// distillMinOutputTokens is the FLOOR on what a summarizer may spend, as
// distinct from how long a summary is asked for.
//
// ⚠️ THE TWO WERE THE SAME NUMBER, and on a thinking model that is fatal.
// recap_max_chars asks the PROMPT for a length; MaxTokens caps what the model
// may EMIT — and a reasoning model spends that cap thinking before it writes
// anything. At the 512-char default the cap was 192 tokens, so qwen3.6 reasoned
// past it and returned no text at all: a silent empty_summary on every attempt,
// reported to the operator as "raise recap_max_chars", which is advice to
// lengthen the summary in order to fix a budget.
//
// A floor costs nothing when the model complies — a compliant summarizer stops
// when it is done, it does not fill the cap — and it is the difference between
// a distiller that works on a local reasoning model and one that never can.
const distillMinOutputTokens = 1024

// summarizeEffort is what a distillation call asks for, and it asks for as
// little reasoning as the provider will give it.
//
// A recap is mechanical: fold a span of transcript into a shorter note. There
// is no reasoning worth paying for, and on a small budget the reasoning is
// exactly what crowds out the answer. Anthropic maps low to NO thinking block,
// Ollama to think:false — the two providers where an unset hint means "use the
// model's own default", which for qwen3/deepseek-r1 is to think.
const summarizeEffort = "low"

func summarizeWith(ctx context.Context, provider providers.Provider, model string, msgs []providers.Message, system, lead string, maxTokens int) (string, error) {
	var convo strings.Builder
	for _, m := range msgs {
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					fmt.Fprintf(&convo, "%s: %s\n\n", m.Role, c.Text)
				}
			case "tool_use":
				fmt.Fprintf(&convo, "%s [tool_use %s]: %s\n\n", m.Role, c.ToolName, string(c.ToolInput))
			case "tool_result":
				fmt.Fprintf(&convo, "%s [tool_result]: %s\n\n", m.Role, c.Text)
			}
		}
	}
	if maxTokens > 0 && maxTokens < distillMinOutputTokens {
		maxTokens = distillMinOutputTokens
	}
	req := providers.Request{
		Model:     model,
		System:    []providers.ContentBlock{{Type: "text", Text: system}},
		MaxTokens: maxTokens,
		Effort:    summarizeEffort,
		Messages: []providers.Message{{
			Role:    "user",
			Content: []providers.ContentBlock{{Type: "text", Text: lead + "\n\n" + convo.String()}},
		}},
	}
	out, err := runSummarizeCall(ctx, provider, req)
	if err == nil || ctx.Err() != nil {
		return out, err
	}
	// ⚠️ THE EFFORT HINT IS THE MOST LIKELY THING TO HAVE BROKEN THIS CALL, and
	// it is the one part of the request the caller never asked for. The OpenAI
	// driver passes Effort straight through as reasoning_effort, which a
	// non-reasoning model rejects, and Ollama refuses `think` on a model that
	// cannot reason — so a hint added for the thinking case can break the
	// providers that never needed it.
	//
	// Rather than guess per model which providers tolerate it, drop it and try
	// once more. Strictly better than the behaviour it replaces: before this,
	// any error here was simply a decline, so a second plain attempt can only
	// turn a decline into a summary.
	plain := req
	plain.Effort = ""
	if retried, rerr := runSummarizeCall(ctx, provider, plain); rerr == nil {
		return retried, nil
	}
	return out, err
}

// runSummarizeCall is one round-trip, collecting only the assistant's TEXT.
// Thinking is deliberately not accumulated — a reasoning trace is not a summary,
// and folding one into the transcript would put the model's scratch work where
// its conclusions belong.
func runSummarizeCall(ctx context.Context, provider providers.Provider, req providers.Request) (string, error) {
	ch, err := provider.Call(ctx, req)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			out.WriteString(ev.Text)
		case providers.EventError:
			return "", errors.New(ev.Error)
		}
	}
	return out.String(), nil
}

// applyCompactSummary replaces `messages` with a compacted form built from a
// PRE-COMPUTED summary (the manual path — the server already summarized) + keepN/
// keepFirst taken verbatim from the control, and emits the persisted marker. The
// loop trusts keepN (the server snapped it on the same in-memory==transcript
// history). Used by drainSteer + parkForInput.
//
// INVARIANT — it takes no RunOptions, and must not start to (RFC BL P3). That is
// what makes memory banking unreachable from here, which matters because this
// function runs on paths where the span has ALREADY been banked or must not be:
// the manual compact banks server-side before pushing the control, and a resumed
// run replays a compaction that happened once. Give this access to
// BankCompactedSpan and both become double-banks that no error surfaces —
// duplicate candidates for the dedup band to absorb, on every resume. Banking
// belongs in maybeAutoCompact, which is the only path that discards a span nobody
// has seen yet. loop.Summarize is RunOptions-free for the same reason: it also
// serves History `recap`, which summarizes without discarding anything.
func applyCompactSummary(messages []providers.Message, summary string, keepN int, keepFirst bool, emit func(providers.Event)) []providers.Message {
	before := estimateMessageTokens(messages)
	if keepN < 0 {
		keepN = 0
	}
	if keepN > len(messages) {
		keepN = len(messages)
	}
	tail := messages[len(messages)-keepN:]
	pinned := ""
	if keepFirst && len(messages) > 0 {
		pinned = messageText(messages[0])
	}
	out := CompactionMessages(pinned, summary, tail)
	if emit != nil {
		emit(providers.Event{Type: providers.EventContextCompaction,
			ContextCompaction: &providers.ContextCompactionEventInfo{
				Summary: summary, KeepN: keepN, KeepFirst: keepFirst,
				BeforeTokens: before, AfterTokens: estimateMessageTokens(out)}})
	}
	return out
}

// shouldAutoCompact reports whether the loop should auto-compact at this
// boundary: compaction enabled, the provider reports a window, the previous
// turn's context footprint crossed the trigger percentage, and we didn't just
// compact (one-iteration debounce against thrash). used/window are the previous
// iteration's live values (0 on the first iteration → never fires).
// containsStr is a local membership check; the exhaustion report dedups tier
// messages so two tiers declining identically read as one condition.
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// reportExhausted emits the "nothing reclaimed the window" report, at most once
// per run per 10-point band.
//
// BANDED rather than once-per-run, because the condition worsens: a run stuck at
// 82% and the same run at 95% are different news, and an operator who saw the
// first deserves the second. Banding is the compromise between that and one
// report per iteration, which would bury itself.
func reportExhausted(emit func(providers.Event), seen map[int]bool, c *config.Compaction,
	used, window int, verdicts []providers.ContextTierVerdict) {
	pct := 0
	if window > 0 {
		pct = used * 100 / window
	}
	band := pct / 10
	if seen[band] {
		return
	}
	seen[band] = true

	msg := fmt.Sprintf("context not reclaimed: %d%% of the window (%d/%d tokens) is in use "+
		"and distillation did not shrink it", pct, used, window)
	switch {
	case len(verdicts) == 0:
		// No tier even ran. Almost always a disabled or unreachable distiller,
		// which is a configuration answer rather than a runtime one.
		msg += " — no distillation path ran; check that the mode's distiller is enabled"
	default:
		// ⚠️ EVERY tier's explanation, not just the last one. With the second
		// tier in place a run can decline twice for DIFFERENT reasons — recap
		// on context.keep_last_n, compaction on compaction.keep_last_n — and an
		// operator handed only the final message fixes half the problem and
		// watches the window keep filling.
		//
		// Carried verbatim rather than paraphrased: each already names its own
		// fix, and a summary of them here would be a second, worse copy.
		var seenMsg []string
		for _, v := range verdicts {
			if v.Message != "" && !containsStr(seenMsg, v.Message) {
				seenMsg = append(seenMsg, v.Message)
			}
		}
		msg += " — " + strings.Join(seenMsg, " | ")
	}
	emit(providers.Event{Type: providers.EventContextExhausted,
		ContextExhausted: &providers.ContextExhaustedInfo{
			UsedTokens: used, WindowTokens: window, UsedPct: pct,
			Verdicts: verdicts, Message: msg,
		}})
}

// backstopAvailable reports whether compaction may run as the LAST RESORT for a
// mode whose own distiller did not reclaim the window.
//
// ⚠️ THIS DELIBERATELY IGNORES THE `enabled` DEFAULT, and the distinction is
// between a FEATURE and a SAFETY NET. `compaction.enabled` is off by default so
// that existing agents are byte-identical — it governs whether the loop
// auto-compacts at the operator's chosen threshold. The backstop is not that:
// it fires at the point where the run is otherwise going to hit the provider's
// limit and stop, and "do nothing" is not a defensible answer there.
//
// An EXPLICIT `enabled: false` is still honoured. That is an operator saying
// they do not want this mechanism, and overriding a stated choice silently
// would be worse than the overflow — but the run then reports exhaustion, so
// the consequence of the choice is visible rather than inferred.
func backstopAvailable(c *config.Compaction) bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// backstopPct is the footprint percentage at which the window is considered to
// be in trouble — compaction's own threshold, read WITHOUT the enabled/debounce
// gating shouldAutoCompact applies.
//
// It is separate because exhaustion is a different question from "should we
// compact now". A run whose compaction block is disabled entirely still needs
// to be told its window is not being reclaimed; answering "no distillation was
// due" to an operator staring at 95% is the silence this line exists to remove.
func backstopPct(c *config.Compaction) int {
	if c != nil && c.AutoCompactAtPct != nil {
		return *c.AutoCompactAtPct
	}
	return config.CompactionDefaultAutoAtPct
}

// aboveBackstop reports whether the footprint has reached the point where
// something should have reclaimed the window.
func aboveBackstop(c *config.Compaction, used, window int) bool {
	if window <= 0 || used <= 0 {
		return false // an unknown ceiling cannot be exceeded
	}
	return used*100 >= window*backstopPct(c)
}

func shouldAutoCompact(c *config.Compaction, used, window, iter, lastCompactIter int) bool {
	if c == nil || c.Enabled == nil || !*c.Enabled {
		return false
	}
	if window <= 0 || used <= 0 || iter <= lastCompactIter+1 {
		return false
	}
	at := config.CompactionDefaultAutoAtPct
	if c.AutoCompactAtPct != nil {
		at = *c.AutoCompactAtPct
	}
	return used*100 >= window*at
}

// drainSteer non-blocking-pulls every currently-queued operator steering
// message. An ordinary message (Kind == "") is appended as a SEPARATE user-role
// turn (TRUSTED — bearer-gated, same trust tier as the run caller — so plain
// text, not fenced); onSteer (if set) fires per message so the runner persists +
// emits it. A steer.KindCompact control instead REPLACES the conversation with
// the compacted form (server-computed summary + keep-N/keep-first) and emits
// EventContextCompaction; it does NOT fire onSteer (the summary is not an
// operator turn and must not be persisted as a user_input replay row). Returns
// the (possibly compacted) messages + whether a compaction was applied, so the
// caller can refresh the context footprint (lastCtxTokens) after a shrink.
func drainSteer(q <-chan steer.Message, messages []providers.Message, onSteer func(steer.Message), emit func(providers.Event)) ([]providers.Message, bool) {
	compacted := false
	for {
		select {
		case m := <-q:
			if m.Kind == steer.KindCompact {
				messages = applyCompactSummary(messages, m.Text, m.KeepN, m.KeepFirst, emit)
				compacted = true
				continue
			}
			messages = append(messages, providers.Message{
				Role:    "user",
				Content: []providers.ContentBlock{{Type: "text", Text: m.Text}},
			})
			if onSteer != nil {
				onSteer(m)
			}
		default:
			return messages, compacted
		}
	}
}

// maybeAutoCompact runs an INLINE summarization (the loop computes the summary
// itself — the auto + self-compact path) and replaces `messages` with the
// compacted form, emitting the marker. Returns the (possibly unchanged) slice +
// whether it compacted. A failed summary call changes nothing (logged via emit).
// The caller gates WHEN this runs (threshold / self-request) at a clean boundary.
// declineSplitMessage names the two numbers that ARE the split diagnosis, and
// the key to change. "distillation declined" sends an operator to read source;
// "keep_last_n 6 pins all 7 messages" sends them to the right line of yaml.
func declineSplitMessage(mode string, messages, keepLastN int) string {
	// ⚠️ ALWAYS QUALIFIED. There are two settings with this short name — recap
	// reads context.keep_last_n (default 6), compaction reads
	// compaction.keep_last_n (default 4) — and a message naming the bare key
	// sends half its readers to edit the one that was not the problem.
	key := "compaction.keep_last_n"
	if mode == "recap" {
		key = "context.keep_last_n"
	}
	return fmt.Sprintf("context %s declined: %s %d pins all %d message(s), leaving nothing to distil — lower %s",
		mode, key, keepLastN, messages, key)
}

// compactionSummaryDecline distinguishes the two summarizer outcomes that both
// end in no text. They are NOT the same problem: a failure has an error to
// read, while an empty return is a budget/routing issue with no error at all —
// the one that produced the silent climb this work exists to fix.
func compactionSummaryDecline(mode, trigger string, used, window int, err error) *providers.ContextDistillDeclinedInfo {
	info := &providers.ContextDistillDeclinedInfo{
		Mode: mode, Trigger: trigger, UsedTokens: used, WindowTokens: window,
		Reason: providers.DistillDeclineEmptySummary,
		Message: "context " + mode + " declined: the summarizer returned no text. A thinking model " +
			"spends a small budget reasoning and emits nothing the summary accumulator collects — " +
			"raise recap_max_chars, or choose an effort that stops the model thinking",
	}
	if err != nil {
		info.Reason = providers.DistillDeclineSummarizeFailed
		info.Message = "context " + mode + " declined: the summarize call failed: " + err.Error()
	}
	return info
}

// declineDistill is the SINGLE exit for "distillation was attempted and did
// nothing". Every `return messages, false` in maybeRecap and maybeAutoCompact
// goes through it, so a bare one added later reads as an asymmetry in review.
//
// The silence WAS the bug. A live chat crossed its threshold and climbed to the
// top of the window with zero recap markers and zero errors, and telling "never
// attempted" from "attempted and the summarizer returned empty" required
// reading this file and counting event types in a raw transcript. Routing the
// returns through one helper is what makes the invariant checkable rather than
// a convention.
func declineDistill(emit func(providers.Event), messages []providers.Message,
	info *providers.ContextDistillDeclinedInfo) ([]providers.Message, bool) {
	// Severity is derived HERE rather than at each call site, for the same
	// reason the exit itself is centralised: a site that forgets it would emit
	// a decline with no severity, which the dedup key and the consumer both
	// read as a distinct (and meaningless) third value.
	if info.Severity == "" {
		info.Severity = declineSeverity(info.Reason)
	}
	emit(providers.Event{Type: providers.EventContextDistillDeclined, ContextDistill: info})
	return messages, false
}

// declineSeverity answers one question: can this path still reclaim the window?
//
// split_declined cannot — keep_last_n pins the whole conversation, so the path
// will decline identically every time and the window keeps filling. The others
// leave the mechanism healthy: reasoning_keep is the operator's instruction,
// not_smaller is a correct refusal, and the two summariser failures are about
// one call rather than the configuration.
func declineSeverity(reason string) string {
	if reason == providers.DistillDeclineSplitDeclined {
		return providers.DistillSeverityWarning
	}
	return providers.DistillSeverityInfo
}

func maybeAutoCompact(ctx context.Context, opts RunOptions, messages []providers.Message, used, window int, emit func(providers.Event), trigger string) ([]providers.Message, bool) {
	c := opts.Compaction
	keepLastN := config.CompactionDefaultKeepLastN
	keepFirst := config.CompactionDefaultKeepFirst
	targetPct := config.CompactionDefaultTargetPct
	model := opts.Model
	if c != nil {
		if c.KeepLastN != nil {
			keepLastN = *c.KeepLastN
		}
		if c.KeepFirst != nil {
			keepFirst = *c.KeepFirst
		}
		if c.TargetPercentage != nil {
			targetPct = *c.TargetPercentage
		}
		if c.Model != nil && *c.Model != "" {
			model = *c.Model
		}
	}
	// The window overrides a pinning keep_last_n — see splitOrCutToWindow.
	firstIdx, cut, ok := splitOrCutToWindow(messages, keepLastN, keepFirst,
		window*compactionKeptTailBudgetPct/100)
	if !ok {
		return declineDistill(emit, messages, &providers.ContextDistillDeclinedInfo{
			Mode: "compaction", Trigger: trigger, Reason: providers.DistillDeclineSplitDeclined,
			UsedTokens: used, WindowTokens: window, Messages: len(messages), KeepLastN: keepLastN,
			Message: declineSplitMessage("compaction", len(messages), keepLastN)})
	}
	// Safety cap: when the provider reports a window, never let the kept-
	// verbatim tail itself approach it — otherwise the post-compaction request
	// still overflows (the slow-local-model failure: a tail snapped to a huge
	// tool-result was STILL over the 131k window after compaction). Drop the
	// oldest kept turns into the summarized span until the tail fits.
	if window > 0 {
		cut = capKeptTailToWindow(messages, cut, window*compactionKeptTailBudgetPct/100)
	}
	summary, err := Summarize(ctx, opts.Provider, model, messages[firstIdx:cut], targetPct)
	if err != nil || strings.TrimSpace(summary) == "" {
		// The EventError stays: terminal-error consumers depend on it. The
		// decline is an additional STRUCTURED twin, not a replacement.
		if err != nil {
			emit(providers.Event{Type: providers.EventError, Error: "compaction summary failed (" + trigger + "): " + err.Error()})
		}
		return declineDistill(emit, messages, compactionSummaryDecline("compaction", trigger, used, window, err))
	}
	before := estimateMessageTokens(messages)
	pinned := ""
	if firstIdx > 0 {
		pinned = messageText(messages[0])
	}
	out := CompactionMessages(pinned, strings.TrimSpace(summary), messages[cut:])
	after := estimateMessageTokens(out)
	// Refuse a distillation that is not smaller — see the twin in maybeRecap.
	// Placed BEFORE the bank and the harvest below, which is why those two need
	// no reordering here: a refused compaction must not hand away a span it is
	// about to keep.
	if after >= before {
		return declineDistill(emit, messages, &providers.ContextDistillDeclinedInfo{
			Mode: "compaction", Trigger: trigger, Reason: providers.DistillDeclineNotSmaller,
			UsedTokens: used, WindowTokens: window, Messages: len(messages), KeepLastN: keepLastN,
			BeforeTokens: before, AfterTokens: after,
			Message: fmt.Sprintf("context compaction declined: the result is not smaller (%d -> %d tokens), "+
				"so it was refused rather than applied", before, after)})
	}
	info := &providers.ContextCompactionEventInfo{
		Summary: strings.TrimSpace(summary), KeepN: len(messages) - cut, KeepFirst: firstIdx > 0,
		BeforeTokens: before, AfterTokens: after, Trigger: trigger}
	// RFC BL P3: bank the span we are about to drop, if the agent asked for it.
	// AFTER the summary succeeded and BEFORE the messages are replaced, because
	// this is the last moment the discarded turns exist. Never fatal — see
	// RunOptions.BankCompactedSpan.
	info.MemoryBanked = bankDiscardedSpan(ctx, opts, messages[firstIdx:cut])
	// Recall harvest: embed the span this compaction is dropping into the run-
	// scoped index so a later Recall(query) can fetch it back. Same "last moment
	// the discarded turns exist" rationale as banking; nil-safe when recall is off.
	opts.RecallIndex.Harvest(ctx, messages[firstIdx:cut])
	emit(providers.Event{Type: providers.EventContextCompaction, ContextCompaction: info})
	lcotel.RecordCompactionCtx(ctx, trigger, before, after) // per-run-shape metric via OTEL span event
	return out, true
}

// bankDiscardedSpan hands the span a compaction is dropping to the banking
// callback, and reports the outcome for the transcript marker. Returns nil when
// the agent did not opt in, so the marker field stays absent by default.
//
// Every failure path returns an Error rather than propagating: the caller has
// already produced a valid summary, and refusing to complete the compaction
// because a queue write failed would trade a live run for a memory nicety.
func bankDiscardedSpan(ctx context.Context, opts RunOptions, dropped []providers.Message) *providers.MemoryBankedInfo {
	if opts.BankCompactedSpan == nil || len(dropped) == 0 {
		return nil
	}
	id, err := opts.BankCompactedSpan(ctx, dropped)
	if err != nil {
		return &providers.MemoryBankedInfo{Error: err.Error()}
	}
	if id == "" {
		// Nothing conversational in the span (all tool traffic) — a legitimate
		// nothing-to-bank, reported so it is not mistaken for the flag being off.
		return &providers.MemoryBankedInfo{Error: "no conversational content in the discarded span"}
	}
	return &providers.MemoryBankedInfo{PendingID: id, Messages: len(dropped)}
}

// harvestToMemory banks an evicted span to the consolidation queue at a recap or
// stateful distillation boundary when the agent set context.harvest_to_memory
// (RFC CT P2) — extending the L0-only compaction banking to those modes so the
// consolidator harvests their durable facts too. The compaction path already banks
// via bankDiscardedSpan (its marker), so this covers only the other two modes.
//
// Never fatal, like all banking: a genuine failure (no store, misconfigured scope,
// no user_id) surfaces as a non-fatal EventError so a misconfiguration is visible
// rather than silently dropping every span; a span with nothing conversational to
// bank (id == "") is a benign no-op and stays silent.
func harvestToMemory(ctx context.Context, opts RunOptions, emit func(providers.Event), evicted []providers.Message) {
	if !config.HarvestToMemoryEnabled(opts.Context) || opts.BankCompactedSpan == nil || len(evicted) == 0 {
		return
	}
	if _, err := opts.BankCompactedSpan(ctx, evicted); err != nil {
		emit(providers.Event{Type: providers.EventError, Error: "memory harvest: " + err.Error()})
	}
}

// --- RFC CR L1: reasoning-recap distillation ---
//
// Recap mode is a sibling of compaction on the retention spectrum. It reuses the
// same boundary-snapping (CompactionSplit) and window safety cap so the kept tail
// never orphans a tool_use/tool_result pair, but differs in two ways: the middle
// span becomes a bounded RUNNING recap (folded incrementally into the prior recap
// — O(1) per step, not a fresh proportional summary each time), and the recent
// tool_use/tool_result pairs are kept VERBATIM rather than summarized. The store
// side is untouched — the full transcript is always retained.

// contextRecapMode reports whether the resolved context policy selects L1 recap.
func contextRecapMode(cx *config.Context) bool {
	return cx != nil && cx.Mode != nil && *cx.Mode == config.ContextModeRecap
}

// RecapMessages builds the replacement conversation for a recap distillation:
//
//	[user(pinnedTask)?] ++ [assistant("[Progress recap]…" + recap)?] ++ keptTail
//
// The task is a user turn ON ITS OWN (never fused with the recap), so it stays a
// fixed size across distillations and the fed prompt stays FLAT — the O(T) property
// the benchmark proved. The recap is a separate ASSISTANT turn (the agent's own
// progress note): on the NEXT distillation it falls INTO the evicted span and is
// folded forward by the recap call, so the recap accumulates correctly with no
// separate running-state to thread or restore on resume. keptTail is snapped to a
// clean user-turn boundary (CompactionSplit) so nothing orphans a tool cycle; when
// there is something to distil it is non-empty and the sequence ends on a real turn.
func RecapMessages(pinnedTask, recap string, keptTail []providers.Message) []providers.Message {
	var out []providers.Message
	if strings.TrimSpace(pinnedTask) != "" {
		// The task is emitted RAW (no wrapping header). It is its own turn, so it
		// reads as the original task without one — and, crucially, re-pinning it
		// next cycle via messageText(messages[0]) then yields the SAME text, so the
		// pinned turn stays a fixed size. A header would nest on every distillation.
		out = append(out, providers.Message{Role: "user",
			Content: []providers.ContentBlock{{Type: "text", Text: pinnedTask}}})
	}
	if strings.TrimSpace(recap) != "" {
		out = append(out, providers.Message{Role: "assistant",
			Content: []providers.ContentBlock{{Type: "text",
				Text: "[Progress recap — what I have done and concluded so far; established context for everything before the recent steps below:]\n\n" + recap}}})
	}
	return append(out, keptTail...)
}

// recapReasoningPrompt builds the system prompt for a RUNNING progress recap
// bounded to maxChars. Unlike compactionPrompt (a proportional N% summary) it asks
// for a fixed-size running note. The newly-evicted span it is fed already CONTAINS
// the prior recap (as the assistant turn RecapMessages emitted last time), so the
// call is incremental — it folds the small prior note plus the new steps into an
// updated note, never re-reading the whole history. That, plus the flat pinned
// task, is what keeps recap-mode cost O(T).
func recapReasoningPrompt(maxChars int) string {
	if maxChars <= 0 {
		maxChars = config.ContextDefaultRecapMaxChars
	}
	return fmt.Sprintf("You maintain a running RECAP of an agent's progress on a task, carried forward as its working context. "+
		"The input may include an earlier '[Progress recap …]' note plus the NEW steps taken since. Produce an UPDATED recap, at most %d characters, "+
		"that folds the new steps into the old: preserve the goal, decisions, established facts and values, tool results that still "+
		"matter, and the current state; drop verbose reasoning and superseded attempts. Write durable prose the agent can rely on to "+
		"continue. Output ONLY the recap — no preamble.", maxChars)
}

// RecapReasoning makes ONE provider call to fold the newly-evicted span (which
// already contains the prior recap turn) into an updated recap bounded near
// maxChars. Reuses summarizeWith (NO tools offered → can't re-enter the loop).
//
// It is fed the whole evicted span (reasoning + tool_use/tool_result), not only
// the assistant's prose: on a procedural task the progress IS in the tool results
// (the counter values, the fetched facts), so recapping prose alone would lose
// exactly what the next step needs. The recap OUTPUT is what replaces the verbose
// reasoning — that is the "reasoning distillation".
func RecapReasoning(ctx context.Context, provider providers.Provider, model string, evicted []providers.Message, maxChars int) (string, error) {
	maxTok := 0
	if maxChars > 0 {
		maxTok = maxChars/4 + 64 // chars→tokens, generous so a compliant model finishes its sentence
	}
	return summarizeWith(ctx, provider, model, evicted, recapReasoningPrompt(maxChars), "Progress to recap:", maxTok)
}

// shouldAutoRecap mirrors shouldAutoCompact for recap mode: the mode is recap,
// the provider reports a window, the previous turn's footprint crossed the
// trigger percentage, and we didn't just distil (one-iteration debounce). Recap
// mode is self-enabling — choosing the mode turns auto-recap on (no separate
// Enabled flag, unlike compaction).
func shouldAutoRecap(cx *config.Context, used, window, iter, lastIter int) bool {
	if !contextRecapMode(cx) {
		return false
	}
	if window <= 0 || used <= 0 || iter <= lastIter+1 {
		return false
	}
	at := config.ContextDefaultAutoRecapAtPct
	if cx.AutoRecapAtPct != nil {
		at = *cx.AutoRecapAtPct
	}
	return used*100 >= window*at
}

// maybeRecap runs an INLINE recap distillation (RFC CR L1): fold the evicted span
// into a fresh running recap, keep the last-N tail verbatim, and replace
// `messages`. Returns the (possibly unchanged) slice and whether it distilled. A
// failed recap call changes nothing (logged via emit). The first user turn (the
// task) is always pinned — the preamble the paper under-counts. No running-state
// is threaded: the prior recap lives in the evicted span (see RecapMessages) and
// is folded forward by the recap call, so replay/resume rebuild identically.
func maybeRecap(ctx context.Context, opts RunOptions, messages []providers.Message, used, window int, emit func(providers.Event), trigger string) ([]providers.Message, bool) {
	cx := opts.Context
	keepLastN := config.ContextDefaultKeepLastN
	reasoning := config.ContextDefaultReasoning
	maxChars := config.ContextDefaultRecapMaxChars
	keepFirst := config.CompactionDefaultKeepFirst // pin the task verbatim, like compaction
	model := opts.Model
	if cx != nil {
		if cx.KeepLastN != nil {
			keepLastN = *cx.KeepLastN
		}
		if cx.Reasoning != nil {
			reasoning = *cx.Reasoning
		}
		if cx.RecapMaxChars != nil {
			maxChars = *cx.RecapMaxChars
		}
		// A cheap, ideally NON-THINKING summarizer beside the run's own model.
		// Same provider — this is a model swap, not a routing decision.
		if cx.Model != nil && *cx.Model != "" {
			model = *cx.Model
		}
	}
	if reasoning == "keep" {
		// Not a fault — but reported, because "nothing happened" must never be
		// silent. An operator who did not realise `keep` disables distillation
		// needs to see that once, not deduce it.
		return declineDistill(emit, messages, &providers.ContextDistillDeclinedInfo{
			Mode: "recap", Trigger: trigger, Reason: providers.DistillDeclineReasoningKeep,
			UsedTokens: used, WindowTokens: window, Messages: len(messages),
			Message: "context recap declined: context.reasoning is \"keep\", which asks for no " +
				"distillation — set reasoning: recap (or drop) to let the window be reclaimed"})
	}
	// The window overrides a pinning keep_last_n — see splitOrCutToWindow.
	firstIdx, cut, ok := splitOrCutToWindow(messages, keepLastN, keepFirst,
		window*compactionKeptTailBudgetPct/100)
	if !ok {
		return declineDistill(emit, messages, &providers.ContextDistillDeclinedInfo{
			Mode: "recap", Trigger: trigger, Reason: providers.DistillDeclineSplitDeclined,
			UsedTokens: used, WindowTokens: window, Messages: len(messages), KeepLastN: keepLastN,
			Message: declineSplitMessage("recap", len(messages), keepLastN)})
	}
	if window > 0 {
		cut = capKeptTailToWindow(messages, cut, window*compactionKeptTailBudgetPct/100)
	}
	newRecap := ""
	if reasoning == "recap" {
		r, err := RecapReasoning(ctx, opts.Provider, model, messages[firstIdx:cut], maxChars)
		if err != nil || strings.TrimSpace(r) == "" {
			// The EventError stays for terminal-error consumers; the decline is
			// the structured twin. The err==nil branch is the one that produced
			// the observed silent climb — it had no signal of any kind.
			if err != nil {
				emit(providers.Event{Type: providers.EventError, Error: "context recap failed (" + trigger + "): " + err.Error()})
			}
			return declineDistill(emit, messages, compactionSummaryDecline("recap", trigger, used, window, err))
		}
		newRecap = strings.TrimSpace(r)
	}
	// reasoning=="drop": no recap call; the evicted span is dropped with no note.
	before := estimateMessageTokens(messages)
	pinned := ""
	if firstIdx > 0 {
		pinned = messageText(messages[0])
	}
	out := RecapMessages(pinned, newRecap, messages[cut:])
	after := estimateMessageTokens(out)
	// Refuse a distillation that is not smaller. On the observed session a
	// manual compaction went 14230 -> 14334 and was applied anyway: both
	// numbers were already measured here and never compared.
	//
	// The predicate is `after >= before` EXACTLY, not a margin — a 10% rule
	// would have refused the first good compaction of that same session (36%),
	// and at 99% of a window a 5% win is still a win.
	if after >= before {
		return declineDistill(emit, messages, &providers.ContextDistillDeclinedInfo{
			Mode: "recap", Trigger: trigger, Reason: providers.DistillDeclineNotSmaller,
			UsedTokens: used, WindowTokens: window, Messages: len(messages), KeepLastN: keepLastN,
			BeforeTokens: before, AfterTokens: after,
			Message: fmt.Sprintf("context recap declined: the result is not smaller (%d -> %d tokens), "+
				"so it was refused rather than applied", before, after)})
	}
	// ⚠️ THE HARVESTS BELONG BELOW THE CHECK, and used to sit above it. They
	// hand the evicted span to Recall and to persistent memory — which is only
	// correct once we know the span is actually being dropped. Above the check,
	// every decline banked a span it then kept, and with the gate re-firing each
	// iteration that wrote the same content repeatedly.
	//
	// Recall harvest: embed the evicted span before it is dropped, so a later
	// Recall(query) can fetch it back — most valuable exactly here, where recap
	// (or drop) loses the detail. nil-safe when recall is off.
	opts.RecallIndex.Harvest(ctx, messages[firstIdx:cut])
	// Persistent-memory harvest (RFC CT P2): bank the same evicted span for the
	// consolidator when the agent opted in. No-op unless context.harvest_to_memory.
	harvestToMemory(ctx, opts, emit, messages[firstIdx:cut])
	emit(providers.Event{Type: providers.EventContextRecap,
		ContextRecap: &providers.ContextRecapEventInfo{
			Recap: newRecap, KeepN: len(messages) - cut, KeepFirst: firstIdx > 0,
			BeforeTokens: before, AfterTokens: after, Trigger: trigger, Reasoning: reasoning}})
	lcotel.RecordCompactionCtx(ctx, "recap:"+trigger, before, after)
	return out, true
}

func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	// An interactive run is operator-driven and Cancel-bounded: each operator
	// turn (and each end_turn park awaiting input) consumes a loop iteration,
	// so the default 16-iteration runaway guard silently ends a live terminal
	// session after 16 turns (reported as "max_iterations"). That guard exists
	// to stop a RUNAWAY AUTONOMOUS agent — it has no purpose for a human-driven
	// terminal the operator can Cancel. So when no explicit cap is set, an
	// interactive run is unbounded (the 1<<20 hard ceiling + run cancellation
	// still bound it), and the operator no longer has to ALSO set
	// unbounded_iterations to get an always-on terminal. An EXPLICIT
	// max_iterations is still honored; an autonomous run keeps the 16 default.
	interactiveUnbounded := opts.Interactive && opts.SteerQueue != nil && opts.MaxIterations == 0

	if opts.MaxIterations == 0 {
		opts.MaxIterations = DefaultMaxIterations
	}
	if opts.ToolParallelism <= 0 {
		opts.ToolParallelism = 8
	}
	if opts.Provider == nil {
		return RunResult{}, fmt.Errorf("loop: provider is nil")
	}

	// A synthetic provider whose loop turns are internal tool-dispatch steps of
	// one run (code-js — Capabilities().UnboundedIterations) is exempt from the
	// MaxIterations soft-cap: capping a code-agent's sequential tool calls at 16
	// is unusable, and the provider bounds the whole run by its own wall-clock
	// timeout (LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS, enforced as a run-level
	// deadline). A high hard ceiling stays as a pure runaway backstop. For every
	// LLM driver MaxIterations is unchanged (the runaway-tool-use guard).
	// code-js providers are unbounded by capability; an LLM agent opts in
	// per-def via UnboundedIterations (interactive/terminal runs). Either way
	// the 1<<20 hard ceiling stays as a pure runaway backstop.
	unboundedIters := opts.Provider.Capabilities().UnboundedIterations || opts.UnboundedIterations || interactiveUnbounded
	iterCap := opts.MaxIterations
	if unboundedIters {
		iterCap = maxIterationsHardCeiling
	}

	// Stamp the run's agent identity onto ctx for providers that need it
	// outside the LLM-shaped Request. Only the synthetic code-js provider
	// (RFC J) reads it — to resolve agent_code/<name>/index.js (Request
	// carries no agent name), to populate the JS run({metadata}) arg, and to
	// derive its per-run deterministic-replay seed (RunID) + clock anchor
	// (StartedAt). The canonical LLM drivers ignore it. Stamped here, the
	// single choke point that calls Provider.Call, so every run-creation site
	// (HTTP / gRPC / MCP / scheduler) inherits it without per-site wiring.
	// StartedAt is stamped once per Run() and stays stable across the run's
	// turns, so code-js's anchored Date.now() is consistent across replays.
	runIdent := tools.RunIdentity(ctx)
	ctx = providers.WithRunMeta(ctx, providers.RunMeta{
		AgentName:         opts.AgentName,
		UserID:            runIdent.UserID,
		RunID:             runIdent.AgentID,
		StartedAt:         time.Now(),
		CodeBody:          opts.CodeBody,
		Metadata:          opts.Metadata,
		PayloadMetadata:   opts.PayloadMetadata,
		RunTimeoutSeconds: opts.RunTimeoutSeconds,
	})

	// Log once per Run if the agent declared an effort hint but the
	// resolved provider is SupportsEffort=false. Operators see a
	// clear "effort dropped on ollama/qwen3:14b" line rather than
	// silently believing the agent thought hard. Once-per-Run is
	// sufficient because the (provider, model) is the same across
	// every iteration of a Run; spamming on each iteration would
	// just be noise.
	if opts.Effort != "" && !opts.Provider.Capabilities().SupportsEffort {
		log.Printf("loop: effort=%q dropped — provider %q does not translate effort to a wire param (model=%q)",
			opts.Effort, opts.Provider.ID(), opts.Model)
	}

	system, freshMessages := splitSegments(opts.Segments)
	// Prepend any prior conversation history (continuation endpoint).
	// PriorMessages is empty for fresh runs.
	messages := make([]providers.Message, 0, len(opts.PriorMessages)+len(freshMessages))
	messages = append(messages, opts.PriorMessages...)
	messages = append(messages, freshMessages...)

	var toolSpecs []providers.ToolSpec
	if opts.Dispatcher != nil {
		toolSpecs = opts.Dispatcher.Specs(opts.Tools)
	}

	emit := func(ev providers.Event) {
		if opts.OnEvent != nil {
			opts.OnEvent(ev)
		}
	}

	// Vision gate (RFC AT). If this run carries an image content block — fresh
	// or replayed from a prior turn — validate it and refuse before the first
	// call when the resolved provider can't accept image input (e.g. an agent
	// whose tier resolved to DeepSeek's text endpoint). This fails loudly here
	// instead of the image being silently dropped. Checked once at the resolved
	// provider AND inside tryProviderFallback against the fallback target — a
	// mid-run swap to a non-vision provider is refused with a clear
	// EventFallbackSuppressed event (RFC AT §4.4), never a silent leak to a
	// raw 400. PriorMessages were validated when first sent, so only the fresh
	// segments need re-validating here.
	if messagesHaveImage(messages) {
		if err := validateImageSegments(opts.Segments); err != nil {
			emit(providers.Event{Type: providers.EventError, Error: err.Error()})
			return RunResult{}, err
		}
		if !opts.Provider.Capabilities().SupportsVision {
			msg := fmt.Sprintf("model %q on provider %q does not support image input", opts.Model, opts.Provider.ID())
			emit(providers.Event{Type: providers.EventError, Error: msg})
			return RunResult{}, errors.New(msg)
		}
	}

	emit(providers.Event{Type: providers.EventStarted})

	// RFC CR tier-routing: `context.mode: auto` resolves to a concrete mode from
	// the RESOLVED provider — a local backend → recap (schema-free, safe for a
	// weaker model), a frontier API → stateful. An interactive run takes recap
	// (see resolveAutoContextMode for why that outlived its reason). Resolved once
	// here on a CLONE (never mutating the shared agent def); a mid-run provider
	// fallback keeps the mode chosen at start.
	if contextAutoMode(opts.Context) {
		opts.Context = resolveAutoContextMode(opts.Context, opts.Provider.Capabilities().Local, opts.Interactive)
	}

	// Recall-augmented distillation: stamp the run-scoped index on ctx (before the
	// stateful branch, so both loop paths and their dispatched tools reach it via
	// recall.FromContext). No-op when opts.RecallIndex is nil (recall off).
	ctx = recall.NewContext(ctx, opts.RecallIndex)

	// Clamp the operator-supplied retry count to the safety cap so
	// a misconfigured yaml can't induce minute-scale delays per error.
	// Above the stateful branch: that loop retries on the same budget.
	if opts.MaxSameProviderRetries > maxSameProviderRetriesCap {
		opts.MaxSameProviderRetries = maxSameProviderRetriesCap
	}

	// Run-lifetime heartbeat: pulse OnHeartbeat every parkHeartbeatInterval for
	// as long as this run's goroutine is alive, IN ADDITION to the per-iteration
	// pulse below. The stale-run sweeper reaps a run whose heartbeat hasn't
	// advanced in HeartbeatStaleAfter (default 10m) as CRASHED — but a SINGLE
	// iteration can legitimately block far longer than the per-iteration cadence:
	// a large-context prefill on a slow local model, a long tool, or
	// same-provider retry backoff. A slow ollama review hit exactly this — two
	// 300s header timeouts inside one iteration (>10m with no pulse) got the
	// live run reaped as heartbeat_timeout. A live goroutine is not a crashed
	// process, so keep the heartbeat fresh regardless of which phase the
	// iteration is in. The callback is a fire-and-forget DB write (server's
	// makeHeartbeat) — safe to call concurrently with the per-iteration pulse.
	// Stops when Run returns (close) or ctx is cancelled. parkForInput keeps its
	// own pulse (this subsumes it; harmless overlap).
	//
	// ⚠️ ABOVE THE STATEFUL BRANCH, deliberately. It used to sit below it, so a
	// stateful run pulsed only while parked: a working one kept a NULL
	// last_heartbeat_at and the sweeper failed it as heartbeat_timeout ten minutes
	// in, with its goroutine still running and spending tokens.
	//
	// The goroutine takes its OWN copy of ctx: Run reassigns ctx below this
	// point (the compact-request stamp, per-iteration values), and a closure
	// over the variable reads it while that write happens — a data race that
	// did not exist while this block sat below the last reassignment.
	if opts.OnHeartbeat != nil {
		hbDone := make(chan struct{})
		defer close(hbDone)
		hbCtx := ctx
		go func() {
			t := time.NewTicker(parkHeartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-hbDone:
					return
				case <-hbCtx.Done():
					return
				case <-t.C:
					opts.OnHeartbeat()
				}
			}
		}()
	}

	// RFC CR L2: a stateful run is a different loop — it feeds only (P, Σ, O) and
	// the model emits a patch + action each step. Branch here, after the preamble
	// P (`system`) and the action-tool catalog are resolved, into the self-
	// contained stateful loop. The append/recap machinery below does not apply.
	if contextStatefulMode(opts.Context) {
		return runStateful(ctx, opts, system, messages, toolSpecs, iterCap, emit)
	}

	// Context compaction (v2): a self-request flag the Context op=compact tool
	// sets (checked at the next iteration boundary), plus the previous iteration's
	// context footprint + window ceiling that drive the auto-compact threshold.
	// lastCompactIter debounces back-to-back auto-compactions.
	var compactRequested atomic.Bool
	ctx = tools.WithCompactRequest(ctx, &compactRequested)
	// ⚠️ SEEDED, not zero. These used to start at 0 and stay there until the
	// first provider call RETURNED, while the distillation gate runs at the TOP
	// of an iteration — so a run whose very first request was already near the
	// window could not distil before sending it.
	//
	// That is not a corner case: a continuation answering at end_turn is ONE
	// iteration, so an interactive chat replaying a large history had no
	// opportunity to distil at all, however full its prompt. The observed
	// session's last run sent 30100 tokens of a 32768 window on a single
	// iteration and reclaimed nothing.
	//
	// The estimate is chars/4 — not a new estimator, the same one the gate
	// already runs on after a steer-delivered compaction, after a recap and
	// after a compaction. Its error direction is the safe one: chars/4
	// OVERCOUNTS dense text, so the seed errs toward distilling slightly early,
	// and the first real usage event overwrites it with the provider's own
	// count.
	//
	// Post-call distillation was the alternative and is the wrong shape: for a
	// continuation it would distil for a NEXT Run() whose counters start at
	// zero again, which is indistinguishable from doing nothing.
	// Computed once: the system prompt and tool catalogue do not change within
	// a run, and they are most of the request on a small window.
	preambleTokens := estimatePreambleTokens(system, toolSpecs)
	lastCtxTokens := estimatePromptTokens(preambleTokens, messages)
	lastWindow := effectiveWindow(0, opts)
	// ⚠️ THE SEED IS A GUESS, AND A GUESS MUST NOT BE REPORTED AS A FINDING.
	// Until a turn returns, lastCtxTokens is chars/4 over content no tokenizer
	// has seen and lastWindow is the static capability a driver may still
	// revise (Ollama reads the model's actually-loaded window from /api/ps
	// only after a call). The seed exists so a one-iteration continuation can
	// still DISTIL before it sends an oversized prompt; arming the run's
	// operator-facing REPORTS from it was collateral.
	//
	// It matters because the seed counts the preamble, and a preamble is not a
	// short history: it is fixed and irreducible, so on an agent whose system
	// prompt and tool catalogue are most of a small window it sits above the
	// threshold on iteration ZERO, before the conversation exists. The run then
	// told its operator "keep_last_n 6 pins all 1 message(s)" at severity
	// warning, and "88% of the window is in use and distillation did not shrink
	// it" — three alarms, on every run, about a request that had not been sent.
	//
	// So: the gate still OPENS on the seed (that protection is the point of
	// seeding it), and distillation still runs. Only the reporting waits for a
	// number the provider actually returned.
	footprintMeasured := false
	lastCompactIter := -2
	// Distillation declines dedup on (mode, reason) for the life of the run,
	// mirroring the server's seenLimit set.
	//
	// The PAIR is the key, deliberately. A reason that is a property of the
	// CONFIGURATION — keep_last_n spanning the conversation, reasoning:keep —
	// is equally true on every iteration, and repeating it would bury it in its
	// own noise. A state-dependent one (not_smaller) can legitimately recur as
	// the history changes, and a DIFFERENT reason always gets through.
	//
	// This complements the debounce above rather than duplicating it: the
	// debounce suppresses re-attempts within its window, this suppresses a
	// repeat of the same verdict after that window expires.
	seenDecline := map[string]bool{}
	// The most recent decline, surfaced on Context op=self.
	//
	// This is why the value is held rather than read off the event stream: the
	// EVENT fires once per (mode, reason), but the CONDITION persists, and an
	// agent reading op=self on turn 20 needs it as much as on turn 3. A
	// consumer that only saw the event would have to remember it itself.
	var lastDistill tools.LastDistillValue
	// iterVerdicts collects the declines of ONE gate opening. The exhaustion
	// report needs what was tried and refused — "the window is full" and "the
	// mechanism ran and refused, here is why" call for opposite next moves.
	var iterVerdicts []providers.ContextTierVerdict
	// seenExhausted keeps the exhaustion report to once per run per severity
	// band, for the same reason declines dedup: a condition that is true every
	// iteration would otherwise bury itself.
	seenExhausted := map[int]bool{}
	distillEmit := func(ev providers.Event) {
		if ev.Type == providers.EventContextDistillDeclined && ev.ContextDistill != nil {
			if !footprintMeasured {
				// Nothing has been measured yet — see footprintMeasured. The
				// decline is real (the distillation genuinely did not run), but
				// the CONDITION it describes is an estimate of a request the
				// provider has never seen, and the next iteration has real
				// numbers to report from. Dropped before the verdict list and
				// before lastDistill, so op=self does not carry it either.
				return
			}
			// Keep this iteration's verdict for the exhaustion report. Cleared
			// at the top of each gate opening, so the report carries what was
			// tried THIS time rather than an accumulation across the run.
			iterVerdicts = append(iterVerdicts, providers.ContextTierVerdict{
				Mode: ev.ContextDistill.Mode, Reason: ev.ContextDistill.Reason,
				Message: ev.ContextDistill.Message,
			})
			// Recorded here because this is the one path EVERY decline takes,
			// so what op=self reports cannot diverge from what was emitted.
			// (Before or after the dedup is equivalent today — the first
			// occurrence always passes through — so this is about having a
			// single origin, not about ordering.)
			lastDistill = tools.LastDistillValue{
				Mode: ev.ContextDistill.Mode, Reason: ev.ContextDistill.Reason,
				Message: ev.ContextDistill.Message,
			}
			// Severity is part of the key so an ESCALATION is reported again.
			// The same reason at info and at warning is two different messages
			// to an operator, and suppressing the second because the first was
			// quieter is how a condition gets buried by its own earlier self.
			key := ev.ContextDistill.Mode + "|" + ev.ContextDistill.Reason + "|" + ev.ContextDistill.Severity
			if seenDecline[key] {
				return
			}
			seenDecline[key] = true
		}
		emit(ev)
	}
	// L1 recap (RFC CR) reuses lastCompactIter to debounce — the two distillation
	// modes are mutually exclusive per run. No running-recap state is threaded: the
	// prior recap rides the transcript (see RecapMessages), so replay/resume are
	// byte-identical without restoring loop state.
	recapMode := contextRecapMode(opts.Context)

	var totalUsage providers.Usage
	var finalText string
	var stopReason string
	// fallbackAttempts counts cumulative v0.8.2 provider switches per
	// run. tryProviderFallback bumps it on each successful switch;
	// the FallbackPolicy.MaxAttempts cap (default 3) is enforced
	// there. A fallback "consumes" one iter slot from the outer loop
	// — we don't decrement iter, because a deliberate switch should
	// still count toward MaxIterations as work the run did.
	var fallbackAttempts int
	// firstTurnSucceeded flips true after the first iteration
	// completes successfully (an assistant message has been
	// appended to `messages`). Used by tryProviderFallback to
	// honor RunOptions.FallbackPolicy.PinAfterSuccess — once the
	// transcript has provider-specific state, fallback to a
	// different provider risks cross-family translation bugs.
	var firstTurnSucceeded bool

	// sameProviderRetries counts CONSECUTIVE retryable failures
	// against the current (provider, model). Reset to 0 on any
	// successful Call() that yields events (whether the iteration
	// ultimately failed in-stream or not — we measure conn-level
	// recovery, not iteration-level success). Also reset by
	// tryProviderFallback when it switches the provider, since the
	// retry budget is per-provider. Capped at opts.MaxSameProviderRetries
	// (clamped to maxSameProviderRetriesCap=5).
	var sameProviderRetries int

	// RFC BH turn-scoped cancel: turnCancelFn cancels the CURRENT turn's ctx and
	// disarmTurn removes its armed registry token. Both are re-created per turn
	// (below, right before the model call) and released here on Run exit — any
	// path, including a panic unwinding through a caller's recover — so a
	// turn-cancel can never fire against a finished run. Initialised to no-ops so
	// the first turn's setup + a non-turn-cancellable run (ArmTurnCancel nil) are
	// clean.
	turnCancelFn := context.CancelCauseFunc(func(error) {})
	disarmTurn := func() {}
	defer func() {
		disarmTurn()
		turnCancelFn(nil)
	}()

	// A resumed run that was parked when it paused waits HERE, before the first
	// model call, rather than being handed a conversation with nothing to answer.
	// When the operator's turn arrives the loop proceeds normally with it
	// appended; when it never does (ctx cancelled), the run ends the way any
	// parked run that is cancelled ends — on the end_turn it had already
	// reached before the pause.
	parkAbandoned := false
	if opts.StartParked && opts.Interactive && opts.SteerQueue != nil {
		var resumedWithInput bool
		messages, lastCtxTokens, resumedWithInput = parkForOperatorTurn(ctx, &opts, messages, 0, lastCtxTokens, preambleTokens, emit)
		if !resumedWithInput {
			// Cancelled while waiting. The run ends on the end_turn it had
			// already reached before the pause; skipping the loop entirely lets
			// the shared terminal block below emit it, so there is one
			// EventDone site rather than two that can drift.
			stopReason = "end_turn"
			parkAbandoned = true
		}
	}

outerLoop:
	for iter := 0; !parkAbandoned && iter < iterCap; iter++ {
		// v0.10.0 OTEL: one loomcycle.iteration span per turn. Nested
		// under the caller-opened loomcycle.run span (api/http opens
		// the run span at each of the 4 run-creation sites). The
		// iteration body has multiple exit paths (fallback continue,
		// unrecoverable return, terminal break, normal fall-through),
		// so End() is called explicitly at each — Go's `defer` runs
		// at function-scope only, not loop-iteration-scope.
		iterCtx, iterSpan := lcotel.RecordIteration(ctx, iter)

		// RFC BH: release the PREVIOUS turn's cancel ctx + registry token at the top
		// of each iteration — before any early-return path below (pause-gate,
		// context-transform) — so nothing leaks and a cancel racing the inter-turn
		// boundary finds no armed token (409). This turn re-arms lower, right before
		// the model call. No-ops on the first iteration and after a park (already
		// disarmed there).
		disarmTurn()
		turnCancelFn(nil)

		// Stamp the CURRENTLY-resolved provider/model onto the per-iteration
		// ctx so the Context tool's op=self can report them to the agent
		// (non-secret introspection). Per-iteration, not once at Run start,
		// because tryProviderFallback swaps opts.Provider/opts.Model in place
		// — this keeps op=self truthful about what the agent is actually
		// running on after a fallback. Tools dispatched this iteration receive
		// iterCtx, so the value reaches them without per-tool wiring.
		iterCtx = tools.WithResolvedProvider(iterCtx, opts.Provider.ID())
		iterCtx = tools.WithResolvedModel(iterCtx, opts.Model)
		iterCtx = tools.WithResolvedSampling(iterCtx, opts.Sampling)
		iterCtx = tools.WithMaxContextTokens(iterCtx, opts.MaxContextTokens) // RFC CJ — configured cap for op=self
		// NB: the context-footprint stamp (tools.WithContextUsage) is applied
		// LOWER — after drainSteer + auto/self compaction — so a same-turn
		// op=self never reports a stale pre-compaction footprint.

		// Heartbeat fires at the top of each iteration. Cheap path —
		// implementations are expected to be ~one UPDATE. Failures
		// should NOT propagate (a sweeper in the future is the
		// authoritative path; a missed heartbeat just means we have
		// to wait for the sweeper to catch up).
		if opts.OnHeartbeat != nil {
			opts.OnHeartbeat()
		}

		// Cooperative pause/quiesce (RFC X / F41): if a runtime pause is in
		// effect, park HERE — a clean iteration boundary, never mid-turn /
		// between a tool_use and its tool_results. Park persists
		// pause_state='paused', blocks until resume, then restores 'running'.
		// On ctx cancel while parked it returns an error and we exit the run.
		if opts.PauseGate != nil && opts.PauseGate.PauseRequested() {
			if err := opts.PauseGate.Park(iterCtx); err != nil {
				iterSpan.End()
				return RunResult{StopReason: "cancelled", Usage: totalUsage}, ctx.Err()
			}
		}

		// Mid-turn steering (internal/steer): drain any operator-injected
		// instructions and append each as a user turn BEFORE this iteration's
		// provider call. Drained ONLY here, at the top of the iteration —
		// never between a tool_use assistant turn and its tool_results user
		// turn (which would orphan the tool_use and 400 the provider). After
		// a tool round the messages end [...assistant(tool_use),
		// user(tool_results)]; appending a user(steer) turn yields consecutive
		// user turns, which every provider accepts (replay already emits them).
		if opts.SteerQueue != nil {
			var steerCompacted bool
			messages, steerCompacted = drainSteer(opts.SteerQueue, messages, opts.OnSteer, emit)
			if steerCompacted {
				// A steer-delivered compaction shrank the running history; refresh
				// the footprint so the auto-compact check + op=self below reflect
				// the compacted size, not the stale pre-compaction value.
				lastCtxTokens = estimatePromptTokens(preambleTokens, messages)
			}
		}

		// Auto / self-requested context distillation — also a clean boundary
		// (drainSteer ran; no tool cycle is mid-flight). Fires when the agent asked
		// (Context op=compact) OR the PREVIOUS iteration's footprint crossed the
		// configured threshold. In recap mode (RFC CR L1) the loop recaps inline;
		// otherwise it compacts (L0). The two are mutually exclusive per run —
		// choosing recap bypasses the compaction auto-trigger. The smaller next
		// request self-debounces. Applies to ALL runs (interactive + autonomous).
		selfReq := compactRequested.Swap(false)
		// The gate opens on EITHER threshold. A mode whose primary sits above
		// the backstop — or whose primary cannot fire at all — must still reach
		// compaction, or the window fills with nothing consulted.
		var primaryDue bool
		if recapMode {
			primaryDue = shouldAutoRecap(opts.Context, lastCtxTokens, lastWindow, iter, lastCompactIter)
		} else {
			primaryDue = shouldAutoCompact(opts.Compaction, lastCtxTokens, lastWindow, iter, lastCompactIter)
		}
		backstopDue := iter > lastCompactIter+1 &&
			backstopAvailable(opts.Compaction) &&
			aboveBackstop(opts.Compaction, lastCtxTokens, lastWindow)
		distill := selfReq || primaryDue || backstopDue
		if distill {
			trigger := "auto"
			if selfReq {
				trigger = "self"
			}
			// The debounce advances on every ATTEMPT, not only on success.
			// It used to sit inside the `if did` blocks below, so a decline
			// left it untouched, the gate re-fired on the very next iteration,
			// and a summarize call was burned every iteration for the rest of
			// the run — against a condition that could not change.
			lastCompactIter = iter
			iterVerdicts = nil
			reclaimed := false
			if recapMode {
				if newMsgs, did := maybeRecap(iterCtx, opts, messages, lastCtxTokens, lastWindow, distillEmit, trigger); did {
					messages = newMsgs
					lastCtxTokens = estimatePromptTokens(preambleTokens, messages)
					reclaimed = true
				}
			}
			// ⚠️ THE SECOND TIER. Compaction is reachable from EVERY mode, not
			// only append — which is the whole point: a recap that declines used
			// to leave nothing else to try, and the run climbed to the limit.
			//
			// It is the right last resort precisely because it fails
			// DIFFERENTLY: empty_summary is a property of the recap budget and
			// the recap prompt, and a compaction summary runs on neither. A
			// second tier that failed for the same reasons would be theatre.
			//
			// Reached when the mode's own distiller did not reclaim AND the
			// footprint is at the backstop. In append mode compaction IS the
			// primary, so this is its only invocation there.
			if !reclaimed && (backstopDue || selfReq || !recapMode) &&
				backstopAvailable(opts.Compaction) {
				if newMsgs, did := maybeAutoCompact(iterCtx, opts, messages, lastCtxTokens, lastWindow, distillEmit, trigger); did {
					messages = newMsgs
					// Compaction shrank the history; refresh the footprint so
					// op=self below reflects the compacted size, not the
					// pre-compaction value (the next turn's usage overwrites it).
					lastCtxTokens = estimatePromptTokens(preambleTokens, messages)
					reclaimed = true
				}
			}
			// Nothing reclaimed the window, and the footprint is at the point
			// where something should have. Say so — this is the run heading for
			// the provider's limit, and it is the one condition an operator must
			// never have to infer from an absence of events.
			if !reclaimed && footprintMeasured &&
				aboveBackstop(opts.Compaction, lastCtxTokens, lastWindow) {
				reportExhausted(distillEmit, seenExhausted, opts.Compaction,
					lastCtxTokens, lastWindow, iterVerdicts)
			}
		}

		// Context footprint (input+cache of the last completed turn, or the
		// post-compaction estimate when a compaction just ran above) so Context
		// op=self can show the agent how full its window is — the signal it needs
		// to decide whether to self-compact (op=compact). Stamped HERE, below
		// drainSteer + auto/self compaction, so a same-turn op=self never reports
		// the stale pre-compaction footprint. 0 on the first iteration.
		iterCtx = tools.WithContextUsage(iterCtx, lastCtxTokens, lastWindow)
		// Stamped HERE, beside the footprint, so the two cannot describe
		// different iterations. The footprint alone is a trap: an agent told it
		// is at 99% calls op=compact, and if distillation is declining
		// structurally that call reaches the same decline and teaches nothing.
		iterCtx = tools.WithLastDistill(iterCtx, lastDistill)

		// Context-transform plugins (RFC Z / F43): run the configured chain on a
		// COPY of the outbound context — the loop's canonical system/messages
		// (and so the persisted transcript + code-js replay input) stay
		// untouched. Skipped for the synthetic code-js provider (local replay,
		// no external leak; redacting its bytes would trip replay divergence).
		// No caching in this version — the chain re-runs over the copy each turn.
		reqSystem, reqMessages := system, messages
		if len(opts.ContextPlugins) > 0 && opts.Provider.ID() != codeJSProviderID {
			cs, cm, perr := contextplugin.Apply(iterCtx, opts.ContextPlugins, system, messages)
			if perr != nil {
				emit(providers.Event{Type: providers.EventError, Error: "context transform: " + perr.Error()})
				return RunResult{Iterations: iter}, perr
			}
			reqSystem, reqMessages = cs, cm
		}

		// RFC BH: arm the per-turn cancel token covering THIS turn's model call +
		// tool dispatch (the previous turn was released at the top of the loop).
		// turnCtx is a child of the RUN ctx, so a whole-run cancel STILL cascades to
		// the turn; a turn-cancel fires only turnCtx (cause ErrTurnCancelled, run ctx
		// alive) and is caught below. When ArmTurnCancel is nil the token is never
		// fired, so turnCtx behaves exactly like iterCtx — the loop stays
		// byte-identical.
		var turnCtx context.Context
		turnCtx, turnCancelFn = context.WithCancelCause(iterCtx)
		disarmTurn = func() {}
		if opts.ArmTurnCancel != nil {
			disarmTurn = opts.ArmTurnCancel(turnCancelFn)
		}

		req := providers.Request{
			Model:            opts.Model,
			System:           reqSystem,
			Messages:         reqMessages,
			Tools:            toolSpecs,
			MaxTokens:        opts.MaxTokens,        // 0 → driver default
			MaxContextTokens: opts.MaxContextTokens, // 0 → driver/provider default (RFC CJ)
			Effort:           opts.Effort,           // "" → driver default; PR 3 wires per-driver translation
			// OnEvent lets the driver fire pre-channel events (currently
			// EventRetry during a 429 sleep) directly to the same caller
			// hook the loop uses for response events. Without this hop
			// the retry would be invisible to SSE consumers.
			OnEvent: emit,
		}
		// Map the resolved per-agent sampling params onto the flat Request
		// fields (each driver applies the subset its provider supports).
		if s := opts.Sampling; s != nil {
			req.Temperature = s.Temperature
			req.TopP = s.TopP
			req.TopK = s.TopK
			req.FrequencyPenalty = s.FrequencyPenalty
			req.PresencePenalty = s.PresencePenalty
			req.Seed = s.Seed
			req.Stop = s.Stop
		}
		ch, err := opts.Provider.Call(turnCtx, req)
		if err != nil {
			// v0.12.9 same-provider retry: when the operator opted
			// into MaxSameProviderRetries > 0, retryable errors
			// (429, 5xx, network) sleep with exponential backoff and
			// re-attempt the SAME (provider, model) before
			// MarkRateLimited cools the matrix entry and
			// tryProviderFallback escalates. Captures the common
			// case of a brief provider-side burst: real-world 429s
			// often clear within 1-3 seconds, well under the 30s
			// MarkRateLimited cooldown.
			if ctx.Err() == nil &&
				sameProviderRetries < opts.MaxSameProviderRetries &&
				providers.ClassifyError(err) == providers.ErrorClassRetryable {
				sameProviderRetries++
				backoff := sameProviderRetryBackoff(sameProviderRetries)
				emit(providers.Event{
					Type: providers.EventRetry,
					Retry: &providers.RetryInfo{
						Provider: opts.Provider.ID(),
						Attempt:  sameProviderRetries,
						WaitMs:   backoff.Milliseconds(),
						Reason:   providers.RetryReasonSchedule,
					},
				})
				select {
				case <-ctx.Done():
					lcotel.SetSpanError(iterSpan, ctx.Err())
					iterSpan.End()
					turnCancelFn(nil) // RFC BH: release this turn's cancel ctx before terminating (no leak)
					return RunResult{Iterations: iter}, ctx.Err()
				case <-time.After(backoff):
				}
				lcotel.SetSpanErrorMessage(iterSpan, "provider call failed; same-provider retry scheduled")
				iterSpan.End()
				continue outerLoop
			}
			// Resolver feedback: a non-context error from Call() is
			// the driver giving up after its own retries. Route the
			// feedback by error class:
			//
			//   429 (rate limit) → MarkRateLimited (self-recovering
			//     30s cooldown; transient, not "model broken")
			//   5xx / model 404 / network → MarkStalled (15-min
			//     probe-gated recovery; "model is broken for a
			//     while, stop wasting calls")
			//
			// The split matters: pre-v0.12.7 MarkStalled treated 429
			// the same as 5xx, which under the x1000 load test
			// (2026-05-26) cascaded ~120 rate-limited runs into 800+
			// "no provider available" 503s for the rest of the probe
			// interval. PR #235 patched this with a skip-on-429
			// guard; this site is the structural fix — positive call
			// to the right method.
			//
			// ctx errors are user-side cancellation, not provider
			// faults — don't pollute the matrix on either path. RFC AX: an
			// operator-key refusal is a PER-RUN policy decision (this restricted
			// run may not use the operator key), not a model outage — marking the
			// (provider, model) stalled would poison the shared availability
			// matrix and wrongly exclude the model from OTHER (non-restricted)
			// runs for the probe interval. Skip matrix feedback for it.
			if ctx.Err() == nil && !errors.Is(err, providers.ErrOperatorKeyForbidden) {
				if providers.IsRateLimit(err) {
					if opts.MarkRateLimited != nil {
						opts.MarkRateLimited(opts.Provider.ID(), opts.Model, 0)
					}
				} else if opts.MarkStalled != nil {
					opts.MarkStalled(opts.Provider.ID(), opts.Model, err.Error())
				}
			}
			// v0.8.2: when the run carries a fallback policy and the
			// error class is retryable, swap to the next provider
			// and re-run this iteration. tryProviderFallback emits
			// EventProviderFallback (+ EventCacheInvalidated when
			// switching away from anthropic) and mutates opts in
			// place. Outcome==Switched → continue the outer loop
			// without surfacing the error. Other outcomes fall
			// through to the original error path. The fallback path
			// switches (provider, model), so the per-pair retry
			// budget resets — the new pair starts fresh.
			if tryProviderFallback(ctx, &opts, &fallbackAttempts, err, emit, messages, firstTurnSucceeded) == fallbackOutcomeSwitched {
				sameProviderRetries = 0
				lcotel.SetSpanErrorMessage(iterSpan, "provider call failed; fallback engaged")
				iterSpan.End()
				continue outerLoop
			}
			emit(providers.Event{Type: providers.EventError, Error: err.Error()})
			lcotel.SetSpanError(iterSpan, err)
			iterSpan.End()
			turnCancelFn(nil) // RFC BH: release this turn's cancel ctx before terminating (no leak)
			return RunResult{Iterations: iter}, err
		}
		// Call() succeeded — the connection / driver was healthy
		// enough to open the response stream. Reset the same-provider
		// retry counter so the next retryable error (if any) starts
		// from a fresh budget. In-stream errors below have their own
		// retry path.
		sameProviderRetries = 0

		// Collect this iteration: assistant text, any tool_use blocks, usage.
		var assistantBlocks []providers.ContentBlock
		var pendingTools []providers.ToolUse
		var iterText string
		var iterStop string
		var iterUsage *providers.Usage
		// iterReasoning holds the accumulated reasoning_content from
		// thinking-mode models (DeepSeek V4 Pro / deepseek-reasoner).
		// Stamped onto the assistant Message we append below so the
		// next iteration's request body echoes it back to the API
		// per DeepSeek's roundtrip contract. Empty for non-thinking
		// providers.
		var iterReasoning string
		// iterReasoningSignature holds the Anthropic extended-thinking block's
		// signature (paired with iterReasoning); stamped onto the assistant
		// Message so the Anthropic driver replays the thinking block, seal
		// included, on the tool-use continuation (else Anthropic 400s). Empty
		// for non-Anthropic / non-thinking turns.
		var iterReasoningSignature string

		for ev := range ch {
			switch ev.Type {
			case providers.EventText:
				iterText += ev.Text
				emit(ev)
			case providers.EventThinking:
				// Forward the live reasoning trace to the consumer (SSE / gRPC /
				// adapters) so a UI can render "thinking…" as the model streams
				// it. Deliberately NOT accumulated into the assistant message
				// content and NOT echoed into the next request — the full trace
				// is carried separately on EventDone.Reasoning (see
				// providers.EventThinking). Before this case existed the switch
				// had no branch for EventThinking (and no default), so every
				// provider's streamed thinking was silently dropped here and
				// never reached any client — the driver emitted it, the loop ate
				// it. Regression: TestRun_ForwardsEventThinking.
				emit(ev)
			case providers.EventToolCall:
				// Some providers (Ollama) don't issue tool_call IDs. Anthropic
				// and OpenAI both 400 if we replay an empty-ID tool_use in the
				// next turn's history, so we synthesise one here. The synth ID
				// is deterministic per (run, iter, slot) so a replay produces
				// the same value.
				tu := *ev.ToolUse
				if tu.ID == "" {
					tu.ID = fmt.Sprintf("lc-%d-%d", iter, len(pendingTools))
				}
				pendingTools = append(pendingTools, tu)
				assistantBlocks = append(assistantBlocks, providers.ContentBlock{
					Type:      "tool_use",
					ToolUseID: tu.ID,
					ToolName:  tu.Name,
					ToolInput: tu.Input,
				})
				emit(providers.Event{Type: providers.EventToolCall, ToolUse: &tu})
			case providers.EventDone:
				iterStop = ev.StopReason
				iterUsage = ev.Usage
				iterReasoning = ev.Reasoning
				iterReasoningSignature = ev.ReasoningSignature
			case providers.EventError:
				// RFC BH: an operator turn-cancel aborted this turn's stream (turnCtx
				// cancelled, RUN ctx still alive) — not a provider fault. Skip the
				// retry / fallback / matrix-feedback machinery below (which would
				// MarkStalled the model + could swap providers), drain the channel so
				// the sender goroutine doesn't leak, and let the post-stream
				// turn-cancel handler park the run.
				if ctx.Err() == nil && errors.Is(context.Cause(turnCtx), ErrTurnCancelled) {
					for range ch {
					}
					break
				}
				// v0.8.2 classification: build the RAW error string so
				// the status-prefix regex sees the bare "<name>
				// <code>:" shape. The streamErr wrap below would
				// obscure that; reserved for the FINAL return value.
				rawErr := errors.New(ev.Error)

				// v0.12.9 same-provider retry on in-stream retryable
				// errors. Drain the rest of the channel (drivers may
				// emit a trailing EventDone after EventError; we must
				// consume it or the goroutine sending events leaks) and
				// re-attempt the same (provider, model). Same budget
				// as the Call() error path.
				if ctx.Err() == nil &&
					sameProviderRetries < opts.MaxSameProviderRetries &&
					providers.ClassifyError(rawErr) == providers.ErrorClassRetryable {
					for range ch {
					}
					sameProviderRetries++
					backoff := sameProviderRetryBackoff(sameProviderRetries)
					emit(providers.Event{
						Type: providers.EventRetry,
						Retry: &providers.RetryInfo{
							Provider: opts.Provider.ID(),
							Attempt:  sameProviderRetries,
							WaitMs:   backoff.Milliseconds(),
							Reason:   providers.RetryReasonSchedule,
						},
					})
					select {
					case <-ctx.Done():
						lcotel.SetSpanError(iterSpan, ctx.Err())
						iterSpan.End()
						turnCancelFn(nil) // RFC BH: release this turn's cancel ctx before terminating (no leak)
						return RunResult{Iterations: iter}, ctx.Err()
					case <-time.After(backoff):
					}
					lcotel.SetSpanErrorMessage(iterSpan, "in-stream provider error; same-provider retry scheduled")
					iterSpan.End()
					continue outerLoop
				}

				// Resolver feedback for in-stream errors: same
				// 429-vs-5xx routing as the Call() error path
				// above. Driver opened the SSE/NDJSON stream
				// successfully but then surfaced a provider-side
				// error mid-stream — route to MarkRateLimited or
				// MarkStalled based on whether it's a 429.
				streamErr := errors.New(ev.Error)
				if ctx.Err() == nil {
					if providers.IsRateLimit(streamErr) {
						if opts.MarkRateLimited != nil {
							opts.MarkRateLimited(opts.Provider.ID(), opts.Model, 0)
						}
					} else if opts.MarkStalled != nil {
						opts.MarkStalled(opts.Provider.ID(), opts.Model, ev.Error)
					}
				}
				if tryProviderFallback(ctx, &opts, &fallbackAttempts, rawErr, emit, messages, firstTurnSucceeded) == fallbackOutcomeSwitched {
					for range ch {
					}
					sameProviderRetries = 0
					lcotel.SetSpanErrorMessage(iterSpan, "in-stream provider error; fallback engaged")
					iterSpan.End()
					continue outerLoop
				}
				emit(ev)
				// Drain any trailing events (a driver may emit an EventDone
				// after EventError) so the sending goroutine doesn't block on
				// the channel until ctx-cancel / the idle timeout — mirrors the
				// same-provider-retry + fallback drains above. Without it, the
				// terminal error path was the one branch that abandoned ch.
				for range ch {
				}
				lcotel.SetSpanErrorMessage(iterSpan, ev.Error)
				iterSpan.End()
				turnCancelFn(nil) // RFC BH: release this turn's cancel ctx before terminating (no leak)
				return RunResult{Iterations: iter}, fmt.Errorf("provider error: %s", ev.Error)
			}
		}

		// RFC BH: did the operator cancel THIS turn while the model was generating
		// (run ctx still alive)? Covers both a driver that surfaced EventError on
		// the turnCtx-cancel (short-circuited above) and one that just closed the
		// stream. The run-level cancel is checked FIRST (ctx.Err()==nil): a
		// whole-run cancel terminates as before, never mistaken for a turn-cancel.
		turnCancelledMidGen := opts.ArmTurnCancel != nil && ctx.Err() == nil &&
			errors.Is(context.Cause(turnCtx), ErrTurnCancelled)

		// Prepend any text before tool_use blocks so the assistant turn is well-formed.
		if iterText != "" {
			assistantBlocks = append(
				[]providers.ContentBlock{{Type: "text", Text: iterText}},
				assistantBlocks...,
			)
		}
		messages = append(messages, providers.Message{
			Role:               "assistant",
			Content:            assistantBlocks,
			Reasoning:          iterReasoning,
			ReasoningSignature: iterReasoningSignature,
		})
		// Mark the run "past turn one." Any subsequent retryable
		// error that would have triggered cross-provider fallback
		// is suppressed when PinAfterSuccess is set — the
		// transcript now has provider-specific state.
		firstTurnSucceeded = true

		// Clear any stale per-model stall flag in the resolver
		// matrix: this iteration just succeeded against
		// (provider, model), which is the most direct possible
		// evidence the pair is healthy. Without this, a stall
		// from an earlier transient failure could outlive a
		// proven recovery and collapse a tier's cascade between
		// probes. Idempotent at the resolver layer.
		if opts.ClearStall != nil {
			opts.ClearStall(opts.Provider.ID(), opts.Model)
		}

		if iterUsage != nil {
			totalUsage.InputTokens += iterUsage.InputTokens
			totalUsage.OutputTokens += iterUsage.OutputTokens
			totalUsage.CacheCreationTokens += iterUsage.CacheCreationTokens
			totalUsage.CacheReadTokens += iterUsage.CacheReadTokens
			totalUsage.Model = iterUsage.Model
			// Capture the ACTUAL provider that served this iteration.
			// opts.Provider is mutated in place by tryProviderFallback
			// when a runtime fallback engages, so reading ID() here
			// reflects the post-fallback identity. Used by downstream
			// analysis to quantify primary-vs-fallback routing.
			totalUsage.Provider = opts.Provider.ID()
			// RFC AV: carry the per-call credential source (which key paid,
			// stamped by the driver) onto the run-level summary. The last
			// successful iteration's source becomes runs.credential_source —
			// best-effort for the summary; the exact per-call split is in the
			// token_usage ledger (one row per EventUsage below).
			totalUsage.CredentialSource = iterUsage.CredentialSource
			totalUsage.CredentialScopeID = iterUsage.CredentialScopeID
			// Stamp the serving model's context-window ceiling onto the
			// per-iteration usage event so the UI can render a "context
			// used / max" gauge. Set on iterUsage (discarded after this
			// iteration) NOT totalUsage, so the run-final accounting stays
			// byte-stable; 0 when the provider reports an unknown window.
			// A driver may already report a per-CALL window (e.g. Ollama
			// reads the model's actual loaded context from /api/ps) — prefer
			// that and only fall back to the static capability default.
			// RFC CJ: a per-agent context budget caps the EFFECTIVE window used
			// by the distillation threshold + the gauge. See effectiveWindow —
			// shared with the run-start seed so the two cannot disagree.
			iterUsage.MaxContextTokens = effectiveWindow(iterUsage.MaxContextTokens, opts)
			// RFC AV: stamp the serving provider onto the per-call usage event
			// too (not just totalUsage) so the token_usage row records which
			// provider actually served this call — exact across mid-run fallback.
			iterUsage.Provider = opts.Provider.ID()
			emit(providers.Event{Type: providers.EventUsage, Usage: iterUsage})
			// Retain this turn's CURRENT context footprint (input + cache, i.e.
			// what the request actually sent — NOT cumulative totalUsage, which
			// only grows) + the window ceiling, so the NEXT iteration's
			// top-of-loop auto-compact check sees the live utilization. After a
			// compaction the next request shrinks, so this self-debounces.
			lastCtxTokens = iterUsage.InputTokens + iterUsage.CacheReadTokens + iterUsage.CacheCreationTokens
			lastWindow = iterUsage.MaxContextTokens
			// Set BESIDE the values it qualifies, not derived from a separate
			// flag: the question the reports ask is "did a provider report
			// this footprint", and the only honest answer is at the assignment
			// that made it so.
			footprintMeasured = true
		}

		stopReason = iterStop
		finalText = iterText

		// RFC BH turn-cancel (mid-generation): the operator stopped this turn while
		// the model was streaming. Keep the partial assistant output (appended
		// above; a completed call's usage was billed above too), but leave the
		// history VALID before parking:
		//   - drop a content-less assistant turn (cancel before anything streamed) —
		//     an empty assistant message 400s the next call;
		//   - synthesize a "cancelled by operator" tool_result for every tool_use the
		//     model started but we never dispatched — a dangling tool_use 400s too.
		// Then route into the interactive park (not the terminate/break path).
		if turnCancelledMidGen {
			if n := len(messages); n > 0 && messages[n-1].Role == "assistant" && len(messages[n-1].Content) == 0 {
				messages = messages[:n-1]
			}
			if len(pendingTools) > 0 {
				messages = append(messages, providers.Message{Role: "user", Content: cancelledToolResults(pendingTools)})
			}
			var resumed bool
			messages, lastCtxTokens, resumed = finishTurnCancel(ctx, &opts, messages, iter, lastCtxTokens, preambleTokens, reasonFromCause(context.Cause(turnCtx)), emit, disarmTurn)
			iterSpan.End()
			if !resumed {
				break
			}
			continue outerLoop
		}

		// Terminal: model is done.
		if iterStop != "tool_use" || len(pendingTools) == 0 {
			// Persistent interactive run: park instead of terminating. Wait
			// for the operator's next instruction (or Cancel). The run holds
			// its concurrency slot while idle — the documented fairness
			// trade-off of an always-on terminal agent (bounded by the
			// existing per-user / global run caps).
			if opts.interactiveAtBoundary(ctx) && opts.SteerQueue != nil {
				// RFC BH: the turn ended and the run is about to park — no longer
				// mid-turn, so disarm the turn-cancel token (a cancel while parked
				// finds nothing armed → the handler 409s it).
				disarmTurn()
				var resumedWithInput bool
				messages, lastCtxTokens, resumedWithInput = parkForOperatorTurn(ctx, &opts, messages, iter, lastCtxTokens, preambleTokens, emit)
				iterSpan.End()
				if !resumedWithInput {
					break
				}
				continue outerLoop
			}
			iterSpan.End()
			break
		}

		// Execute pending tools concurrently, bounded by
		// opts.ToolParallelism, and append a single user turn with
		// all results in tool_call order.
		//
		// Two ordering rules cohabit here. The MESSAGE we hand the
		// model on the next turn lists tool_results in the same order
		// the model emitted the tool_calls — Anthropic correlates by
		// tool_use_id but staying stable on the wire avoids subtle
		// surprises in cache reuse and transcript reads. The EVENTS
		// we emit on the SSE stream go out in COMPLETION order — fast
		// tools' results stream out first, slow ones last — because
		// callers rendering live progress want "company 1 done" the
		// moment company 1 is done, not after company 3 finishes too.
		ident := tools.RunIdentity(ctx)
		hookIdent := hooks.Identity{
			Agent:   opts.AgentName,
			UserID:  ident.UserID,
			AgentID: ident.AgentID,
			// RFC AF: the run's authoritative tenant so the registry fires a
			// tenant-scoped hook only on its own tenant's runs (global hooks,
			// Tenant=="", still fire on all).
			Tenant: ident.TenantID,
		}
		toolResults := executePendingTools(turnCtx, opts.Dispatcher, pendingTools, opts.ToolParallelism, opts.Hooks, hookIdent, emit)
		messages = append(messages, providers.Message{Role: "user", Content: toolResults})

		// RFC BH turn-cancel (mid-tool-dispatch): the operator stopped this turn
		// while its tools ran. executePendingTools returns one result per pending
		// tool (a ctx-cancelled tool is reported error-shaped), so the
		// assistant(tool_use)/user(tool_result) pairing above is already VALID — no
		// synthesis needed. The completed model call was billed above; the
		// interrupted tools just carry an error result. Park instead of looping into
		// another model turn. Run-cancel is checked first (ctx.Err()==nil).
		if opts.ArmTurnCancel != nil && ctx.Err() == nil && errors.Is(context.Cause(turnCtx), ErrTurnCancelled) {
			var resumed bool
			messages, lastCtxTokens, resumed = finishTurnCancel(ctx, &opts, messages, iter, lastCtxTokens, preambleTokens, reasonFromCause(context.Cause(turnCtx)), emit, disarmTurn)
			iterSpan.End()
			if !resumed {
				break
			}
			continue outerLoop
		}

		// Normal fall-through to next iteration. Disarm the turn-cancel token at
		// this turn boundary so a cancel racing the inter-turn gap finds nothing
		// armed (409) rather than firing a spent token (RFC BH §9.4); the next turn
		// re-arms. End the iteration span here — the for loop's continue opens a
		// fresh one.
		disarmTurn()
		iterSpan.End()
	}
	// RFC BH: release the last turn's cancel ctx once the loop has exited (break or
	// MaxIterations exhaustion). The run-exit defer would catch it too, but calling
	// it here keeps the context lifecycle explicit + go-vet clean.
	turnCancelFn(nil)

	// If the for loop exited by exhausting MaxIterations while the model was
	// still mid-tool-use, the stop_reason will be stuck at "tool_use" but no
	// tools ran on this final iteration. Surface that distinctly to the
	// caller — they can decide whether to bump MaxIterations and retry, or
	// surface a different error to the user.
	if stopReason == "tool_use" {
		stopReason = "max_iterations"
		// An unbounded-iterations provider (code-js) is exempt from the
		// MaxIterations cap and bounded by its run-level timeout instead, so
		// reaching iterCap here means the runaway hard ceiling — run() kept
		// requesting tool calls without returning AND the timeout never fired.
		// That's a non-terminating tool-call loop (a code-agent bug), not a too-
		// small cap; name it accordingly. (Capability-driven, not ID-coupled.)
		if unboundedIters {
			log.Printf("code agent %q hit the %d-call runaway ceiling without returning — a code-agent is bounded by its run timeout (LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS), not by iteration count; this indicates a tool-call loop that never terminates.", opts.AgentName, iterCap)
		}
	}

	emit(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: &totalUsage})

	return RunResult{
		StopReason: stopReason,
		FinalText:  finalText,
		Iterations: iterationCount(messages),
		Usage:      totalUsage,
	}, nil
}

// executeTool runs one tool through the dispatcher; returns a marker error
// result if no dispatcher is wired up (defensive — Run() should reject earlier).
func executeTool(ctx context.Context, d *tools.Dispatcher, tu providers.ToolUse) tools.Result {
	if d == nil {
		return tools.Result{Text: "no tool dispatcher", IsError: true}
	}
	// Stamp the tool_use id so a tool can tag side events with it (RFC X
	// Phase 3: the Agent tool's parallel_spawn ledger keys on the parent
	// tool_use id). Harmless for every other tool.
	ctx = tools.WithToolUseID(ctx, tu.ID)
	return d.Execute(ctx, tu.Name, tu.Input)
}

// dispatchOneTool wraps executeTool with the registered tool-use hook
// chains (when hookDispatcher is non-nil). Pre-hooks run first; if any
// returns a non-nil deny, executeTool is skipped and the synthetic
// result becomes the tool_result the model sees. Post-hooks then run
// over the (real or synthetic) result; the final post-chain output is
// what reaches the parent emit / message.
//
// hookDispatcher == nil OR no matching hooks registered → fast path
// reduces to plain executeTool. The dispatcher's Match call is
// O(N) over registered hooks; with no hooks registered this is a
// nil-slice return and the function shape is identical to pre-hook
// behaviour.
//
// emit is the same callback used by the loop for SSE/store emissions.
// It's invoked here for the v0.8.17 EventHostWidened audit event,
// fired ONCE per dispatched call that a permitted Pre-hook widened.
// emit may be nil (some tests inject a dispatcher without one) —
// host_widened emission is silently skipped in that case; the
// widening itself still applies.
func dispatchOneTool(
	ctx context.Context,
	dispatcher *tools.Dispatcher,
	tu providers.ToolUse,
	hookDispatcher *hooks.Dispatcher,
	ident hooks.Identity,
	emit func(providers.Event),
) tools.Result {
	if hookDispatcher == nil {
		return executeTool(ctx, dispatcher, tu)
	}
	hookTC := hooks.ToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input}

	pre := hookDispatcher.RunPre(ctx, ident, hookTC)
	var r tools.Result
	if pre.Deny != nil {
		// A Pre-hook short-circuited; do NOT run the real tool. The
		// synthetic result IS the tool_result the model sees. Post
		// chain still runs — operators may want to wrap or audit
		// even denied results.
		r = tools.Result{Text: pre.Deny.Text, IsError: pre.Deny.IsError}
	} else {
		// Run the tool with the (possibly hook-rewritten) input.
		running := tu
		if pre.Input != nil {
			running.Input = pre.Input
		}
		// Emit the v0.8.17 host-widened audit event BEFORE executing
		// the tool, so the SSE stream ordering reads
		// tool_call → host_widened → tool_result. Persisted via
		// makeRecordingEmit so the events table carries an audit row.
		if emit != nil && len(pre.AllowHosts) > 0 {
			emit(providers.Event{
				Type: providers.EventHostWidened,
				HostWidening: &providers.HostWideningEventInfo{
					ToolCallID: tu.ID,
					ToolName:   tu.Name,
					URL:        extractToolURL(running.Input),
					HookOwner:  pre.GrantingHookOwner,
					HookName:   pre.GrantingHookName,
					HostsAdded: pre.AllowHosts,
				},
			})
		}
		// Attach any per-call host-widening grants from permitted
		// Pre-hooks (v0.8.17). WithExtraAllowedHosts is a no-op when
		// pre.AllowHosts is empty, so this is cost-free for the common
		// path. The widened ctx is per-tool-call — sub-agents and the
		// loop's next iteration see the original ctx without the
		// extras (CLAUDE.md confused-deputy guidance: grant scope is
		// one Execute call, no implicit propagation).
		execCtx := tools.WithExtraAllowedHosts(ctx, pre.AllowHosts)
		r = executeTool(execCtx, dispatcher, running)
	}

	post := hookDispatcher.RunPost(ctx, ident, hookTC, hooks.ToolResult{Text: r.Text, IsError: r.IsError})
	// The hook wire carries only text and is_error, so a Post chain can
	// replace those two and nothing else. Rebuilding the result from them
	// alone dropped Error and Count on EVERY call — the server always wires
	// a dispatcher, matching hooks or not — so a classified failure reached
	// the model with no classification. Keep the tool's structured fields;
	// a hook that turns a failure into a success takes the failure's
	// classification with it, since Error is nil on success by contract.
	r.Text, r.IsError = post.Text, post.IsError
	if !r.IsError {
		r.Error = nil
	}
	return r
}

// extractToolURL best-effort pulls a URL string out of common tool
// input shapes (HTTP, WebFetch). Returns "" when no URL field is
// present — the audit event still emits, with an empty URL field,
// rather than failing the dispatch. JSON-parse failures are silent
// for the same reason.
func extractToolURL(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var probe struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &probe); err != nil {
		return ""
	}
	return probe.URL
}

// executePendingTools runs the assistant turn's tool_calls concurrently,
// bounded by `parallelism`, and returns the tool_result content blocks in
// the same order as `pending` (so the next turn's user message preserves
// tool_call ordering). EventToolResult emissions happen in COMPLETION
// order — a fast tool's result reaches the SSE consumer before a slow
// one's, even when the slow one came first in `pending`.
//
// Tools share `ctx`, so a parent cancellation propagates to every
// in-flight goroutine. Tool errors are surfaced as IsError tool_results
// (the existing dispatcher contract); they do NOT abort the batch — the
// other tools still run, and the model gets to see every result.
//
// emit() concurrency: most emits (EventToolResult below) happen from
// THIS goroutine, reading from a results channel — single-writer.
// EXCEPTIONS (v0.8.4 EventChannel* from Channel tool's Execute;
// v0.8.17 EventHostWidened from dispatchOneTool when a permitted
// Pre-hook widens) are emitted from worker goroutines inside
// dispatchOneTool. The production emit (api/http/server.go's
// makeRecordingEmit) is mutex-protected to handle this; test-side
// emit collectors used with ToolParallelism > 1 MUST take the same
// precaution (a plain `func(ev) { events = append(events, ev) }`
// would data-race on the slice).
func executePendingTools(
	ctx context.Context,
	dispatcher *tools.Dispatcher,
	pending []providers.ToolUse,
	parallelism int,
	hookDispatcher *hooks.Dispatcher,
	hookIdent hooks.Identity,
	emit func(providers.Event),
) []providers.ContentBlock {
	if len(pending) == 0 {
		return nil
	}
	if parallelism < 1 {
		parallelism = 1
	}

	type result struct {
		idx int
		tu  providers.ToolUse
		res tools.Result
	}

	resCh := make(chan result, len(pending))
	sem := make(chan struct{}, parallelism)

	var wg sync.WaitGroup
	for i, tu := range pending {
		wg.Add(1)
		go func(i int, tu providers.ToolUse) {
			defer wg.Done()
			// Acquire the slot. ctx-aware so a parent cancellation
			// during a saturated batch unblocks the goroutine instead
			// of having it sit forever on a full sem channel.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				resCh <- result{idx: i, tu: tu, res: tools.Result{
					IsError: true,
					Text:    "tool dispatch cancelled before slot acquired: " + ctx.Err().Error(),
				}}
				return
			}
			r := dispatchOneTool(ctx, dispatcher, tu, hookDispatcher, hookIdent, emit)
			resCh <- result{idx: i, tu: tu, res: r}
		}(i, tu)
	}
	go func() {
		wg.Wait()
		close(resCh)
	}()

	results := make([]providers.ContentBlock, len(pending))
	for r := range resCh {
		// Emit in completion order so the SSE consumer sees each
		// tool's result the moment it's done.
		// Render ONCE, before both uses. The emitted event is what gets
		// persisted, and replayTranscript rebuilds the model's tool_result
		// block from that persisted Text — so prefixing only the block below
		// would make the same tool call read differently before and after a
		// resume, silently dropping the classification on replay.
		text := renderToolResultText(r.res)
		emit(providers.Event{
			Type:    providers.EventToolResult,
			ToolUse: &providers.ToolUse{ID: r.tu.ID, Name: r.tu.Name, Input: r.tu.Input},
			Text:    text,
			IsError: r.res.IsError,
		})
		// Place by index so the message we hand back to the model
		// stays in tool_call order regardless of finish order.
		// ToolName is set for the benefit of providers that
		// correlate tool_use ↔ tool_result by NAME rather than by
		// id — Gemini's functionResponse and Ollama's tool messages
		// both require the name. Anthropic / OpenAI / DeepSeek use
		// the id only and ignore the redundant name field.
		results[r.idx] = providers.ContentBlock{
			Type:      "tool_result",
			ToolUseID: r.tu.ID,
			ToolName:  r.tu.Name,
			Text:      text,
			IsError:   r.res.IsError,
		}
	}
	return results
}

// renderToolResultText puts a classified failure's category, retryability and
// backoff in front of the tool's own output.
//
// WHY IN THE TEXT AT ALL: IsError reaches the model only on Anthropic. That
// driver serializes is_error on the tool_result block; the OpenAI dialect —
// and so DeepSeek, vLLM and llama.cpp — plus Gemini and Ollama have no slot
// for it in their wire formats. A classification that lived only in a field
// would therefore be an Anthropic-only feature, invisible on every other
// provider and expensive to discover later.
//
// WHY UNIFORMLY, INCLUDING ANTHROPIC: one rendering path means no driver can
// drift and one test covers every provider. Suppressing it where the flag
// exists would make an agent's tool_result text provider-dependent, so a
// prompt or an eval tuned on one provider could behave differently on another
// for a reason nobody would think to look for. On Anthropic the flag and the
// text say the same thing, which is redundant but never contradictory.
//
// Unclassified failures are returned untouched, so the overwhelming majority
// of tool results are byte-identical to before.
func renderToolResultText(res tools.Result) string {
	if res.Error == nil || res.Error.Category == "" {
		return res.Text
	}

	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(string(res.Error.Category))
	if res.Error.Retryable {
		b.WriteString(" · retryable")
		// Only when there is a real hint. A missing backoff and a zero one
		// are opposite instructions, so silence beats inventing "0s".
		if d := res.Error.RetryAfter; d != nil && *d > 0 {
			fmt.Fprintf(&b, " · retry in %s", d.Round(time.Second))
		}
	} else {
		b.WriteString(" · not retryable")
	}
	b.WriteString("] ")

	desc := strings.TrimSpace(res.Error.Description)
	body := strings.TrimSpace(res.Text)
	// A description that merely restates the tool's own message costs tokens
	// and carries no extra decision.
	if desc != "" && desc != body {
		b.WriteString(desc)
		if body != "" {
			b.WriteString("\n\n")
		}
	}
	b.WriteString(res.Text)
	return b.String()
}

// splitSegments separates "system" segments (which become provider System
// blocks) from "user" segments (which become the first user Message).
func splitSegments(segs []PromptSegment) (system []providers.ContentBlock, messages []providers.Message) {
	var firstUser []providers.ContentBlock
	for _, s := range segs {
		switch s.Role {
		case "system":
			for _, c := range s.Content {
				system = append(system, flattenContent(c))
			}
		case "user":
			for _, c := range s.Content {
				firstUser = append(firstUser, flattenContent(c))
			}
		}
	}
	if len(firstUser) > 0 {
		messages = append(messages, providers.Message{Role: "user", Content: firstUser})
	}
	return
}

// allowedUntrustedKinds is the set of `kind` values an untrusted-block may
// declare. Anything else is normalised to "untrusted" so a caller can't
// inject a tag that the model treats as a trusted boundary (e.g. "system").
var allowedUntrustedKinds = map[string]bool{
	"untrusted":     true,
	"web_content":   true,
	"uploaded_cv":   true,
	"qa_question":   true,
	"user_input":    true,
	"tool_output":   true,
	"search_result": true,
	"run_metadata":  true, // non-secret metadata projected from an external trigger body
}

// validImageMediaTypes is the whitelist of media types an "image" content
// block may declare (RFC AT). It is the common denominator across all four
// vision providers (Anthropic's set); anything else is rejected before the
// call rather than letting a provider 400 on an unsupported type.
var validImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// validateImageSegments validates every "image" block in the inbound segments
// (RFC AT §4.3): an image is valid only in a user-role segment, must carry a
// whitelisted media_type, and its Data must decode as base64. It returns a
// descriptive error — surfaced to the caller as an EventError before any
// provider call — so a malformed image fails fast and clearly instead of
// reaching a provider as an opaque 400. flattenContent itself has no error
// channel, so this is the single validation choke point.
func validateImageSegments(segs []PromptSegment) error {
	for _, s := range segs {
		for _, c := range s.Content {
			if c.Type != "image" {
				continue
			}
			if s.Role != "user" {
				return fmt.Errorf("image content is only allowed in user-role segments (got role %q)", s.Role)
			}
			if !validImageMediaTypes[c.MediaType] {
				return fmt.Errorf("unsupported image media_type %q (allowed: image/png, image/jpeg, image/gif, image/webp)", c.MediaType)
			}
			if c.Data == "" {
				return errors.New("image content block has empty data")
			}
			if _, err := base64.StdEncoding.DecodeString(c.Data); err != nil {
				return fmt.Errorf("image content block data is not valid base64: %w", err)
			}
		}
	}
	return nil
}

// messagesHaveImage reports whether any assembled message (prior-turn or
// fresh) carries an image content block — the signal the loop uses to decide
// whether the vision capability gate applies for this run.
func messagesHaveImage(messages []providers.Message) bool {
	for _, m := range messages {
		for _, c := range m.Content {
			if c.Type == "image" {
				return true
			}
		}
	}
	return false
}

// FlattenContent is the public version of flattenContent for callers that
// need to apply the same trust-escaping rules during transcript replay
// (continuation endpoint). External callers should not depend on this for
// any other purpose; it's stable but narrow.
func FlattenContent(c PromptContentBlock) providers.ContentBlock {
	return flattenContent(c)
}

// flattenContent converts the caller's typed content union into a provider
// ContentBlock. Untrusted blocks are wrapped in <kind>...</kind> tags so any
// embedded "instructions" lose force. Two protections:
//
//   - kind is validated against allowedUntrustedKinds; unknown values are
//     normalised to "untrusted" so a caller can't open a "system"- or
//     "trusted"-shaped tag.
//
//   - the body is escaped: every `<` becomes `&lt;`. Without this, content
//     containing `</web_content>` followed by attacker text and a re-opened
//     `<web_content>` would syntactically close our wrapping and present
//     the inner text to the model as if it were trusted.
func flattenContent(c PromptContentBlock) providers.ContentBlock {
	switch c.Type {
	case "untrusted-block":
		kind := c.Kind
		if kind == "" || !allowedUntrustedKinds[kind] {
			kind = "untrusted"
		}
		safe := strings.ReplaceAll(c.Text, "<", "&lt;")
		return providers.ContentBlock{
			Type: "text",
			Text: fmt.Sprintf("<%s>\n%s\n</%s>", kind, safe, kind),
		}
	case "image":
		// Images are NOT tag-fenced: untrusted-block fencing defends against
		// text that reads as instructions, but an image is opaque bytes to the
		// wire. Media-type/base64 validity is enforced upstream in
		// validateImageSegments (before any provider call), not here — this
		// function has no error channel. See RFC AT §4.3/§6.
		return providers.ContentBlock{Type: "image", MediaType: c.MediaType, Data: c.Data}
	default: // "trusted-text"
		return providers.ContentBlock{Type: "text", Text: c.Text, Cacheable: c.Cacheable}
	}
}

func iterationCount(messages []providers.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

var _ = json.Valid // keep encoding/json in deps for json.RawMessage docs above

// interactiveAtBoundary reports whether this run should PARK at a turn boundary
// rather than finish.
//
// Read at the boundary rather than captured at start, because a run can be
// promoted to interactive while it is already running — the operator who wants
// to correct an agent mid-flight has no way to have asked for that when the run
// began. The static flag remains the answer for every run that was never
// retuned, which is almost all of them.
func (o *RunOptions) interactiveAtBoundary(ctx context.Context) bool {
	if o.InteractiveNow != nil {
		return o.InteractiveNow(ctx)
	}
	return o.Interactive
}
