package main

import (
	"context"
	"testing"
)

// The cluster resume sweep does nothing while the runtime is paused — an
// operator's cluster pause, or this replica's own shutdown — when a resumed run
// would only park again; otherwise it runs the resume pass.
func TestResumeOrphanedPausedRuns_SkipsWhilePaused(t *testing.T) {
	calls := 0
	resume := func(context.Context) (int, []string) { calls++; return 0, nil }

	resumeOrphanedPausedRuns(context.Background(), func() bool { return true }, resume)
	if calls != 0 {
		t.Errorf("the pass ran %d time(s) while paused, want 0", calls)
	}
	resumeOrphanedPausedRuns(context.Background(), func() bool { return false }, resume)
	if calls != 1 {
		t.Errorf("the pass ran %d time(s) while running, want 1", calls)
	}
}
