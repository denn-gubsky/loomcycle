package store

import "testing"

// RunDeletable names what may be deleted and refuses everything else: a state
// it does not know, or cannot read, is live.
func TestRunDeletable_FailsClosedOnAnyStateItDoesNotKnow(t *testing.T) {
	for _, st := range append([]RunStatus{RunConfigured}, TerminalRunStatuses...) {
		if !RunDeletable(string(st), PauseStateRunning) {
			t.Errorf("status %q at rest should be deletable", st)
		}
		for _, pause := range []string{PauseStatePaused, PauseStatePausing, "", "half-paused", "RUNNING"} {
			if RunDeletable(string(st), pause) {
				t.Errorf("status %q with pause state %q must not be deletable", st, pause)
			}
		}
	}
	for _, status := range []string{string(RunRunning), "", "zombie", "Completed", "awaiting_input"} {
		if RunDeletable(status, PauseStateRunning) {
			t.Errorf("status %q must not be deletable", status)
		}
	}
}
