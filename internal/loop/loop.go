// Package loop runs the model→tool_use→tool_result→model cycle.
//
// One Run() call drives one agent run to completion. It calls the provider,
// streams events to the caller, dispatches tool_use to the dispatcher, sends
// tool_result back to the provider on the next iteration, and stops when the
// model signals end_turn (or hits MaxIterations).
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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
type PromptContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Cacheable bool   `json:"cacheable,omitempty"`
	Kind      string `json:"kind,omitempty"` // for untrusted-block: e.g. "web_content", "uploaded_cv"
}

// RunOptions is one Run() invocation.
type RunOptions struct {
	Provider      providers.Provider
	Model         string
	Tools         []tools.Tool
	Dispatcher    *tools.Dispatcher
	Segments      []PromptSegment
	OnEvent       func(providers.Event) // streaming hook (called from loop goroutine)
	MaxIterations int                   // safety cap; default 16

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

	// Cache-loss event when switching AWAY from Anthropic. The
	// cache_control breakpoints in the system block (and on system_
	// prompt segments) are Anthropic-specific; no other provider
	// carries the equivalent state today. A switch FROM anthropic
	// means downstream iterations on the new provider get cache-
	// cold rates. Switches INTO anthropic don't emit this event —
	// the new provider has no cache to begin with, and Anthropic's
	// implicit cache will warm naturally.
	if failedProvider == "anthropic" && newProvider.ID() != "anthropic" {
		emit(providers.Event{
			Type: providers.EventCacheInvalidated,
			Text: fmt.Sprintf("Anthropic cache_control breakpoints lost on switch to %s; this run's downstream iterations will be cache-cold", newProvider.ID()),
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
			stripped++
		}
	}
	if stripped > 0 {
		emit(providers.Event{
			Type: providers.EventReasoningInvalidated,
			Text: fmt.Sprintf("cleared reasoning_content from %d assistant turn(s) on switch from %s to %s; cross-provider echo would 400", stripped, failedProvider, newProvider.ID()),
		})
	}

	opts.Provider = newProvider
	opts.Model = newModel
	opts.Effort = newEffort
	return fallbackOutcomeSwitched
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
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	if opts.MaxIterations == 0 {
		opts.MaxIterations = 16
	}
	if opts.ToolParallelism <= 0 {
		opts.ToolParallelism = 8
	}
	if opts.Provider == nil {
		return RunResult{}, fmt.Errorf("loop: provider is nil")
	}

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

	emit(providers.Event{Type: providers.EventStarted})

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

outerLoop:
	for iter := 0; iter < opts.MaxIterations; iter++ {
		// Heartbeat fires at the top of each iteration. Cheap path —
		// implementations are expected to be ~one UPDATE. Failures
		// should NOT propagate (a sweeper in the future is the
		// authoritative path; a missed heartbeat just means we have
		// to wait for the sweeper to catch up).
		if opts.OnHeartbeat != nil {
			opts.OnHeartbeat()
		}
		req := providers.Request{
			Model:     opts.Model,
			System:    system,
			Messages:  messages,
			Tools:     toolSpecs,
			MaxTokens: opts.MaxTokens, // 0 → driver default
			Effort:    opts.Effort,    // "" → driver default; PR 3 wires per-driver translation
			// OnEvent lets the driver fire pre-channel events (currently
			// EventRetry during a 429 sleep) directly to the same caller
			// hook the loop uses for response events. Without this hop
			// the retry would be invisible to SSE consumers.
			OnEvent: emit,
		}
		ch, err := opts.Provider.Call(ctx, req)
		if err != nil {
			// Resolver stall feedback: any non-context error from
			// Call() is a driver giving up after its own retries.
			// Mark the (provider, model) stalled so the resolver
			// stops returning it until the next periodic probe
			// re-validates. ctx errors are user-side cancellation,
			// not provider faults — don't pollute the matrix.
			if opts.MarkStalled != nil && ctx.Err() == nil {
				opts.MarkStalled(opts.Provider.ID(), opts.Model, err.Error())
			}
			// v0.8.2: when the run carries a fallback policy and the
			// error class is retryable, swap to the next provider
			// and re-run this iteration. tryProviderFallback emits
			// EventProviderFallback (+ EventCacheInvalidated when
			// switching away from anthropic) and mutates opts in
			// place. Outcome==Switched → continue the outer loop
			// without surfacing the error. Other outcomes fall
			// through to the original error path.
			if tryProviderFallback(ctx, &opts, &fallbackAttempts, err, emit, messages, firstTurnSucceeded) == fallbackOutcomeSwitched {
				continue outerLoop
			}
			emit(providers.Event{Type: providers.EventError, Error: err.Error()})
			return RunResult{Iterations: iter}, err
		}

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

		for ev := range ch {
			switch ev.Type {
			case providers.EventText:
				iterText += ev.Text
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
			case providers.EventError:
				// Resolver stall feedback for in-stream errors:
				// driver opened the SSE/NDJSON stream successfully
				// but then surfaced a provider-side error (5xx
				// mid-stream, model 404 on dispatch, etc.). Mark
				// stalled. Same ctx-guard as above — user cancel
				// shouldn't pollute the matrix.
				if opts.MarkStalled != nil && ctx.Err() == nil {
					opts.MarkStalled(opts.Provider.ID(), opts.Model, ev.Error)
				}
				// v0.8.2: classify the RAW provider error string
				// (e.g. "anthropic 503: backend unavailable") for
				// the fallback decision. The classifier's status-
				// prefix regex needs the bare "<name> <code>:"
				// shape, which the streamErr wrap below would
				// obscure. The wrap is reserved for the FINAL
				// return value when fallback isn't eligible.
				rawErr := errors.New(ev.Error)
				if tryProviderFallback(ctx, &opts, &fallbackAttempts, rawErr, emit, messages, firstTurnSucceeded) == fallbackOutcomeSwitched {
					for range ch {
					}
					continue outerLoop
				}
				emit(ev)
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
			Role:      "assistant",
			Content:   assistantBlocks,
			Reasoning: iterReasoning,
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
			emit(providers.Event{Type: providers.EventUsage, Usage: iterUsage})
		}

		stopReason = iterStop
		finalText = iterText

		// Terminal: model is done.
		if iterStop != "tool_use" || len(pendingTools) == 0 {
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
		}
		toolResults := executePendingTools(ctx, opts.Dispatcher, pendingTools, opts.ToolParallelism, opts.Hooks, hookIdent, emit)
		messages = append(messages, providers.Message{Role: "user", Content: toolResults})
	}

	// If the for loop exited by exhausting MaxIterations while the model was
	// still mid-tool-use, the stop_reason will be stuck at "tool_use" but no
	// tools ran on this final iteration. Surface that distinctly to the
	// caller — they can decide whether to bump MaxIterations and retry, or
	// surface a different error to the user.
	if stopReason == "tool_use" {
		stopReason = "max_iterations"
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
