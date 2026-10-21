package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// cancelDuringRunRunner cancels the scheduler's ctx while a pass's run is in
// flight — the process's shutdown cancelling the background context the
// scheduler ticks under — and records what that did to the run's own ctx.
type cancelDuringRunRunner struct {
	*fakeRunner
	stop      context.CancelFunc
	mu        sync.Mutex
	cancelled []bool
}

func (r *cancelDuringRunRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	r.stop()
	// AfterFunc runs its func on its own goroutine: give a cancel that would
	// reach the run time to land before judging that it did not.
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	r.mu.Lock()
	r.cancelled = append(r.cancelled, ctx.Err() != nil)
	r.mu.Unlock()
	return r.fakeRunner.RunOnce(ctx, in, cb)
}

// A consolidation pass's run outlives the scheduler's stop, as an ordinary
// scheduled run does: a shutdown parks it for the next boot to resume, and
// cancelling it there would end it cancelled after the shutdown counted it
// paused. Its budget still ends a slow pass (the fan-out escalation tests).
func TestFanout_ConsolidationPassOutlivesTheSchedulerStop(t *testing.T) {
	sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
	seedSettledSession(t, st, "", "alice")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	cr := &cancelDuringRunRunner{fakeRunner: fr, stop: stop}
	sched.runner = cr

	sched.tick(ctx)
	sched.sweeps.Wait()
	sched.runs.Wait()

	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.cancelled) != 1 {
		t.Fatalf("RunOnce calls = %d, want 1", len(cr.cancelled))
	}
	if cr.cancelled[0] {
		t.Error("stopping the scheduler cancelled the consolidation pass's run")
	}
}
