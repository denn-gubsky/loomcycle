package scheduler

import (
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// A scheduled run is counted under the schedule trigger, whatever ctx the
// tick ran on.
func TestScheduler_RunStartsUnderTheScheduleTrigger(t *testing.T) {
	sched, sr, _, _ := trackedFixture(t, channelHookDef("done-hook"))
	fireT(t, sched)
	_, ctxs := sr.started()
	if len(ctxs) != 1 {
		t.Fatalf("runs = %d, want 1", len(ctxs))
	}
	if got := lcotel.RunTriggerFromContext(ctxs[0]); got != lcotel.TriggerSchedule {
		t.Errorf("trigger = %q, want %q", got, lcotel.TriggerSchedule)
	}
}
