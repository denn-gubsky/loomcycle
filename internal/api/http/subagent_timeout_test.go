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
