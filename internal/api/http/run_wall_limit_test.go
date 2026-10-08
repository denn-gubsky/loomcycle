package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func waitRunEndedWithin(t *testing.T, st store.Store, runID string, within time.Duration) store.Run {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		run, err := st.GetRun(context.Background(), runID)
		if err == nil && store.IsTerminalRunStatus(run.Status) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s is %q after %s (err %v), want it ended", runID, run.Status, within, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The request's done-when: a detached run with max_wall_seconds is cancelled
// when the limit runs out, and its row says why.
func TestMaxWallSeconds_ADetachedRunIsCancelledAtItsLimit(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4) // the gate is never opened: the run would wait forever
	req := connector.SpawnRunRequest{Agent: "r", UserID: "u1", Segments: oneUserSeg("go"), MaxWallSeconds: 1}
	start := time.Now()
	res := batchOne(t, s, context.Background(), "detach", 0, req)
	if res.Status != "running" || res.RunID == "" {
		t.Fatalf("detach = %+v, want a running child", res)
	}
	run := waitRunEndedWithin(t, s.store, res.RunID, 6*time.Second)
	if lived := time.Since(start); lived < 900*time.Millisecond {
		t.Errorf("the run ended after %s, before its 1s limit", lived)
	}
	if run.Status != store.RunCancelled || run.StopReason != WallLimitReason {
		t.Errorf("run ended %q with stop_reason %q (error %q); want cancelled / %s", run.Status, run.StopReason, run.ErrorMsg, WallLimitReason)
	}
	// The limit is part of the run's spec: a resume is still bound by it.
	var spec struct {
		MaxWallSeconds int `json:"max_wall_seconds"`
	}
	if err := json.Unmarshal(run.RunConfig, &spec); err != nil || spec.MaxWallSeconds != 1 {
		t.Errorf("run_config = %s (err %v), want max_wall_seconds 1 recorded", run.RunConfig, err)
	}
	if n := s.cancelReg.Count(); n != 0 {
		t.Errorf("%d run(s) still registered after the limit ended the run", n)
	}
}

// A joined run reports the same end to its caller.
func TestMaxWallSeconds_AJoinedRunReportsTheLimit(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4)
	res := batchOne(t, s, context.Background(), "join", 0,
		connector.SpawnRunRequest{Agent: "r", Segments: oneUserSeg("go"), MaxWallSeconds: 1})
	if res.Status != "cancelled" || res.StopReason != WallLimitReason {
		t.Errorf("join = %+v, want status cancelled with stop_reason %s", res, WallLimitReason)
	}
	if run, err := s.store.GetRun(context.Background(), res.RunID); err != nil || run.StopReason != WallLimitReason {
		t.Errorf("row stop_reason = %q (err %v), want %s", run.StopReason, err, WallLimitReason)
	}
}

// POST /v1/runs builds its run without RunOnce, so it carries the limit on
// its own.
func TestMaxWallSeconds_BoundsARunStartedOverHTTP(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4)
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	status, body := postKeyedRun(t, ts.URL,
		`{"agent":"r","agent_id":"a_walled","max_wall_seconds":1,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	run, err := s.store.GetRunByAgentID(context.Background(), "a_walled")
	if err != nil {
		t.Fatal(err)
	}
	run = waitRunEndedWithin(t, s.store, run.ID, 6*time.Second)
	if run.Status != store.RunCancelled || run.StopReason != WallLimitReason {
		t.Errorf("run ended %q / %q, want cancelled / %s; stream: %s", run.Status, run.StopReason, WallLimitReason, body)
	}
}

// A run that finishes inside its limit is untouched by it.
func TestMaxWallSeconds_ARunThatFinishesInTimeCompletes(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	gate <- struct{}{}
	res := batchOne(t, s, context.Background(), "join", 0,
		connector.SpawnRunRequest{Agent: "r", Segments: oneUserSeg("go"), MaxWallSeconds: 30})
	if res.Status != "completed" || res.FinalText != "partial" {
		t.Errorf("join = %+v, want the run completed with its answer", res)
	}
}

func TestMaxWallSeconds_OutOfRangeIsRefused(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4)
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	ctx := context.Background()
	for _, n := range []int{-1, connector.MaxWallSecondsCeiling + 1} {
		_, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Mode: "detach",
			Spawns: []connector.SpawnRunRequest{{Agent: "r", Segments: oneUserSeg("go"), MaxWallSeconds: n}}})
		if err == nil || !strings.Contains(err.Error(), "max_wall_seconds must be between") {
			t.Errorf("batch with max_wall_seconds %d: err = %v, want a refusal", n, err)
		}
		res, err := s.SpawnRun(ctx, connector.SpawnRunRequest{Agent: "r", Segments: oneUserSeg("go"), MaxWallSeconds: n})
		if err != nil || res.Status != "failed" || !strings.Contains(res.Error, "max_wall_seconds") {
			t.Errorf("spawn with max_wall_seconds %d = %+v, %v; want a refusal", n, res, err)
		}
	}
	status, body := postKeyedRun(t, ts.URL,
		`{"agent":"r","max_wall_seconds":-5,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "max_wall_seconds") {
		t.Errorf("POST /v1/runs with a negative limit = %d %s, want 400", status, body)
	}
	if _, ok := connector.ValidateMaxWallSeconds(connector.MaxWallSecondsCeiling); !ok {
		t.Error("the ceiling itself was refused")
	}
}

// A detached batch still refuses timeout_ms, and now says what to use.
func TestSpawnRunBatch_DetachWithTimeoutPointsAtMaxWallSeconds(t *testing.T) {
	s, _ := newGatedBatchServer(t, 4)
	_, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{Mode: "detach", TimeoutMS: 1000,
		Spawns: []connector.SpawnRunRequest{{Agent: "r", Segments: oneUserSeg("go")}}})
	if err == nil || !strings.Contains(err.Error(), "max_wall_seconds") {
		t.Errorf("err = %v, want the refusal to name max_wall_seconds", err)
	}
}

// A run resumed after a restart is charged the lifetime it already used: what
// its clock recorded when it parked, or, with no record, the time since it
// started.
func TestRunConfigRecord_ClockCarryFromChargesAnUnrecordedRunItsAge(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)

	walled := runConfigRecord{MaxWallSeconds: 120}
	if got := walled.clockCarryFrom(started).Wall; got < 89*time.Second || got > 95*time.Second {
		t.Errorf("a run with a limit and no recorded clock carries %s, want about 90s", got)
	}
	recorded := runConfigRecord{MaxWallSeconds: 120, Clock: &runClockRecord{WallMs: 5000}}
	if got := recorded.clockCarryFrom(started).Wall; got != 5*time.Second {
		t.Errorf("a recorded clock carries %s, want the 5s it recorded", got)
	}
	unbounded := runConfigRecord{}
	if got := unbounded.clockCarryFrom(started).Wall; got != 0 {
		t.Errorf("a run with no limit carries %s, want nothing", got)
	}
}
