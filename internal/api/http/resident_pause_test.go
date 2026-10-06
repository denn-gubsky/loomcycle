package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/pause"
)

// A runtime pause cancels nothing in flight: the sweeper reaps no resident
// child while the runtime is paused, however long the pause, and the pause
// does not count as idle time once it lifts. The idle clock then runs on
// from where the pause stopped it.
func TestResidentChild_ARuntimePauseIsNotIdleTime(t *testing.T) {
	srv := newResidentTestServer(t)
	srv.SetPauseManager(pause.NewManager(srv.store, 2*time.Second))
	ctx := residentParentCtx("parent-agent", "")
	runID, _, state, err := srv.openResidentChild(ctx, "child", "start", "", 0, 0)
	if err != nil || state != "awaiting_input" {
		t.Fatalf("open: state=%q err=%v", state, err)
	}
	defer func() { _ = srv.closeResidentChild(ctx, runID) }()
	ttl := srv.residentChildIdleTTL()
	alive := func(when string) {
		t.Helper()
		rc, ok := srv.residentReg.get(runID)
		if !ok || rc.reaped() != "" {
			t.Fatalf("the resident child was reaped %s (%v)", when, rc.reaped())
		}
	}

	if _, err := srv.pauseMgr.Pause(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	pausedAt := time.Now()
	srv.sweepResidentChildren(pausedAt)
	pauseEnds := pausedAt.Add(ttl + time.Minute) // a pause longer than the idle TTL
	srv.sweepResidentChildren(pauseEnds)
	alive("while the runtime was paused")

	if _, err := srv.pauseMgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	srv.sweepResidentChildren(pauseEnds.Add(time.Second))
	alive("at the first sweep after the pause lifted")

	// The idle clock runs again: a TTL past the pause, the child is idle.
	srv.sweepResidentChildren(pauseEnds.Add(ttl + time.Minute))
	waitResidentGone(t, srv, runID)
}

// The pause comes off both clocks: a turn running when the runtime paused is
// not nearer its ceiling for the pause, and a clock that moved during the
// pause (a turn that ended in it) restarts when the pause lifts.
func TestResidentChild_APauseComesOffBothClocks(t *testing.T) {
	since := time.Now()
	now := since.Add(time.Hour)
	rc := &residentChild{lastUsed: since.Add(-10 * time.Minute), turnStarted: since.Add(-20 * time.Minute)}
	rc.discountPause(since, now)
	if want := now.Add(-10 * time.Minute); !rc.lastUsed.Equal(want) {
		t.Errorf("idle clock = %s after the pause, want %s", rc.lastUsed.Sub(now), want.Sub(now))
	}
	if want := now.Add(-20 * time.Minute); !rc.turnStarted.Equal(want) {
		t.Errorf("turn clock = %s after the pause, want %s", rc.turnStarted.Sub(now), want.Sub(now))
	}
	rc = &residentChild{lastUsed: since.Add(5 * time.Minute)}
	rc.discountPause(since, now)
	if !rc.lastUsed.Equal(now) {
		t.Errorf("a clock moved during the pause = %s, want it restarted when the pause lifted", rc.lastUsed.Sub(now))
	}
}
