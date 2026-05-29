package a2a

import (
	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// translateEvent maps one loomcycle providers.Event to zero-or-more A2A
// events, given the task-info provider (the ExecutorContext) that stamps
// task_id + context_id onto each emitted event.
//
// The mapping is deliberately lossy-toward-A2A-semantics: the A2A
// protocol's event vocabulary is artifacts + status transitions, so
// loomcycle's finer-grained provider events collapse onto those:
//
//   - EventStarted  → status WORKING (the run is now executing).
//   - EventText     → a text artifact (TaskArtifactUpdateEvent). One
//     artifact per text event; the SDK's append semantics are handled
//     by the caller (executor) which decides new-vs-append.
//   - EventToolResult → if it carries file-shaped output, a FilePart
//     artifact; otherwise no A2A event (tool-call/tool-result chatter
//     is loomcycle-internal and not part of the A2A artifact stream).
//   - EventDone / EventError / EventUsage → no event here; terminal
//     status is emitted by the executor from the run's final
//     RunStatus + StopReason, which is the authoritative outcome (a
//     stream may carry EventError mid-run and still complete).
//
// Returning a slice (rather than a single event) keeps the contract
// honest: most events map to exactly one, EventToolResult may map to
// zero, and future kinds can map to several without changing callers.
func translateEvent(ev providers.Event, info a2asdk.TaskInfoProvider) []a2asdk.Event {
	switch ev.Type {
	case providers.EventStarted:
		return []a2asdk.Event{a2asdk.NewStatusUpdateEvent(info, a2asdk.TaskStateWorking, nil)}

	case providers.EventText:
		if ev.Text == "" {
			return nil
		}
		return []a2asdk.Event{a2asdk.NewArtifactEvent(info, a2asdk.NewTextPart(ev.Text))}

	case providers.EventToolResult:
		// Only surface tool results that look like file output. The
		// generic tool-call/tool-result loop is loomcycle-internal
		// transcript detail, not A2A artifact content. A file result
		// is carried as Text today (loomcycle has no typed file block
		// on providers.Event); when a result is flagged as an error we
		// drop it — A2A artifacts represent produced content, and a
		// failed tool call is reflected in the terminal status, not an
		// artifact.
		if ev.IsError || ev.Text == "" {
			return nil
		}
		// No file-typed block exists on providers.Event in this slice;
		// tool results carrying text are not promoted to artifacts to
		// avoid duplicating the assistant's EventText. Reserved for a
		// later slice that threads a typed FilePart through the loop.
		return nil

	default:
		// EventToolCall, EventUsage, EventDone, EventError and any
		// future typed event have no standalone A2A artifact
		// representation — terminal status is the executor's job.
		return nil
	}
}

// runOutcome is the finished-run summary the executor feeds to
// terminalStatusForRun: the persisted RunStatus plus a human-readable
// detail string (StopReason for a completed run, error message for a
// failed one).
type runOutcome struct {
	Status store.RunStatus
	Detail string
}

// terminalStatusForRun maps a finished run's outcome to the closing A2A
// status event. The detail rides along as the status message so an A2A
// client can render "why it ended" without a follow-up fetch. It is
// surfaced verbatim — it originates from the run loop, never from
// unauthenticated model-supplied policy. Returns (event, true) on a
// known terminal status; (nil, false) for a non-terminal status (caller
// treats that as a bug — the run loop returned without a terminal
// state).
func terminalStatusForRun(info a2asdk.TaskInfoProvider, outcome runOutcome) (a2asdk.Event, bool) {
	state, ok := TaskStateForRunStatus(outcome.Status)
	if !ok || !state.Terminal() {
		return nil, false
	}
	var msg *a2asdk.Message
	if outcome.Detail != "" {
		msg = a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart(outcome.Detail))
	}
	return a2asdk.NewStatusUpdateEvent(info, state, msg), true
}
