package http

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// hangingProvider never answers: every call waits until its ctx ends.
type hangingProvider struct{}

func (hangingProvider) ID() string                  { return "stub" }
func (hangingProvider) Probe(context.Context) error { return nil }
func (hangingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (hangingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (hangingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func agentToolOf(t *testing.T, s *Server) *builtin.AgentTool {
	t.Helper()
	for _, tl := range s.tools {
		if a, ok := tl.(*builtin.AgentTool); ok {
			return a
		}
	}
	t.Fatal("the server wired no Agent tool")
	return nil
}

// A parallel_spawn child that never ends is reported as timed out at its
// bound, and its run row ends cancelled with the timeout as the reason.
func TestSubAgentTimeout_ChildThatNeverEndsIsCancelledAndReportedTimeout(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"child": {Model: "stub-model", SystemPrompt: "child"}}
	srv, _ := makeServer(t, hangingProvider{}, cfg)
	ctx, _ := lockedParentCtx(t, srv)

	start := time.Now()
	res, err := agentToolOf(t, srv).Execute(ctx, json.RawMessage(
		`{"op":"parallel_spawn","spawns":[{"name":"child","prompt":"x","timeout_ms":200}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the call returned after %v; the bound was 200ms", took)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatalf("envelope: %v; raw=%s", err, res.Text)
	}
	row := env.Results[0]
	if row.Ok || row.Status != "timeout" || row.RunID == "" {
		t.Fatalf("row = %+v, want ok=false status=timeout with the child's run_id", row)
	}
	run, err := srv.store.GetRun(context.Background(), row.RunID)
	if err != nil {
		t.Fatalf("child run: %v", err)
	}
	if run.Status != store.RunCancelled || !strings.Contains(run.StopReason, "timeout_ms=200") {
		t.Errorf("child run status=%q stop_reason=%q, want cancelled with the timeout as reason", run.Status, run.StopReason)
	}
}

// The child the Agent tool starts reports its review holds to the observer on
// its ctx — the clock its timeout_ms runs on — so a hold pauses the bound.
func TestSubAgentTimeout_AHeldChildReportsItsHoldToTheBound(t *testing.T) {
	h := newReviewHarness(t)
	hold := newRecordingHook(t, `{"decision":"hold","reason":"a person reads this"}`)
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "review", Phase: hooks.PhaseAgentStop, Agents: []string{"writer"}, CallbackURL: hold.srv.URL})
	ctx, events := lockedParentCtx(t, h.srv)
	var (
		mu    sync.Mutex
		holds []bool
	)
	ctx = teamrun.WithHoldObserver(ctx, func(held bool) {
		mu.Lock()
		holds = append(holds, held)
		mu.Unlock()
	})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := h.srv.runAgentToolChild(ctx, "writer", "write the plan", "")
		done <- err
	}()
	childRun := ""
	for deadline := time.Now().Add(5 * time.Second); childRun == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, ev := range events.snapshot() {
			if ev.SubagentHold != nil && ev.SubagentHold.State == providers.SubagentHoldHeld {
				childRun = ev.SubagentHold.SubagentRunID
			}
		}
	}
	if childRun == "" {
		t.Fatal("the child was never held")
	}
	if _, err := h.srv.ReviewRun(context.Background(), childRun, "approve", "", "api"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the approved child failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(holds) != 2 || !holds[0] || holds[1] {
		t.Errorf("hold reports = %v, want [true false]", holds)
	}
}

// A runtime pause does not spend a child's timeout_ms. The child here is in
// its model call when the operator pauses — it never parks — and the runtime
// stays paused for twice its bound: it is not timed out meanwhile, and once
// the runtime resumes it runs out only the time it had left.
func TestSubAgentTimeout_APausedRuntimeDoesNotSpendTheChildsBound(t *testing.T) {
	const bound = 800 * time.Millisecond
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"child": {Model: "stub-model", SystemPrompt: "child"}}
	srv, _ := makeServer(t, hangingProvider{}, cfg)
	mgr := pause.NewManager(srv.store, time.Second)
	srv.SetPauseManager(mgr)
	defer cancelAllRuns(srv)
	ctx, _ := lockedParentCtx(t, srv)

	type outcome struct {
		text    string
		isError bool
		err     error
		at      time.Time
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := agentToolOf(t, srv).Execute(ctx, json.RawMessage(`{"op":"spawn","name":"child","prompt":"x","timeout_ms":800}`))
		done <- outcome{text: res.Text, isError: res.IsError, err: err, at: time.Now()}
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := mgr.Pause(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused := time.Now()
	select {
	case o := <-done:
		t.Fatalf("the spawn returned %v after it started, into the pause: %s", o.at.Sub(start), o.text)
	case <-time.After(2 * bound):
	}
	resumed := time.Now()
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the child was never timed out after the resume")
	}
	if o.err != nil || !o.isError || !strings.Contains(o.text, "timed out: timeout_ms=800") {
		t.Fatalf("spawn = %q (error %v), want the child timed out", o.text, o.err)
	}
	left := bound - paused.Sub(start)
	if got := o.at.Sub(resumed); got < left-150*time.Millisecond || got > left+time.Second {
		t.Errorf("the child timed out %v after the resume, want about the %v it had left", got, left)
	}
}
