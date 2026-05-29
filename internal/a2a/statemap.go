// Package a2a bridges the github.com/a2aproject/a2a-go SDK's
// server-side AgentExecutor surface to loomcycle's run loop. It is the
// reusable, transport-agnostic core: an executor that drives
// runner.RunOnce from an inbound A2A Message, an events translator that
// turns providers.Event into a2a.Event, and a TaskStore backed by
// loomcycle's run table. The HTTP-route mounting / well-known URI /
// multi-tenant routing that consumes this package lives in a later
// slice (A2A-5); this package stays unit-testable in isolation against
// a fake runner.Runner and an in-memory store.
//
// Security posture inherited from the rest of loomcycle: peer bearers
// and signing keys never enter logs or OTEL spans. The bridge only
// reads an already-authenticated principal (see auth.go) and never
// makes a trust decision from model-derived text.
package a2a

import (
	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runStatusToTaskState is the single source of truth for translating a
// loomcycle run lifecycle state into the A2A TaskState vocabulary.
//
// loomcycle persists only four RunStatus values (running / completed /
// failed / cancelled — see store.RunStatus). The A2A protocol has a
// wider state set (submitted, input_required, rejected, auth_required).
// The states loomcycle never persists are handled as follows:
//
//   - SUBMITTED is emitted by the executor BEFORE the run starts (the
//     A2A "task accepted, not yet working" beat), not derived from a
//     stored row — so it has no RunStatus to map from.
//   - INPUT_REQUIRED is the A2A representation of a run parked on an
//     interruption (the Interruption tool's human-in-the-loop wait).
//     loomcycle models that as a still-"running" row plus a pending
//     interrupt record; the actual park/resume wiring is slice A2A-6.
//     This slice exposes the mapping so callers that detect an
//     awaiting-input run can translate it, but does not build the
//     resume path. Because no distinct RunStatus exists for it, the
//     mapping is keyed by the helper below, not this table.
//   - REJECTED maps an inbound A2A "agent refused to start" outcome to
//     FAILED on loomcycle's side; loomcycle never persists a "rejected"
//     RunStatus, so it cannot appear as an input to this table. The
//     pairing rejected→FAILED is asserted in the tests as a documented
//     invariant of taskStateForRejected.
//
// Returns TaskStateUnspecified for any RunStatus the table does not
// know — callers treat that as a bug (an unmapped state) rather than
// silently defaulting to a terminal state.
var runStatusToTaskState = map[store.RunStatus]a2asdk.TaskState{
	store.RunRunning:   a2asdk.TaskStateWorking,
	store.RunCompleted: a2asdk.TaskStateCompleted,
	store.RunFailed:    a2asdk.TaskStateFailed,
	store.RunCancelled: a2asdk.TaskStateCanceled,
}

// TaskStateForRunStatus maps a persisted loomcycle RunStatus to its A2A
// TaskState. Second return is false when the status is unknown (an
// unmapped state — caller should treat it as a bug, not default it).
func TaskStateForRunStatus(s store.RunStatus) (a2asdk.TaskState, bool) {
	ts, ok := runStatusToTaskState[s]
	return ts, ok
}

// taskStateForRejected documents the rejected→FAILED pairing required
// by the slice spec. loomcycle never persists a "rejected" RunStatus,
// so an A2A rejection (the agent refused to start a run — e.g. a
// rejected RunInput) surfaces as FAILED on the A2A side. This is a
// function rather than a runStatusToTaskState entry precisely because
// there is no RunStatus input for it; the executor calls it on a
// build-input rejection, and the statemap test asserts the pairing.
func taskStateForRejected() a2asdk.TaskState {
	return a2asdk.TaskStateFailed
}

// TaskStateInputRequired is the A2A state for a run parked awaiting
// human input (the Interruption tool). loomcycle has no distinct
// RunStatus for it (an awaiting-input run is still "running" plus a
// pending interrupt record), so it is exposed as a named target rather
// than a table entry. The detection + resume path is slice A2A-6; this
// slice publishes the mapping so that layer has one constant to bind
// to.
const TaskStateInputRequired = a2asdk.TaskStateInputRequired
