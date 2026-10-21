package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
)

// A shutdown stops the armed turn of an interactive run that has not parked
// in time — with the turn-cancel cause the loop parks on, naming the shutdown —
// so the run parks waiting for its next message rather than being left inside
// its model call.
func TestShutdownPause_StopsTheTurnOfAnUnparkedInteractiveRun(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	mgr := pause.NewManager(srv.store, time.Second)
	srv.SetPauseManager(mgr)
	runID, _ := liveInteractiveRun(t, srv)
	mgr.RegisterRun(runID)
	defer mgr.DeregisterRun(runID)
	turn, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	srv.turnCancelReg.Arm(runID, stop)

	_ = mgr.PauseForShutdown(200 * time.Millisecond)

	if turn.Err() == nil {
		t.Fatal("the unparked interactive run's turn was not stopped")
	}
	cause := context.Cause(turn)
	if !errors.Is(cause, loop.ErrTurnCancelled) || !strings.Contains(cause.Error(), "shutting down") {
		t.Errorf("cause = %v, want a turn cancel naming the shutdown", cause)
	}
}
