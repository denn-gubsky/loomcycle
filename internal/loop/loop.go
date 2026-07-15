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

	// Interactive makes this a PERSISTENT run: instead of terminating when the
	// model ends its turn, the loop parks on SteerQueue waiting for the
	// operator's next instruction (resuming on it, ending only on Cancel).
	// Requires SteerQueue. An interactive run with no explicit MaxIterations is
	// automatically unbounded (Run lifts the 16-turn soft cap — see the
	// interactiveUnbounded note in Run), so an always-on terminal works without
	// also setting UnboundedIterations; the 1<<20 hard ceiling + Cancel still
	// bound it. Set an explicit MaxIterations to cap an interactive session.
	Interactive bool

	// PauseGate, when non-nil, cooperatively quiesces this run for the
	// runtime-wide pause/snapshot protocol (RFC X / F41). At the TOP of each
	// iteration the loop asks PauseRequested(); if true it calls Park, which
	// persists pause_state='paused', blocks until resume (or ctx cancel), then
	// restores 'running'. nil disables pausing (direct loop callers / tests).
	PauseGate PauseGate

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
}

// Run drives the agent loop to completion.
// parkHeartbeatInterval is how often a parked interactive run pulses
// OnHeartbeat while idle, so the staleness sweeper doesn't reap it (the
// per-iteration heartbeat is suspended during the block). Matches the
// interruption tool's blocked-heartbeat cadence. A var (not const) so tests
// can lower it; not an operator knob.
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

// Summarize makes ONE provider call to compact msgs into a summary string. The
// conversation is flattened into a single user message (role-tagged) so the call
// is valid regardless of how msgs alternates, and the model clearly sees
// "summarize this". NO tools are offered → the summary call cannot re-enter the
// tool / compaction machinery. Shared by the loop (auto + self-compact) and the
// server (manual + terminal compaction). model may be a cheaper same-provider model.
func Summarize(ctx context.Context, provider providers.Provider, model string, msgs []providers.Message, targetPct int) (string, error) {
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
	ch, err := provider.Call(ctx, providers.Request{
		Model:  model,
		System: []providers.ContentBlock{{Type: "text", Text: compactionPrompt(targetPct)}},
		Messages: []providers.Message{{
			Role:    "user",
			Content: []providers.ContentBlock{{Type: "text", Text: "Conversation to compact:\n\n" + convo.String()}},
		}},
	})
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
func maybeAutoCompact(ctx context.Context, opts RunOptions, messages []providers.Message, window int, emit func(providers.Event), trigger string) ([]providers.Message, bool) {
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
	firstIdx, cut, ok := CompactionSplit(messages, keepLastN, keepFirst)
	if !ok {
		return messages, false
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
		if err != nil {
			emit(providers.Event{Type: providers.EventError, Error: "compaction summary failed (" + trigger + "): " + err.Error()})
		}
		return messages, false
	}
	before := estimateMessageTokens(messages)
	pinned := ""
	if firstIdx > 0 {
		pinned = messageText(messages[0])
	}
	out := CompactionMessages(pinned, strings.TrimSpace(summary), messages[cut:])
	after := estimateMessageTokens(out)
	emit(providers.Event{Type: providers.EventContextCompaction,
		ContextCompaction: &providers.ContextCompactionEventInfo{
			Summary: strings.TrimSpace(summary), KeepN: len(messages) - cut, KeepFirst: firstIdx > 0,
			BeforeTokens: before, AfterTokens: after, Trigger: trigger}})
	lcotel.RecordCompactionCtx(ctx, trigger, before, after) // per-run-shape metric via OTEL span event
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
		opts.MaxIterations = 16
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

	// Context compaction (v2): a self-request flag the Context op=compact tool
	// sets (checked at the next iteration boundary), plus the previous iteration's
	// context footprint + window ceiling that drive the auto-compact threshold.
	// lastCompactIter debounces back-to-back auto-compactions.
	var compactRequested atomic.Bool
	ctx = tools.WithCompactRequest(ctx, &compactRequested)
	var lastCtxTokens, lastWindow int
	lastCompactIter := -2

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

	// Clamp the operator-supplied retry count to the safety cap so
	// a misconfigured yaml can't induce minute-scale delays per error.
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
	if opts.OnHeartbeat != nil {
		hbDone := make(chan struct{})
		defer close(hbDone)
		go func() {
			t := time.NewTicker(parkHeartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-hbDone:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					opts.OnHeartbeat()
				}
			}
		}()
	}

outerLoop:
	for iter := 0; iter < iterCap; iter++ {
		// v0.10.0 OTEL: one loomcycle.iteration span per turn. Nested
		// under the caller-opened loomcycle.run span (api/http opens
		// the run span at each of the 4 run-creation sites). The
		// iteration body has multiple exit paths (fallback continue,
		// unrecoverable return, terminal break, normal fall-through),
		// so End() is called explicitly at each — Go's `defer` runs
		// at function-scope only, not loop-iteration-scope.
		iterCtx, iterSpan := lcotel.RecordIteration(ctx, iter)

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
				lastCtxTokens = estimateMessageTokens(messages)
			}
		}

		// Auto / self-requested compaction — also a clean boundary (drainSteer
		// ran; no tool cycle is mid-flight). Fires when the agent asked
		// (Context op=compact) OR the PREVIOUS iteration's context footprint
		// crossed the configured threshold. The loop summarizes inline and
		// replaces the history; the smaller next request self-debounces the
		// threshold. Applies to ALL runs (interactive + autonomous).
		if selfReq := compactRequested.Swap(false); selfReq || shouldAutoCompact(opts.Compaction, lastCtxTokens, lastWindow, iter, lastCompactIter) {
			trigger := "auto"
			if selfReq {
				trigger = "self"
			}
			if newMsgs, did := maybeAutoCompact(iterCtx, opts, messages, lastWindow, emit, trigger); did {
				messages = newMsgs
				lastCompactIter = iter
				// Compaction shrank the history; refresh the footprint so op=self
				// below reflects the compacted size, not the pre-compaction value
				// (the next real turn's usage overwrites it).
				lastCtxTokens = estimateMessageTokens(messages)
			}
		}

		// Context footprint (input+cache of the last completed turn, or the
		// post-compaction estimate when a compaction just ran above) so Context
		// op=self can show the agent how full its window is — the signal it needs
		// to decide whether to self-compact (op=compact). Stamped HERE, below
		// drainSteer + auto/self compaction, so a same-turn op=self never reports
		// the stale pre-compaction footprint. 0 on the first iteration.
		iterCtx = tools.WithContextUsage(iterCtx, lastCtxTokens, lastWindow)

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

		req := providers.Request{
			Model:     opts.Model,
			System:    reqSystem,
			Messages:  reqMessages,
			Tools:     toolSpecs,
			MaxTokens: opts.MaxTokens, // 0 → driver default
			Effort:    opts.Effort,    // "" → driver default; PR 3 wires per-driver translation
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
		ch, err := opts.Provider.Call(iterCtx, req)
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
				return RunResult{Iterations: iter}, fmt.Errorf("provider error: %s", ev.Error)
			}
		}

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
			if iterUsage.MaxContextTokens == 0 {
				iterUsage.MaxContextTokens = opts.Provider.Capabilities().MaxContextTokens
			}
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
		}

		stopReason = iterStop
		finalText = iterText

		// Terminal: model is done.
		if iterStop != "tool_use" || len(pendingTools) == 0 {
			// Persistent interactive run: park instead of terminating. Wait
			// for the operator's next instruction (or Cancel). The run holds
			// its concurrency slot while idle — the documented fairness
			// trade-off of an always-on terminal agent (bounded by the
			// existing per-user / global run caps).
			if opts.Interactive && opts.SteerQueue != nil {
				emit(providers.Event{Type: providers.EventAwaitingInput,
					AwaitingInput: &providers.AwaitingInputEventInfo{SinceTurn: iter}})
				// Park until a real operator turn arrives. A steer.KindCompact
				// control replaces the in-memory history with the summary pair
				// and RE-parks (compaction is not itself new input — the
				// operator's next message is), so the loop never calls the
				// provider on the bare summary.
				resumedWithInput := false
				for {
					m, resumed := parkForInput(ctx, opts.SteerQueue, opts.OnHeartbeat)
					if !resumed {
						break // ctx cancelled (or queue closed) → terminate
					}
					if m.Kind == steer.KindCompact {
						messages = applyCompactSummary(messages, m.Text, m.KeepN, m.KeepFirst, emit)
						// Refresh the footprint so the next operator turn's op=self
						// reports the compacted size, not the stale pre-compaction
						// value. Without this a parked run that compacted kept
						// reporting its old ~full context (used_tokens / used_pct)
						// until the next real turn's usage landed.
						lastCtxTokens = estimateMessageTokens(messages)
						continue // re-park: wait for the operator's actual next turn
					}
					messages = append(messages, providers.Message{
						Role:    "user",
						Content: []providers.ContentBlock{{Type: "text", Text: m.Text}},
					})
					if opts.OnSteer != nil {
						opts.OnSteer(m)
					}
					resumedWithInput = true
					break
				}
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
		toolResults := executePendingTools(iterCtx, opts.Dispatcher, pendingTools, opts.ToolParallelism, opts.Hooks, hookIdent, emit)
		messages = append(messages, providers.Message{Role: "user", Content: toolResults})
		// Normal fall-through to next iteration. End the iteration span
		// here — the for loop's continue will open a fresh one.
		iterSpan.End()
	}

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
	return tools.Result{Text: post.Text, IsError: post.IsError}
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
		emit(providers.Event{
			Type:    providers.EventToolResult,
			ToolUse: &providers.ToolUse{ID: r.tu.ID, Name: r.tu.Name, Input: r.tu.Input},
			Text:    r.res.Text,
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
			Text:      r.res.Text,
			IsError:   r.res.IsError,
		}
	}
	return results
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
