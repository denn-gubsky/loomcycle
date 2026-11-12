package loop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// countingPauseGate counts how often the loop asks whether to pause.
type countingPauseGate struct{ asked atomic.Int32 }

func (g *countingPauseGate) PauseRequested() bool       { g.asked.Add(1); return false }
func (g *countingPauseGate) Park(context.Context) error { return nil }

// A cancelled run stops at the top of its iteration, as a stateful run stops
// at the top of its step: no heartbeat, no pause check and no model call
// after the cancel.
func TestRun_CancelledRunStopsBeforeHeartbeatPauseAndRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prov := &endTurnProvider{}
	gate := &countingPauseGate{}
	var beats atomic.Int32
	res, err := Run(ctx, RunOptions{
		Provider:    prov,
		Model:       "x",
		Dispatcher:  tools.NewDispatcher(nil),
		Segments:    userSeg("go"),
		PauseGate:   gate,
		OnHeartbeat: func() { beats.Add(1) },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.StopReason != "cancelled" {
		t.Errorf("stop reason = %q, want cancelled", res.StopReason)
	}
	if n := prov.calls(); n != 0 {
		t.Errorf("provider called %d time(s) after the cancel, want 0", n)
	}
	if n := beats.Load(); n != 0 {
		t.Errorf("heartbeat sent %d time(s) after the cancel, want 0", n)
	}
	if n := gate.asked.Load(); n != 0 {
		t.Errorf("pause asked %d time(s) after the cancel, want 0", n)
	}
}
