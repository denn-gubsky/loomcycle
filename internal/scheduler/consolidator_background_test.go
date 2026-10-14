package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// ctxRunner records whether each run was started under a ctx marked as
// background work.
type ctxRunner struct {
	*fakeRunner
	mu         sync.Mutex
	background []bool
}

func (r *ctxRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	r.mu.Lock()
	r.background = append(r.background, lanegate.IsBackground(ctx))
	r.mu.Unlock()
	return r.fakeRunner.RunOnce(ctx, in, cb)
}

func (r *ctxRunner) seen() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.background...)
}

// TestFanout_ConsolidationPassesRunAsBackgroundWork — a consolidation pass is
// maintenance: the ctx its run starts under is marked, so every model call the
// pass makes (the run's, its sub-agents', its Memory writes' embeddings)
// queues in its provider's lane behind the calls of live runs. A local model
// host that consolidates while people are using it must not make them wait
// their turn behind the sweep.
func TestFanout_ConsolidationPassesRunAsBackgroundWork(t *testing.T) {
	sched, fr, st, _ := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	cr := &ctxRunner{fakeRunner: fr}
	sched.runner = cr
	seedSettledSession(t, st, "", "alice")
	seedSettledSession(t, st, "", "bob")

	fireT(t, sched)

	got := cr.seen()
	if len(got) != 2 {
		t.Fatalf("RunOnce calls = %d, want 2 (one per target)", len(got))
	}
	for i, bg := range got {
		if !bg {
			t.Errorf("consolidation pass %d started under an unmarked ctx: its calls would queue ahead of live runs", i)
		}
	}
}

// TestScheduler_AnOrdinaryScheduledRunIsNotBackgroundWork — the mark is the
// consolidation sweep's, not the scheduler's: a schedule an operator wrote to
// run an agent is that operator's live work and takes its turn like any run.
func TestScheduler_AnOrdinaryScheduledRunIsNotBackgroundWork(t *testing.T) {
	enabled := true
	sched, fr, _, _, _ := schedulerFixture(t, scheduleDef{
		Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled,
	}, time.Now().Add(-time.Minute))
	cr := &ctxRunner{fakeRunner: fr}
	sched.runner = cr

	fireT(t, sched)

	got := cr.seen()
	if len(got) != 1 {
		t.Fatalf("RunOnce calls = %d, want 1", len(got))
	}
	if got[0] {
		t.Error("an ordinary scheduled run started as background work")
	}
}
