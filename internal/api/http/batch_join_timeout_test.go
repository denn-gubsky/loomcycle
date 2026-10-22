package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// joinHangLimit is how long a test waits for a join that should end at a
// sub-second bound. On the unfixed code the join never ends, so the test fails
// here instead of hanging the package.
const joinHangLimit = 10 * time.Second

// heldRunServer is a batch server with a server-side join bound and one run,
// started detached under key "held", that never ends until the test releases
// it. The release is also registered as cleanup, so a join stuck on the run
// (the bug) still lets the test tear down.
func heldRunServer(t *testing.T, bound time.Duration) (*Server, connector.SpawnRunResult, chan struct{}) {
	t.Helper()
	s, gate := newGatedBatchServer(t, 8)
	s.cfg().Env.BatchJoinTimeout = bound
	held := batchOne(t, s, context.Background(), "detach", 0, keyed("held", "u1"))
	if held.Status != "running" || held.RunID == "" {
		t.Fatalf("held run = %+v, want a fresh running run", held)
	}
	t.Cleanup(func() {
		for i := 0; i < cap(gate); i++ {
			select {
			case gate <- struct{}{}:
			default:
			}
		}
		// Let the run write its end before the store closes under it.
		for i := 0; i < 100; i++ {
			if run, err := s.store.GetRun(context.Background(), held.RunID); err == nil && store.IsTerminalRunStatus(run.Status) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return s, held, gate
}

// joinWithin runs one batch call and fails the test if it has not returned
// within joinHangLimit.
func joinWithin(t *testing.T, call func() connector.BatchSpawnResult) (connector.BatchSpawnResult, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan connector.BatchSpawnResult, 1)
	go func() { done <- call() }()
	select {
	case res := <-done:
		return res, time.Since(start)
	case <-time.After(joinHangLimit):
		t.Fatalf("the join did not return within %s: it waits on a run that never ends", joinHangLimit)
		return connector.BatchSpawnResult{}, 0
	}
}

// assertHeldRunUntouched checks the held run is still running: the join that
// gave up on it did not start it, so it must not have ended it either.
func assertHeldRunUntouched(t *testing.T, s *Server, held connector.SpawnRunResult) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	run, err := s.store.GetRun(context.Background(), held.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunRunning {
		t.Fatalf("the held run is %q after the join gave up on it, want it still running", run.Status)
	}
}

// assertJoinedUnfinished checks a child the join stopped waiting for: the held
// run's ids, still running, deduplicated, and saying why it has no result.
func assertJoinedUnfinished(t *testing.T, r connector.SpawnRunResult, held connector.SpawnRunResult) {
	t.Helper()
	if !r.Deduplicated || r.RunID != held.RunID || r.SessionID != held.SessionID || r.Status != "running" {
		t.Fatalf("child = %+v, want the held run %s reported running, deduplicated", r, held.RunID)
	}
	if !strings.Contains(r.Error, "join timed out") {
		t.Errorf("child error = %q, want it to say the join timed out", r.Error)
	}
}

// A join keyed to a run that never ends returns at the server's bound, with
// that child reported running and the run itself left alone.
func TestSpawnRunBatch_JoinOnAHeldKeyEndsAtTheServerBound(t *testing.T) {
	s, held, gate := heldRunServer(t, 300*time.Millisecond)

	res, took := joinWithin(t, func() connector.BatchSpawnResult {
		res, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{keyed("held", "u1")}})
		if err != nil {
			t.Errorf("SpawnRunBatch: %v", err)
		}
		return res
	})
	if took < 300*time.Millisecond {
		t.Errorf("the join returned after %s, before its 300ms bound", took)
	}
	assertJoinedUnfinished(t, res.Results[0], held)
	assertHeldRunUntouched(t, s, held)

	gate <- struct{}{}
	waitRunStatus(t, s.store, held.RunID, store.RunCompleted)
}

// The request's own timeout_ms is the bound when it gives one, in place of the
// server's.
func TestSpawnRunBatch_TimeoutMSReplacesTheServerBound(t *testing.T) {
	s, held, _ := heldRunServer(t, time.Hour)
	res, took := joinWithin(t, func() connector.BatchSpawnResult {
		res, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{TimeoutMS: 200, Spawns: []connector.SpawnRunRequest{keyed("held", "u1")}})
		if err != nil {
			t.Errorf("SpawnRunBatch: %v", err)
		}
		return res
	})
	if took > 5*time.Second {
		t.Errorf("the join took %s, want it to end at its 200ms timeout_ms", took)
	}
	assertJoinedUnfinished(t, res.Results[0], held)
	assertHeldRunUntouched(t, s, held)
}

// A caller whose deadline is shorter than the server's bound ends the join at
// its deadline, and the held run goes on.
func TestSpawnRunBatch_ACallerDeadlineEndsTheJoinBeforeTheBound(t *testing.T) {
	s, held, _ := heldRunServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, took := joinWithin(t, func() connector.BatchSpawnResult {
		res, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{keyed("held", "u1")}})
		if err != nil {
			t.Errorf("SpawnRunBatch: %v", err)
		}
		return res
	})
	if took > 5*time.Second {
		t.Errorf("the join took %s, want it to end at the caller's 200ms deadline", took)
	}
	r := res.Results[0]
	if !r.Deduplicated || r.RunID != held.RunID || r.Status != "running" {
		t.Fatalf("child = %+v, want the held run reported running", r)
	}
	assertHeldRunUntouched(t, s, held)
}

// At the server's bound the join only stops waiting: a fresh child still
// going comes back running, with its ids, and goes on after the call — even
// once the caller's ctx ends, as a transport's does when the call returns —
// to complete as it would have.
func TestSpawnRunBatch_AFreshChildOutlivesTheServerBound(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	s.cfg().Env.BatchJoinTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		select {
		case gate <- struct{}{}:
		default:
		}
	})
	callerCtx, callerDone := context.WithCancel(context.Background())
	defer callerDone()

	res, _ := joinWithin(t, func() connector.BatchSpawnResult {
		res, err := s.SpawnRunBatch(callerCtx, connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
			{Agent: "r", UserID: "u1", Segments: oneUserSeg("a")},
		}})
		if err != nil {
			t.Errorf("SpawnRunBatch: %v", err)
		}
		return res
	})
	callerDone() // the transport's ctx ends with the call

	r := res.Results[0]
	if r.Status != "running" || r.RunID == "" || r.SessionID == "" || r.AgentID == "" || r.Deduplicated {
		t.Fatalf("child = %+v, want this batch's run reported running with its ids", r)
	}
	if !strings.Contains(r.Error, "join timed out") || !strings.Contains(r.Error, "goes on") {
		t.Errorf("child error = %q, want it to say the join timed out and the run goes on", r.Error)
	}
	time.Sleep(100 * time.Millisecond)
	if run, err := s.store.GetRun(context.Background(), r.RunID); err != nil || run.Status != store.RunRunning {
		t.Fatalf("the run is %q (err %v) after the join stopped waiting, want it still running", run.Status, err)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, r.RunID, store.RunCompleted)
}

// A caller that leaves while the join still waits ends a fresh child as it
// always did — the server's bound changes only what happens when the bound
// itself passes first.
func TestSpawnRunBatch_ACallerThatLeavesStillEndsAFreshChild(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	s.cfg().Env.BatchJoinTimeout = time.Hour
	t.Cleanup(func() {
		select {
		case gate <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	res, _ := joinWithin(t, func() connector.BatchSpawnResult {
		res, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
			{Agent: "r", UserID: "u1", Segments: oneUserSeg("a")},
		}})
		if err != nil {
			t.Errorf("SpawnRunBatch: %v", err)
		}
		return res
	})
	r := res.Results[0]
	if r.RunID == "" || strings.Contains(r.Error, "join timed out") {
		t.Fatalf("child = %+v, want the run the caller's departure ended, not a join timeout", r)
	}
	run, err := s.store.GetRun(context.Background(), r.RunID)
	if err != nil || !store.IsTerminalRunStatus(run.Status) {
		t.Fatalf("the run is %q (err %v) after its caller left, want it ended", run.Status, err)
	}
}

// An explicit timeout_ms is the caller's own deadline on its runs: a fresh
// child still going is cancelled — row and envelope — and says so, while a
// child that finished in time is reported as it always was.
func TestSpawnRunBatch_TimeoutMSCancelsAFreshChildAndSaysSo(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	s.cfg().Env.BatchJoinTimeout = time.Hour
	gate <- struct{}{} // exactly one of the two children may finish
	t.Cleanup(func() {
		select {
		case gate <- struct{}{}:
		default:
		}
	})

	res, _ := joinWithin(t, func() connector.BatchSpawnResult {
		res, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{TimeoutMS: 300, Spawns: []connector.SpawnRunRequest{
			{Agent: "r", UserID: "u1", Segments: oneUserSeg("a")},
			{Agent: "r", UserID: "u1", Segments: oneUserSeg("b")},
		}})
		if err != nil {
			t.Errorf("SpawnRunBatch: %v", err)
		}
		return res
	})
	var finished, stopped *connector.SpawnRunResult
	for i := range res.Results {
		switch res.Results[i].Status {
		case "completed":
			finished = &res.Results[i]
		case "cancelled":
			stopped = &res.Results[i]
		}
	}
	if finished == nil || stopped == nil {
		t.Fatalf("results = %+v, want one completed and one cancelled at the timeout", res.Results)
	}
	if finished.Error != "" || finished.FinalText != "partial" {
		t.Errorf("the finished child = %+v, want its answer and no error", *finished)
	}
	if stopped.RunID == "" || stopped.SessionID == "" || !strings.Contains(stopped.Error, "join timed out") {
		t.Errorf("the stopped child = %+v, want its ids and an error saying the join timed out", *stopped)
	}
	waitRunStatus(t, s.store, stopped.RunID, store.RunCancelled)
}

// A batch whose children all finish inside the bound is answered as before:
// every child completed, none carrying an error.
func TestSpawnRunBatch_ABatchThatFinishesInsideTheBoundIsUnchanged(t *testing.T) {
	s := newBatchTestServer(t, &latchProvider{dwell: 5 * time.Millisecond}, 8)
	s.cfg().Env.BatchJoinTimeout = 10 * time.Second
	res, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
		{Agent: "r", Segments: oneUserSeg("a")},
		{Agent: "r", Segments: oneUserSeg("b")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Results {
		if r.Status != "completed" || r.Error != "" || r.RunID == "" || r.FinalText != "ok" {
			t.Errorf("results[%d] = %+v, want a completed run with its answer and no error", i, r)
		}
	}
}

// POST /v1/runs:batch: the same bound, the same envelope.
func TestRunsBatch_HTTPJoinOnAHeldKeyEndsAtTheServerBound(t *testing.T) {
	s, held, _ := heldRunServer(t, 300*time.Millisecond)
	body := `{"spawns":[{"agent":"r","user_id":"u1","idempotency_key":"held","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}]}`
	res, _ := joinWithin(t, func() connector.BatchSpawnResult {
		rec := doJSON(t, s, "POST", "/v1/runs:batch", body)
		var got connector.BatchSpawnResult
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
			t.Errorf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		return got
	})
	if len(res.Results) != 1 {
		t.Fatalf("results = %+v", res.Results)
	}
	assertJoinedUnfinished(t, res.Results[0], held)
	assertHeldRunUntouched(t, s, held)
}

// MCP spawn_runs over the real connector: the same bound, the same envelope.
func TestMCPSpawnRuns_JoinOnAHeldKeyEndsAtTheServerBound(t *testing.T) {
	s, held, _ := heldRunServer(t, 300*time.Millisecond)
	mcpSrv := lcmcp.New(lcmcp.Config{Connector: s, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_runs","arguments":{"spawns":[{"agent":"r","user_id":"u1","idempotency_key":"held","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}]}}}` + "\n"
	res, _ := joinWithin(t, func() connector.BatchSpawnResult {
		var stdout bytes.Buffer
		if err := mcpSrv.Serve(context.Background(), strings.NewReader(in), &stdout); err != nil {
			t.Errorf("Serve: %v", err)
		}
		var resp loommcp.Response
		var tool loommcp.CallToolResult
		var got connector.BatchSpawnResult
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
			t.Errorf("decode response %q: %v", stdout.String(), err)
			return got
		}
		if err := json.Unmarshal(resp.Result, &tool); err != nil || tool.IsError || len(tool.Content) == 0 {
			t.Errorf("tool result = %s (%v)", resp.Result, err)
			return got
		}
		if err := json.Unmarshal([]byte(tool.Content[0].Text), &got); err != nil {
			t.Errorf("decode envelope %q: %v", tool.Content[0].Text, err)
		}
		return got
	})
	if len(res.Results) != 1 {
		t.Fatalf("results = %+v", res.Results)
	}
	assertJoinedUnfinished(t, res.Results[0], held)
	assertHeldRunUntouched(t, s, held)
}
