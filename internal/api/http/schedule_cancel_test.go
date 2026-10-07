package http

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// CancelScheduledRun is what a concurrency_policy: replace slot stops the
// schedule's running run through. It reaches an agent run through the cancel
// registry and a walk through its own cancel, reports a run that already
// ended as stopped, and reports a walk it cannot reach as NOT stopped — so the
// scheduler never starts a walk over one it did not stop.
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

	t.Run("a walk not live here, with no route to its replica, is not stopped", func(t *testing.T) {
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

// A replace slot claimed on one replica stops a team walk running on ANOTHER:
// the cancel is routed to the walk's replica, whose walk table holds its
// cancel. The walk ends with the slot's reason.
//
// Fails-before: a walk not live on the calling replica was reported "not
// stopped" without trying, so replace skipped the slot as forbid would.
func TestCancelScheduledRun_StopsAWalkOnAnotherReplica(t *testing.T) {
	walkOn := func(t *testing.T, routed bool) (b *Server, runID string, cause func() error) {
		t.Helper()
		a, b := twoReplicas(t, hangingProvider{}, makeBaseConfig(), routed)
		runID = seedRunInTenant(t, a.store, "acme", "alice", teamWalkAgentPrefix+"weekly")
		var got atomic.Value
		a.walks.add(runID, "", runStateMeta{RunID: runID}, func(err error) { got.Store(err) })
		t.Cleanup(func() { a.walks.remove(runID) })
		return b, runID, func() error {
			err, _ := got.Load().(error)
			return err
		}
	}

	t.Run("routed to the walk's replica", func(t *testing.T) {
		b, runID, cause := walkOn(t, true)
		stopped, err := b.CancelScheduledRun(context.Background(), runID, "replaced by the next slot")
		if err != nil || !stopped {
			t.Fatalf("stopped=%v err=%v, want the walk stopped through its replica", stopped, err)
		}
		if got := cause(); got == nil || cancel.ReasonFromCause(got) != "replaced by the next slot" {
			t.Errorf("the walk's cancel cause = %v, want the slot's reason", got)
		}
	})

	t.Run("no route: not stopped, and not cancelled", func(t *testing.T) {
		b, runID, cause := walkOn(t, false)
		stopped, err := b.CancelScheduledRun(context.Background(), runID, "replaced by the next slot")
		if err != nil || stopped {
			t.Errorf("stopped=%v err=%v, want false", stopped, err)
		}
		if got := cause(); got != nil {
			t.Errorf("the walk was cancelled with no route to it: %v", got)
		}
	})
}
