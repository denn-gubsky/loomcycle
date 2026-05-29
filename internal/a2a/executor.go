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
}

// NewExecutor builds an Executor for one loomcycle agent. Skill-based
// multi-agent routing is opt-in via WithAgentResolver.
func NewExecutor(r runner.Runner, conn connector.Connector, runs RunReader, agentName string) *Executor {
	return &Executor{runner: r, conn: conn, runs: runs, agentName: agentName}
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

		// SUBMITTED beat only for a brand-new task (no stored task).
		if execCtx.StoredTask == nil {
			if !yield(a2asdk.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		in, err := e.buildRunInput(ctx, execCtx)
		if err != nil {
			yield(a2asdk.NewStatusUpdateEvent(execCtx, taskStateForRejected(),
				agentMessage(err.Error())), nil)
			return
		}

		// Buffered enough to absorb a burst without blocking the loop
		// between yields; the loop still blocks once the buffer fills,
		// which is the desired back-pressure.
		events := make(chan providers.Event, 16)
		var runErr error
		done := make(chan struct{})

		go func() {
			defer close(done)
			defer close(events)
			runErr = e.runner.RunOnce(ctx, in, runner.RunCallbacks{
				OnEvent: func(ev providers.Event) {
					select {
					case events <- ev:
					case <-ctx.Done():
					}
				},
			})
		}()

		for ev := range events {
			for _, out := range translateEvent(ev, execCtx) {
				if !yield(out, nil) {
					// Consumer abandoned the stream; let the run
					// finish in the background (RunOnce honours ctx
					// cancellation from the transport).
					return
				}
			}
		}
		<-done

		if runErr != nil {
			// A setup/internal RunOnce error before a terminal run row
			// is the A2A FAILED case carrying the cause.
			yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
				agentMessage(runErr.Error())), nil)
			return
		}

		outcome, ok := e.finalOutcome(ctx, in.AgentID)
		if !ok {
			yield(a2asdk.NewStatusUpdateEvent(execCtx, a2asdk.TaskStateFailed,
				agentMessage("a2a executor: run finished without a resolvable terminal status")), nil)
			return
		}
		if term, ok := terminalStatusForRun(execCtx, outcome); ok {
			yield(term, nil)
		}
	}
}

// Cancel requests the loop stop working on the task. It routes through
// the Connector's CancelRun (cascade-aware, idempotent) keyed by the
// A2A Task.id, which IS the loomcycle agent_id, then emits the CANCELED
// status the SDK contract expects.
func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2asdk.Event, error] {
	return func(yield func(a2asdk.Event, error) bool) {
		agentID := string(execCtx.TaskID)
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
