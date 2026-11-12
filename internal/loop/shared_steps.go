package loop

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/contextplugin"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The steps both loops take: the append loop (runState.iteration) and the
// stateful loop (statefulLoop.step) call these instead of keeping a copy
// each. Where the two loops still differ around a step — where they call it,
// what result an exit carries — the difference is commented at the call.

// queueMoveAt is the queue-aware routing after the start: the first call's
// provider was chosen when the run started; at each later call boundary, a run
// with no slot held for it (its lease was preempted or expired, or the lane
// keeps none) may move once to a provider where it would not wait so long.
// Never after the first success when the operator pinned runs to their
// provider (PinAfterSuccess) — the reason that pin exists applies to a move as
// much as to a fallback. It returns whether the run has made its move.
func queueMoveAt(ctx context.Context, opts *RunOptions, iter int, moved, succeeded bool, msgs []providers.Message, emit func(providers.Event)) bool {
	if iter > 0 && !moved && opts.MoveForQueue != nil &&
		!(opts.FallbackPolicy.PinAfterSuccess && succeeded) {
		return moveForQueue(ctx, opts, msgs, emit)
	}
	return moved
}

// heartbeatAndPause is the boundary's heartbeat, then the cooperative pause.
//
// The heartbeat is cheap — implementations are expected to be ~one UPDATE.
// Failures should NOT propagate (a sweeper is the authoritative path; a missed
// heartbeat just means we have to wait for the sweeper to catch up). Run's
// lifetime ticker covers a step that blocks longer than one interval.
//
// Cooperative pause/quiesce (RFC X / F41): if a runtime pause is in effect,
// park HERE — a clean boundary before a model call, never mid-turn / between a
// tool call and its result. Park persists pause_state='paused', blocks until
// resume, then restores 'running'. A non-nil return is the park ending on a
// cancel; the caller ends the run.
func heartbeatAndPause(parkCtx context.Context, opts *RunOptions) error {
	if opts.OnHeartbeat != nil {
		opts.OnHeartbeat()
	}
	if opts.PauseGate != nil && opts.PauseGate.PauseRequested() {
		return opts.PauseGate.Park(parkCtx)
	}
	return nil
}

// stampResolvedRoute stamps the CURRENTLY-resolved provider, model and
// sampling onto ctx so the Context tool's op=self can report them to the
// agent (non-secret introspection). tryProviderFallback swaps
// opts.Provider/opts.Model in place, so a loop stamps them where they are
// current for the tools that read them.
func stampResolvedRoute(ctx context.Context, opts *RunOptions) context.Context {
	ctx = tools.WithResolvedProvider(ctx, opts.Provider.ID())
	ctx = tools.WithResolvedModel(ctx, opts.Model)
	return tools.WithResolvedSampling(ctx, opts.Sampling)
}

// transformOutbound runs the context-transform plugins (RFC Z / F43) on a COPY
// of the outbound context — the loop's canonical system/messages (and so the
// persisted transcript + code-js replay input) stay untouched. Skipped for the
// synthetic code-js provider (local replay, no external leak; redacting its
// bytes would trip replay divergence). A failure is emitted here and returned
// for the caller to end the run on.
func transformOutbound(ctx context.Context, opts *RunOptions, system []providers.ContentBlock, msgs []providers.Message, emit func(providers.Event)) ([]providers.ContentBlock, []providers.Message, error) {
	if len(opts.ContextPlugins) > 0 && opts.Provider.ID() != codeJSProviderID {
		cs, cm, perr := contextplugin.Apply(ctx, opts.ContextPlugins, system, msgs)
		if perr != nil {
			emit(providers.Event{Type: providers.EventError, Error: "context transform: " + perr.Error()})
			return nil, nil, perr
		}
		return cs, cm, nil
	}
	return system, msgs, nil
}

// dispatchInterruptible runs the turn's tool calls through executePendingTools
// — hooks, the parallel_spawn ledger key, failure classification and the
// tool_result events — under a ctx a shutdown interrupts (RFC ES F1), so the
// run parks at its next boundary.
func dispatchInterruptible(ctx context.Context, opts *RunOptions, pending []providers.ToolUse, parallelism int, hookIdent hooks.Identity, emit func(providers.Event)) []providers.ContentBlock {
	ctx, stopInterrupt := interruptibleTools(ctx, opts.PauseGate)
	blocks := executePendingTools(ctx, opts.Dispatcher, pending, parallelism, opts.Hooks, hookIdent, emit)
	stopInterrupt()
	return blocks
}

// repeatedFailureStop reports the dispatcher's verdict that the run keeps
// repeating a call that cannot succeed: the error event, and the error the
// run ends on. nil when the run may go on.
func repeatedFailureStop(opts *RunOptions, emit func(providers.Event)) error {
	if why, stop := opts.Dispatcher.RepeatedFailure(); stop {
		msg := "run stopped: " + why + " after being told it cannot succeed as sent"
		emit(providers.Event{Type: providers.EventError, Error: msg})
		return errors.New(msg)
	}
	return nil
}

// promptFootprint is the provider's own count of the whole request a reply
// answered: input plus cache, which is what the distillation and eviction
// thresholds measure.
func promptFootprint(u *providers.Usage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}
