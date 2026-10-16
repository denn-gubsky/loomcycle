package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A no-trace run that waits for a run slot records only its mode, as one that
// starts at once does: the settings it resolves after the wait are written
// when it leaves the queue, and that write is filtered too.
func TestNoTrace_AQueuedRunRecordsOnlyItsMode(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)

	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds the slot")})
	_, holderRun := holder.ids(t)
	queued := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Retention: store.RetentionNone, Segments: oneUserSeg("waits")})
	_, runID := queued.ids(t)
	waitRunStatus(t, s.store, runID, store.RunQueued)

	gate <- struct{}{} // the holder ends; the queued run takes its slot
	if err := holder.wait(t); err != nil {
		t.Fatalf("holder: %v", err)
	}
	waitRunStatus(t, s.store, holderRun, store.RunCompleted)
	waitRunStatus(t, s.store, runID, store.RunRunning)
	gate <- struct{}{}
	if err := queued.wait(t); err != nil {
		t.Fatalf("queued run: %v", err)
	}
	run, err := s.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Retention != store.RetentionNone {
		t.Fatalf("retention = %q, want none", run.Retention)
	}
	if string(run.RunConfig) != string(noTraceRunConfig) {
		t.Errorf("a queued no-trace run's settings record = %s, want only its mode %s", run.RunConfig, noTraceRunConfig)
	}
}
