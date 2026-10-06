// Package pause implements the v0.8.17 runtime-wide pause/resume protocol.
//
// The substrate's quiesce primitive. Operators issue POST /v1/runtime/pause
// and every in-flight run reaches a clean iteration boundary; new /v1/runs
// requests return 503 with Retry-After until POST /v1/runtime/resume.
// Designed to close three production pain shapes: provider rate-limit waits,
// pre-backup database quiesce, and experiment migration between machines.
// See doc-internal/rfcs/pause-resume-snapshot.md for the locked design.
//
// Package shape:
//   - state.go     — RuntimeState enum + transition rules.
//   - manager.go   — the Manager type; one per server; exposes Pause/Resume,
//     PauseCh() for loop.Run to check at iteration boundary, and the
//     in-flight run registry the pause barrier waits on.
//
// Pause cancels nothing in flight: a run finishes the model call or tool
// calls it is in and parks at its next iteration boundary.
package pause

import (
	"fmt"
	"sync/atomic"
)

// RuntimeState is the discrete state the runtime exposes via
// GET /v1/runtime/state. Stored atomically inside Manager so concurrent
// readers (HTTP handlers + the loop's boundary check) don't need a lock
// to inspect the current value.
//
// Transitions are linear, NOT a state machine with loops:
//
//	StateRunning  → StatePausing  → StatePaused
//	StatePaused   → StateRunning  (resume — skips through pausing)
//
// StatePausing is the in-flight transition where the manager is waiting
// for in-flight runs to finish their current model or tool calls and park
// at their next iteration boundary (nothing is cancelled). The state DOES NOT block new
// /v1/runs at this point; 503 starts at StatePausing already, so new
// requests are refused the moment pause is declared.
//
// The int32 backing matches what atomic.Int32.Store accepts. Constants
// are stable wire values — operators / SSE consumers / dashboards
// depend on the string form via String().
type RuntimeState int32

const (
	// StateRunning is the default. The runtime accepts new runs, the
	// loop's iteration boundary check is a no-op, and no resume is
	// needed. Both fresh boots and post-resume converge to this state.
	StateRunning RuntimeState = iota
	// StatePausing is the operator-issued-pause-in-flight state.
	// Already-running runs proceed to their iteration boundary; new
	// /v1/runs requests get 503. Manager transitions out of this state
	// to StatePaused once every in-flight run has committed
	// pause_state='paused', or the pause timeout elapsed (a run still
	// working then parks at its next boundary).
	StatePausing
	// StatePaused is the at-rest paused state. No runs are
	// progressing past a boundary; a run that was still working when the
	// pause timeout elapsed parks at its next one.
	// New /v1/runs continue to return 503. Operator calls
	// POST /v1/runtime/resume to transition back to StateRunning.
	StatePaused
)

// String returns the wire-stable lowercase name. Used in HTTP responses,
// SSE events, log lines, and Web UI display. DO NOT change these strings
// without bumping a major API version — operators alerting on the
// string form would silently break.
func (s RuntimeState) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StatePausing:
		return "pausing"
	case StatePaused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", int32(s))
	}
}

// AcceptsNewRuns reports whether the runtime should accept a new
// /v1/runs or /v1/sessions/{id}/messages request. Only StateRunning
// returns true — both StatePausing and StatePaused refuse new work.
// Returning 503 from StatePausing (not just StatePaused) prevents
// unbounded queue growth during the wind-down window.
func (s RuntimeState) AcceptsNewRuns() bool {
	return s == StateRunning
}

// loadState is a helper for tests + Manager that wraps the atomic load
// in the typed conversion. Cheaper than holding the manager's mutex
// for read-only state checks on the hot path.
func loadState(a *atomic.Int32) RuntimeState {
	return RuntimeState(a.Load())
}
