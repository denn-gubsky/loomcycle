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
// scheduler ticks under — and records whether that reached the run's ctx.
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

// A consolidation pass's run ends with the scheduler's stop, unlike an
// ordinary scheduled run: the pass runs inside an advisory lock that pins a
// pool connection until it returns, and a run that outlived a shutdown would
// hold the pool's close until the SIGKILL. The next tick does the pass again.
func TestFanout_ConsolidationPassEndsWithTheSchedulerStop(t *testing.T) {
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
	if !cr.cancelled[0] {
		t.Error("stopping the scheduler left the consolidation pass's run going: it would hold its advisory lock's connection")
	}
}
