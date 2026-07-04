package a2a

import (
	"context"
	"fmt"
	"iter"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Executor implements the SDK's a2asrv.AgentExecutor by driving
// loomcycle's run loop. Each inbound A2A message becomes one
// runner.RunOnce; the run's provider-event stream is translated into
// A2A Task events (see events.go); cancellation routes through the
// Connector's cascade-aware CancelRun.
//
// All dependencies are constructor-injected so the package stays
// unit-testable against fakes (a scripted runner.Runner + an in-memory
// store) with no HTTP wiring.
type Executor struct {
	// runner drives the actual agent loop. Blocking; we run it in a
	// goroutine and bridge its callbacks onto the SDK's event iterator.
	runner runner.Runner

	// conn is the cancel path. CancelRun cascades to sub-agent runs and
	// is idempotent — exactly the semantics A2A's Cancel expects.
	conn connector.Connector

	// runs resolves an A2A Task.id (== loomcycle agent_id) to a run row
	// for the terminal-status lookup after RunOnce returns.
	runs RunReader

	// agentName is the fallback loomcycle agent this executor fronts
	// when no skill-based resolution applies (single-agent server, or a
	// request that carried no skill id). A2A skills route to a server
	// card; the card may bind multiple loomcycle agents (one per
	// exposed skill) — see resolveAgent.
	agentName string

	// resolveAgent maps an inbound A2A skill id to a loomcycle agent
	// name. The A2A-5 mounting layer injects it from the active
	// A2AServerCardDef's exposed_agents so ONE mounted server can
	// dispatch to the right agent per request. Returns ("", false) for
	// an unknown/unexposed skill, which the executor rejects.
	//
	// Nil ⇒ single-agent mode: every request routes to agentName
	// regardless of skill id (back-compat with the A2A-4 bridge tests).
	resolveAgent func(skillID string) (string, bool)

	// resolver wakes a run parked on an Interruption.ask when a
	// same-task follow-up message/send arrives. Nil ⇒ the INPUT_REQUIRED
	// bridge is disabled: a parked run is treated like any other (it
	// still parks, but a follow-up starts a fresh run rather than
	// resuming). Installed via WithInterruptionBridge.
	resolver InterruptResolver

	// parks tracks runs blocked on an interruption so a resume can
	// re-attach to the same run's event stream. Always non-nil.
	parks *parkRegistry
}

// NewExecutor builds an Executor for one loomcycle agent. Skill-based
// multi-agent routing is opt-in via WithAgentResolver; the
// INPUT_REQUIRED ↔ Interruption bridge is opt-in via
// WithInterruptionBridge.
func NewExecutor(r runner.Runner, conn connector.Connector, runs RunReader, agentName string) *Executor {
	return &Executor{runner: r, conn: conn, runs: runs, agentName: agentName, parks: newParkRegistry()}
}

// WithInterruptionBridge installs the resolver that wakes runs parked on
// an Interruption.ask. With it set, a same-task follow-up message/send
// RESOLVES the pending interruption (rather than starting a new run) and
// resumes the parked run to terminal. Without it, parking still surfaces
// INPUT_REQUIRED but a follow-up starts a fresh run.
func (e *Executor) WithInterruptionBridge(r InterruptResolver) *Executor {
	e.resolver = r
	return e
}

// WithAgentResolver installs a skill-id → agent-name resolver so one
// Executor fronts the multiple loomcycle agents an A2AServerCardDef
// exposes. The A2A-5 server builds this from the card's exposed_agents.
func (e *Executor) WithAgentResolver(resolve func(skillID string) (string, bool)) *Executor {
	e.resolveAgent = resolve
	return e
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// Execute drives one agent run and yields the resulting A2A event
// stream: SUBMITTED (only when no task exists yet), then per-provider-
// event artifacts/status, then a terminal status from the run's final
// RunStatus. Per the SDK contract, the server stops consuming after the
// terminal status event.
//
// The run executes in a goroutine because runner.RunOnce blocks and
// reports progress via callbacks; the callbacks forward onto a channel
// that this iterator drains, so back-pressure from a slow A2A consumer
// (a stalled yield) naturally pauses event delivery.
func (e *Executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2asdk.Event, error] {
	return func(yield func(a2asdk.Event, error) bool) {
		if execCtx.Message == nil {
			yield(nil, fmt.Errorf("a2a executor: nil message on execute"))
			return
		}

		// RESUME path: a follow-up message carrying a known task whose
		// run is parked on an interruption is an answer, not a new run.
		// Detect it and route to resolve+resume instead of starting a
		// fresh run. An UNKNOWN task (no parked run for it) falls through
		// to the fresh-start path below — exactly the "unknown-taskId
		// follow-up starts a new run" contract.
		if execCtx.StoredTask != nil {
			if parked, ok := e.parks.take(execCtx.TaskID); ok {
				e.resumeParkedRun(ctx, execCtx, parked, yield)
				return
			}
		}

		in, err := e.buildRunInput(ctx, execCtx)
		if err != nil {
			// A rejected brand-new message (unknown skill, unsupported
			// part, bad input) still needs a Task to fail against: the SDK
			// aggregation rejects a bare status update as the first event
			// ("first event must be a Task or a message"). Emit the
			// SUBMITTED task first, then the FAILED status — so the
			// rejection surfaces as a terminal FAILED task rather than an
			// "invalid agent response" transport error. A stored task is
			// already present, so the beat is skipped on a resume.
			if execCtx.StoredTask == nil {
				if !yield(a2asdk.NewSubmittedTask(execCtx, execCtx.Message), nil) {
					return
				}
			}
			yield(a2asdk.NewStatusUpdateEvent(execCtx, taskStateForRejected(),
				agentMessage(err.Error())), nil)
			return
		}

		// SUBMITTED beat only for a brand-new task (no stored task).
		if execCtx.StoredTask == nil {
			if !yield(a2asdk.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		stream := e.startRun(ctx, in)
		e.streamToClient(ctx, execCtx, in.AgentID, stream, yield)
	}
}

// runStream is the live output of a backgrounded run: forwarded events,
// a done signal, and a pointer to the run error (valid after done).
type runStream struct {
	out     <-chan providers.Event
	done    <-chan struct{}
	runErr  *error
	agentID string
	// cancel stops the detached run. RunOnce's lifetime is decoupled from
	// the per-request ctx (see startRun), so callers must cancel
	// explicitly when the run is abandoned or can never be resumed.
	cancel context.CancelFunc
}

// startRun launches RunOnce in a goroutine and forwards its OnEvent
// callbacks onto a buffered channel. The goroutine owns the channel and
// outlives any single Execute call, so a run that parks on an
// interruption keeps draining in the background — the resume Execute
// re-attaches to the SAME `out` channel via the park registry. This is
// the key difference from a simple per-call goroutine: events emitted
// after the park (once the bus wakes the run) are not lost.
func (e *Executor) startRun(ctx context.Context, in runner.RunInput) *runStream {
	// Buffered to absorb bursts; the run still blocks once full, which
	// is the desired back-pressure when no consumer is attached.
	events := make(chan providers.Event, 16)
	done := make(chan struct{})
	var runErr error
	// Detach the run's lifetime from the per-request ctx. The SDK cancels
	// the request ctx the instant the FIRST Execute response completes
	// (a2asrv jsonrpc.go/rest.go: `requestCtx, cancel := WithCancel(ctx);
	// defer cancel()`). A run that PARKS on an Interruption must outlive
	// that first response so a follow-up message can resume it — and the
	// Interruption tool's wait honours ctx.Done(), so a request-scoped ctx
	// would abort the parked run with context.Canceled before any answer
	// arrives, breaking INPUT_REQUIRED entirely. Cancellation now flows
	// only through explicit paths: Executor.Cancel (Connector cascade) and
	// stream.cancel (client abandon / unresumable park).
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(done)
		defer close(events)
		// Release the detached context when the run returns so a
		// completed run never leaks its cancel.
		defer cancel()
		runErr = e.runner.RunOnce(runCtx, in, runner.RunCallbacks{
			OnEvent: func(ev providers.Event) {
				select {
				case events <- ev:
				case <-runCtx.Done():
				}
			},
		})
	}()
	return &runStream{out: events, done: done, runErr: &runErr, agentID: in.AgentID, cancel: cancel}
}

// streamToClient drains a run stream, yielding translated A2A events to
// the client. It implements the INPUT_REQUIRED bridge: when the run
// emits EventInterruptionPending it has parked on the bus inside the
// Interruption tool, so the executor emits TASK_STATE_INPUT_REQUIRED
// (carrying the question), registers the still-live stream in the park
// registry keyed by the task, and RETURNS — pausing yielding exactly as
// A2A-4 found the SDK expects on input-required. The background run
// goroutine stays blocked on the bus; a follow-up message resolves it.
//
// On normal stream end it yields the run's terminal status. Setup errors
// surface as FAILED.
func (e *Executor) streamToClient(ctx context.Context, execCtx *a2asrv.ExecutorContext, agentID string, stream *runStream, yield func(a2asdk.Event, error) bool) {
	for ev := range stream.out {
		if ev.Type == providers.EventInterruptionPending {
			// The run is now parked on intr:<id>. Surface the question
			// as INPUT_REQUIRED and stop consuming; the run lives on in
			// the background so a resume can continue it.
			if e.resolver == nil {
				// No resume bridge wired: the parked run can never be
				// woken and a follow-up message starts a fresh run, so
				// cancel it rather than leak a goroutine blocked on the
				// bus forever.
				stream.cancel()
				yield(inputRequiredStatus(execCtx, ev.Interruption), nil)
				return
			}
			e.parks.put(execCtx.TaskID, &parkedRun{
				out:       stream.out,
				done:      stream.done,
				runErrPtr: stream.runErr,
				agentID:   agentID,
				cancel:    stream.cancel,
			})
			if !yield(inputRequiredStatus(execCtx, ev.Interruption), nil) {
				// Client abandoned the stream exactly at the park: no
				// resume will arrive on this connection. Reclaim the
				// registry entry (a concurrent resume may have taken it
				// first — then it owns the run) and cancel the otherwise-
				// unreachable run so it does not block on the bus forever.
				if p, ok := e.parks.take(execCtx.TaskID); ok {
					p.cancel()
				}
			}
			return
		}
		for _, out := range translateEvent(ev, execCtx) {
			if !yield(out, nil) {
				// Consumer abandoned the stream. The run is detached from
				// the request ctx (see startRun), so cancel it explicitly
				// rather than relying on a ctx teardown that no longer
				// reaches it.
				stream.cancel()
				return
			}
		}
	}
	<-stream.done
	e.yieldTerminal(ctx, execCtx, agentID, *stream.runErr, yield)
}

// resumeParkedRun answers the run's pending interruption and re-attaches
// to its already-running event stream, yielding the rest of the run to
// terminal. The follow-up message's text is the human's answer. If the
// run parks AGAIN on a second interruption, the bridge re-registers it
// (streamToClient handles that) so multi-turn input flows compose.
func (e *Executor) resumeParkedRun(ctx context.Context, execCtx *a2asrv.ExecutorContext, parked *parkedRun, yield func(a2asdk.Event, error) bool) {
	run, err := e.runs.GetRunByAgentID(ctx, parked.agentID)
	if err != nil {
		yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
			agentMessage("a2a executor: resume could not locate the parked run: "+err.Error())), nil)
		return
	}
	intrID, ok := e.resolver.PendingForRun(ctx, run.ID)
	if !ok {
		// No pending interrupt despite a registered park — the run was
		// resolved out-of-band (timeout, HTTP resolve) between park and
		// this follow-up. Re-attach and stream to terminal anyway.
		e.streamFromParked(ctx, execCtx, parked, yield)
		return
	}
	answer, aerr := answerFromMessage(execCtx.Message)
	if aerr != nil {
		// The follow-up carried a non-text part (file/data). It cannot be an
		// interruption answer; fail loudly rather than resolve with "" and
		// wake the parked run as if the human answered nothing — mirroring
		// buildRunInput's rejection on the brand-new-message path.
		yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
			agentMessage("a2a executor: resume message content unusable (only text parts are accepted): "+aerr.Error())), nil)
		return
	}
	if answer == "" {
		yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
			agentMessage("a2a executor: resume message carried no usable text answer")), nil)
		return
	}
	if err := e.resolver.Resolve(ctx, intrID, answer); err != nil {
		yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
			agentMessage("a2a executor: resolve interruption failed: "+err.Error())), nil)
		return
	}
	e.streamFromParked(ctx, execCtx, parked, yield)
}

// streamFromParked re-attaches to a parked run's stream and drives it to
// terminal (or to a second park). Same body as the tail of
// streamToClient but starting from an existing stream.
func (e *Executor) streamFromParked(ctx context.Context, execCtx *a2asrv.ExecutorContext, parked *parkedRun, yield func(a2asdk.Event, error) bool) {
	stream := &runStream{out: parked.out, done: parked.done, runErr: parked.runErrPtr, agentID: parked.agentID, cancel: parked.cancel}
	e.streamToClient(ctx, execCtx, parked.agentID, stream, yield)
}

// yieldTerminal emits the closing status event for a finished run.
func (e *Executor) yieldTerminal(ctx context.Context, execCtx *a2asrv.ExecutorContext, agentID string, runErr error, yield func(a2asdk.Event, error) bool) {
	if runErr != nil {
		// A setup/internal RunOnce error before a terminal run row is
		// the A2A FAILED case carrying the cause.
		yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
			agentMessage(runErr.Error())), nil)
		return
	}
	if outcome, ok := e.finalOutcome(ctx, agentID); ok {
		if term, ok := terminalStatusForRun(execCtx, outcome); ok {
			yield(term, nil)
			return
		}
	}
	// No resolvable terminal status — the run row is missing, or it is
	// still non-terminal because the terminal write lagged or failed
	// (finalOutcome accepts a RunRunning row, which maps to the
	// non-terminal WORKING state). The SDK contract requires the closing
	// event to be terminal, so fail closed rather than ending the stream
	// in WORKING (which would strand the A2A client's task forever).
	yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
		agentMessage("a2a executor: run finished without a resolvable terminal status")), nil)
}

// inputRequiredStatus builds the TASK_STATE_INPUT_REQUIRED event from a
// pending interruption, carrying the question text as the status message
// so the A2A client can render the prompt. Verbatim from the interrupt
// row (loop-originated, never unauthenticated model-policy text).
func inputRequiredStatus(info a2asdk.TaskInfoProvider, intr *providers.InterruptionEventInfo) a2asdk.Event {
	var msg *a2asdk.Message
	if intr != nil && intr.Question != "" {
		msg = a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart(intr.Question))
	}
	return a2asdk.NewStatusUpdateEvent(info, a2asdk.TaskStateInputRequired, msg)
}

// answerFromMessage extracts the human's answer text from a resume
// message's parts (text parts concatenated). The answer is recorded
// against the interruption + fed back into the parked loop. It returns an
// error when the message carries a non-text part (file/data) so the caller
// can reject the resume rather than silently resolving with "" — the same
// loud rejection partsToContentBlocks gives the brand-new-message path.
func answerFromMessage(msg *a2asdk.Message) (string, error) {
	if msg == nil {
		return "", nil
	}
	blocks, err := partsToContentBlocks(msg.Parts)
	if err != nil {
		return "", err
	}
	var out string
	for _, b := range blocks {
		if b.Text == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out, nil
}

// Cancel requests the loop stop working on the task. It routes through
// the Connector's CancelRun (cascade-aware, idempotent) keyed by the
// A2A Task.id, which IS the loomcycle agent_id, then emits the CANCELED
// status the SDK contract expects.
func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2asdk.Event, error] {
	return func(yield func(a2asdk.Event, error) bool) {
		agentID := string(execCtx.TaskID)
		// Tenant gate before the destructive cancel. agentID is the
		// caller-supplied A2A Task.id; CancelRun resolves it purely by
		// agent_id (no tenant predicate), so without this a peer on
		// tenant-A's routed host could cancel tenant-B's in-flight run
		// (cascading to its sub-agents). No-op in single-tenant/none mode.
		if err := authorizeTaskTenant(ctx, e.runs, agentID); err != nil {
			yield(nil, err)
			return
		}
		if _, err := e.conn.CancelRun(ctx, agentID, "a2a cancel"); err != nil {
			yield(nil, fmt.Errorf("a2a executor: cancel run %q: %w", agentID, err))
			return
		}
		yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateCanceled, nil), nil)
	}
}

// buildRunInput maps the inbound A2A message + authenticated principal
// to a loomcycle RunInput. The A2A Task.id becomes the loomcycle
// agent_id so the two share one addressable handle; the message parts
// become a single user prompt segment of trusted-text blocks (the parts
// arrive from an authenticated A2A peer, so they enter as trusted
// input, matching how the HTTP /v1/runs body is treated).
func (e *Executor) buildRunInput(ctx context.Context, execCtx *a2asrv.ExecutorContext) (runner.RunInput, error) {
	agent, err := e.agentFor(execCtx)
	if err != nil {
		return runner.RunInput{}, err
	}
	blocks, err := partsToContentBlocks(execCtx.Message.Parts)
	if err != nil {
		return runner.RunInput{}, err
	}
	if len(blocks) == 0 {
		return runner.RunInput{}, fmt.Errorf("a2a executor: message had no usable content parts")
	}
	p := principalFromContext(ctx, execCtx.Tenant)
	return runner.RunInput{
		Agent:    agent,
		AgentID:  string(execCtx.TaskID),
		TenantID: p.TenantID,
		UserID:   p.UserID,
		Segments: []loop.PromptSegment{{Role: "user", Content: blocks}},
		// RFC AX anti-bypass: carry the operator-key restriction the frontier
		// interceptor derived from THIS peer's own scopes, so a restricted A2A
		// peer's run is restricted at admission (RunOnce reads this off RunInput).
		OperatorKeyRestricted: OperatorKeyRestrictedFrom(ctx),
	}, nil
}

// agentFor resolves which loomcycle agent handles this request. With no
// resolver installed (single-agent mode) it is always e.agentName. With
// a resolver, the inbound A2A skill id (Message.Metadata["skillId"], the
// spec's skill-selection carrier) is mapped to an exposed agent; an
// unknown or absent skill is REJECTED rather than silently falling back,
// so a peer cannot reach an unexposed agent by omitting the skill id.
func (e *Executor) agentFor(execCtx *a2asrv.ExecutorContext) (string, error) {
	if e.resolveAgent == nil {
		return e.agentName, nil
	}
	skillID := skillIDFromMessage(execCtx.Message)
	if skillID == "" {
		return "", fmt.Errorf("a2a executor: request carried no skill id; this server exposes multiple agents and requires one")
	}
	agent, ok := e.resolveAgent(skillID)
	if !ok {
		return "", fmt.Errorf("a2a executor: unknown or unexposed skill %q", skillID)
	}
	return agent, nil
}

// skillIDFromMessage extracts the A2A skill id from a message's
// metadata. The A2A spec carries skill selection in Message.Metadata
// under the "skillId" key. Returns "" when absent or not a string.
func skillIDFromMessage(msg *a2asdk.Message) string {
	if msg == nil || msg.Metadata == nil {
		return ""
	}
	if v, ok := msg.Metadata["skillId"].(string); ok {
		return v
	}
	return ""
}

// finalOutcome resolves the run's terminal outcome from the run table
// after RunOnce returns. Detail is the StopReason for a completed run
// or the error message for a failed one.
func (e *Executor) finalOutcome(ctx context.Context, agentID string) (runOutcome, bool) {
	run, err := e.runs.GetRunByAgentID(ctx, agentID)
	if err != nil {
		return runOutcome{}, false
	}
	if _, ok := TaskStateForRunStatus(run.Status); !ok {
		return runOutcome{}, false
	}
	detail := run.StopReason
	if run.Status == store.RunFailed && run.ErrorMsg != "" {
		detail = run.ErrorMsg
	}
	return runOutcome{Status: run.Status, Detail: detail}, true
}

// partsToContentBlocks maps A2A message parts to loomcycle prompt
// content blocks. Text parts become trusted-text blocks. Non-text parts
// (file/data/url) are not supported as run input this slice — they
// surface as an error so the executor can reject the message rather than
// silently dropping content the caller expected to matter.
func partsToContentBlocks(parts a2asdk.ContentParts) ([]loop.PromptContentBlock, error) {
	blocks := make([]loop.PromptContentBlock, 0, len(parts))
	for i, part := range parts {
		if part == nil {
			continue
		}
		switch part.Content.(type) {
		case a2asdk.Text:
			blocks = append(blocks, loop.PromptContentBlock{
				Type: "trusted-text",
				Text: part.Text(),
			})
		default:
			return nil, fmt.Errorf("a2a executor: unsupported part type at index %d (only text parts accepted this version)", i)
		}
	}
	return blocks, nil
}

// agentMessage wraps a status detail string as an agent-role A2A
// message for a status event. Returns nil for an empty string so the
// status event carries no message rather than an empty one.
func agentMessage(text string) *a2asdk.Message {
	if text == "" {
		return nil
	}
	return a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart(text))
}
