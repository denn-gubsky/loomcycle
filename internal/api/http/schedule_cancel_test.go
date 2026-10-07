package http

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// CancelScheduledRun is what a concurrency_policy: replace slot stops the
// schedule's running run through. It reaches an agent run through the cancel
// registry and a walk through its own cancel, reports a run that already
// ended as stopped, and reports a walk it cannot reach as NOT stopped — so the
// scheduler never starts a walk over one still running elsewhere.
func TestCancelScheduledRun(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()

	t.Run("an agent run, through the cancel registry", func(t *testing.T) {
		runID := seedRunInTenant(t, st, "acme", "alice", "a_sched_live")
		wasCancelled := registerLiveCancel(t, s, runID, "a_sched_live", "alice")
		stopped, err := s.CancelScheduledRun(ctx, runID, "replaced by the next slot")
		if err != nil || !stopped || !wasCancelled() {
			t.Errorf("stopped=%v err=%v cancelled=%v, want the run cancelled", stopped, err, wasCancelled())
		}
	})

	t.Run("a walk on this replica, through its own cancel", func(t *testing.T) {
		runID := seedRunInTenant(t, st, "acme", "alice", teamWalkAgentPrefix+"weekly")
		var cancelled atomic.Bool
		s.walks.add(runID, "", runStateMeta{RunID: runID}, func(error) { cancelled.Store(true) })
		t.Cleanup(func() { s.walks.remove(runID) })
		stopped, err := s.CancelScheduledRun(ctx, runID, "replaced by the next slot")
		if err != nil || !stopped || !cancelled.Load() {
			t.Errorf("stopped=%v err=%v cancelled=%v, want the walk cancelled", stopped, err, cancelled.Load())
		}
	})

	t.Run("a walk running on another replica is not stopped", func(t *testing.T) {
		runID := seedRunInTenant(t, st, "acme", "alice", teamWalkAgentPrefix+"elsewhere")
		stopped, err := s.CancelScheduledRun(ctx, runID, "replaced by the next slot")
		if err != nil || stopped {
			t.Errorf("stopped=%v err=%v, want false — nothing here can stop it", stopped, err)
		}
	})

	t.Run("a run that already ended counts as stopped", func(t *testing.T) {
		runID := seedRunInTenant(t, st, "acme", "alice", "a_sched_done")
		if err := st.FinishRun(ctx, runID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
			t.Fatal(err)
		}
		if stopped, err := s.CancelScheduledRun(ctx, runID, "x"); err != nil || !stopped {
			t.Errorf("stopped=%v err=%v, want true", stopped, err)
		}
		if stopped, err := s.CancelScheduledRun(ctx, "r_gone", "x"); err != nil || !stopped {
			t.Errorf("a run that is gone: stopped=%v err=%v, want true", stopped, err)
		}
	})
}
