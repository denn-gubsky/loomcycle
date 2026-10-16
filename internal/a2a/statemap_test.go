package a2a

import (
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestStatemap_EveryRunStatusMapsToExpectedTaskState locks the single
// source of truth: every loomcycle RunStatus has exactly one A2A
// TaskState, and the four terminal-vs-working classifications are
// correct. A drift here is a wire-visible bug (an A2A client would see
// the wrong lifecycle state).
func TestStatemap_EveryRunStatusMapsToExpectedTaskState(t *testing.T) {
	cases := []struct {
		status   store.RunStatus
		want     a2asdk.TaskState
		terminal bool
	}{
		{store.RunRunning, a2asdk.TaskStateWorking, false},
		{store.RunCompleted, a2asdk.TaskStateCompleted, true},
		{store.RunFailed, a2asdk.TaskStateFailed, true},
		{store.RunCancelled, a2asdk.TaskStateCanceled, true},
	}
	for _, tc := range cases {
		got, ok := TaskStateForRunStatus(tc.status)
		if !ok {
			t.Fatalf("RunStatus %q is unmapped", tc.status)
		}
		if got != tc.want {
			t.Errorf("RunStatus %q → %q, want %q", tc.status, got, tc.want)
		}
		if got.Terminal() != tc.terminal {
			t.Errorf("RunStatus %q terminal=%v, want %v", tc.status, got.Terminal(), tc.terminal)
		}
	}
}

// TestStatemap_EveryStoredRunStatusIsMapped derives the cases from the
// store's own list, not a hand list: a status added there with no entry here
// made the task store answer "task not found" for a live run of that status.
// That is how a queued run (waiting for a slot) became invisible to an A2A
// peer polling its task.
func TestStatemap_EveryStoredRunStatusIsMapped(t *testing.T) {
	for _, s := range store.RunStatuses {
		if _, ok := TaskStateForRunStatus(s); !ok {
			t.Errorf("RunStatus %q has no A2A TaskState — a task of that status reads as not found", s)
		}
	}
	// Waiting for a slot, or created and not yet started, is not working:
	// both are the A2A "accepted, not yet working" state.
	for _, s := range []store.RunStatus{store.RunQueued, store.RunConfigured} {
		if got, _ := TaskStateForRunStatus(s); got != a2asdk.TaskStateSubmitted {
			t.Errorf("RunStatus %q → %q, want %q", s, got, a2asdk.TaskStateSubmitted)
		}
	}
}

// TestStatemap_UnknownRunStatusReportsUnmapped ensures the lookup
// signals "unknown" rather than silently defaulting — an unmapped state
// must surface as a bug, not as a spurious terminal state.
func TestStatemap_UnknownRunStatusReportsUnmapped(t *testing.T) {
	if got, ok := TaskStateForRunStatus(store.RunStatus("bogus")); ok {
		t.Fatalf("unknown status mapped to %q, want unmapped", got)
	}
}

// TestStatemap_RejectedMapsToFailed documents the rejected→FAILED
// invariant. loomcycle never persists a "rejected" RunStatus, so the
// A2A rejection outcome is FAILED on the loomcycle side.
func TestStatemap_RejectedMapsToFailed(t *testing.T) {
	if got := taskStateForRejected(); got != a2asdk.TaskStateFailed {
		t.Fatalf("rejected → %q, want %q", got, a2asdk.TaskStateFailed)
	}
}

// TestStatemap_InputRequiredTargetIsExposed locks the INPUT_REQUIRED
// mapping target the interruption slice (A2A-6) binds to.
func TestStatemap_InputRequiredTargetIsExposed(t *testing.T) {
	if TaskStateInputRequired != a2asdk.TaskStateInputRequired {
		t.Fatalf("input-required target = %q, want %q", TaskStateInputRequired, a2asdk.TaskStateInputRequired)
	}
}
