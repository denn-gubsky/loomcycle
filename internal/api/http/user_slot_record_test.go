package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run's row records whether its admission took a slot in the cluster's
// shared per-user counter, on every start path, so a resume after its replica
// stopped gives back exactly that (RFC ES C4).
func TestRunOnce_RecordsWhetherItTookASharedUserSlot(t *testing.T) {
	ctx := context.Background()
	take := func(t *testing.T, s *Server, runID string) store.RunUserSlot {
		t.Helper()
		got, err := s.store.TakeRunUserSlot(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("counted", func(t *testing.T) {
		s, gate := newGatedBatchServer(t, 4)
		s.sem = concurrency.New(4, 8, 0).WithPerUserCap(2).WithUserQuotaStore(&quotaGateCounts{})
		r := startRunOnce(s, ctx, runner.RunInput{Agent: "r", UserID: "alice", Segments: oneUserSeg("go")})
		_, runID := r.ids(t)
		gate <- struct{}{}
		if err := r.wait(t); err != nil {
			t.Fatal(err)
		}
		if got := take(t, s, runID); got != store.RunUserSlotHeld {
			t.Errorf("row of a run counted against its user = %v, want held", got)
		}
	})

	t.Run("cap off", func(t *testing.T) {
		s, gate := newGatedBatchServer(t, 4)
		s.sem = concurrency.New(4, 8, 0).WithUserQuotaStore(&quotaGateCounts{})
		r := startRunOnce(s, ctx, runner.RunInput{Agent: "r", UserID: "alice", Segments: oneUserSeg("go")})
		_, runID := r.ids(t)
		gate <- struct{}{}
		if err := r.wait(t); err != nil {
			t.Fatal(err)
		}
		if got := take(t, s, runID); got != store.RunUserSlotNone {
			t.Errorf("row of a run admitted with the per-user cap off = %v, want none", got)
		}
	})

	t.Run("queued", func(t *testing.T) {
		s, gate := newGatedBatchServer(t, 1)
		s.sem = concurrency.New(1, 8, 0).WithPerUserCap(2).WithUserQuotaStore(&quotaGateCounts{})
		holder := startRunOnce(s, ctx, runner.RunInput{Agent: "r", UserID: "alice", Segments: oneUserSeg("holds")})
		holder.ids(t)
		queued := startRunOnce(s, ctx, runner.RunInput{Agent: "r", UserID: "alice", Segments: oneUserSeg("waits")})
		_, runID := queued.ids(t)
		waitRunStatus(t, s.store, runID, store.RunQueued)
		gate <- struct{}{}
		gate <- struct{}{}
		if err := holder.wait(t); err != nil {
			t.Fatal(err)
		}
		if err := queued.wait(t); err != nil {
			t.Fatal(err)
		}
		if got := take(t, s, runID); got != store.RunUserSlotHeld {
			t.Errorf("row of a queued run counted against its user = %v, want held", got)
		}
	})
}
