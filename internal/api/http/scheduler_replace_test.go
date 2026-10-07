package http

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The whole replace path, through the real server: the sweeper starts a run
// that never ends; the next slot comes due; concurrency_policy: replace
// cancels the running run through the server's cancel registry, and starts
// its own. The replaced run's row ends cancelled with the reason, and the
// schedule records it.
func TestScheduler_ReplaceCancelsTheRunningRunThroughTheServer(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}}
	srv, _ := makeServer(t, hangingProvider{}, cfg)
	st := srv.store
	ctx := context.Background()

	body, _ := json.Marshal(map[string]any{
		"agent": "writer", "schedule": "* * * * *", "enabled": true, "concurrency_policy": "replace",
		"prompt": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "trusted-text", "text": "go"}}}},
	})
	const defID = "sd_replace"
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: defID, Name: "nightly", Definition: body}); err != nil {
		t.Fatal(err)
	}
	if err := st.ScheduleDefSetActive(ctx, "", "nightly", defID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The replacing run outlives the test body: it is cancelled in cleanup,
	// and the goroutine finishing it may log after the test returns.
	var over atomic.Bool
	logf := func(format string, args ...any) {
		if !over.Load() {
			t.Logf(format, args...)
		}
	}
	sched := scheduler.New(scheduler.Config{TickInterval: 20 * time.Millisecond}, st, srv, nil, nil, logf)
	sched.SetRunCanceller(srv)
	sched.Start(ctx)
	t.Cleanup(sched.Stop)

	active := func() []store.ScheduleActiveRun {
		rows, _ := st.ScheduleActiveRunsList(ctx, defID)
		return rows
	}
	waitFor(t, "the first run to start", func() bool { return len(active()) == 1 })
	first := active()[0].RunID

	// The next slot comes due while the first run is still going.
	if err := st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the replacing run to start", func() bool {
		rows := active()
		return len(rows) == 1 && rows[0].RunID != first
	})
	second := active()[0].RunID
	t.Cleanup(func() {
		_, _ = srv.CancelScheduledRun(ctx, second, "test over")
		// Let it end before the store closes under it.
		waitFor(t, "the replacing run to end", func() bool {
			got, err := st.GetRun(ctx, second)
			return err == nil && store.IsTerminalRunStatus(got.Status)
		})
		over.Store(true)
	})

	var run store.Run
	waitFor(t, "the replaced run to end", func() bool {
		got, err := st.GetRun(ctx, first)
		run = got
		return err == nil && store.IsTerminalRunStatus(got.Status)
	})
	if run.Status != store.RunCancelled || run.StopReason != "replaced by the next slot" {
		t.Errorf("replaced run = status %s stop_reason %q, want cancelled / replaced by the next slot", run.Status, run.StopReason)
	}
	if got, err := st.GetRun(ctx, second); err != nil || got.Status != store.RunRunning {
		t.Errorf("replacing run = %+v (err %v), want running", got.Status, err)
	}
}
