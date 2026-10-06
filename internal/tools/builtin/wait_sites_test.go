package builtin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// Each wait site parks the CALLER's run clock for exactly the part that
// blocks, so a run whose budget is active time (a code agent) does not spend
// it while it waits. Each test blocks the site for siteBlocks and checks the
// caller's clock counted it as waited — and, where the site can leak a wait
// to a run it no longer holds up, that it does not.

const siteBlocks = 400 * time.Millisecond

// withClock gives ctx a fresh run clock, as the loop does for a code-js run.
func withClock(ctx context.Context) (context.Context, *providers.RunClock) {
	c := providers.NewRunClock(time.Now(), providers.RunClockState{})
	return providers.WithRunClock(ctx, c), c
}

// requireWaited fails unless the clock counted about siteBlocks as waited.
func requireWaited(t *testing.T, c *providers.RunClock) {
	t.Helper()
	if got := c.State().Waited; got < siteBlocks*3/4 {
		t.Fatalf("the caller's clock counted %s as waited, want about %s — the wait was spent as active time", got, siteBlocks)
	}
}

func TestAgentSpawn_ParksTheCallersClockWhileTheChildRuns(t *testing.T) {
	ctx, clock := withClock(context.Background())
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		time.Sleep(siteBlocks)
		return "done", nil
	}}
	res, _ := a.Execute(ctx, json.RawMessage(`{"name":"kid","prompt":"go"}`))
	if res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	requireWaited(t, clock)
}

func TestAgentParallelSpawn_ParksTheCallersClockWhileTheChildrenRun(t *testing.T) {
	ctx, clock := withClock(context.Background())
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) {
		time.Sleep(siteBlocks)
		return "done", nil
	}}
	res, _ := a.Execute(ctx, json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"kid","prompt":"a"},{"name":"kid","prompt":"b"}]}`))
	if res.IsError {
		t.Fatalf("parallel_spawn: %s", res.Text)
	}
	requireWaited(t, clock)
	// Two children blocking side by side are one wait, not two.
	if got := clock.State().Waited; got > 2*siteBlocks-siteBlocks/4 {
		t.Errorf("waited %s for two concurrent children of %s each — counted twice", got, siteBlocks)
	}
}

func TestChannelAwait_ParksTheCallersClockWhileWaiting(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()
	go func() {
		time.Sleep(siteBlocks)
		mustPublish(t, tool, ctx, "c1")
	}()
	waitCtx, clock := withClock(ctx)
	res, _ := tool.Execute(waitCtx, json.RawMessage(`{"op":"await","channels":["c1"],"wait_ms":5000}`))
	if res.IsError {
		t.Fatalf("await: %s", res.Text)
	}
	if out := decodeResult(t, res.Text); out["satisfied"] != true {
		t.Fatalf("await did not see the publish: %s", res.Text)
	}
	requireWaited(t, clock)
}

func TestChannelSubscribe_ParksTheCallersClockWhileWaiting(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()
	go func() {
		time.Sleep(siteBlocks)
		mustPublish(t, tool, ctx, "c1")
	}()
	waitCtx, clock := withClock(ctx)
	res, _ := tool.Execute(waitCtx, json.RawMessage(`{"op":"subscribe","channel":"c1","wait_ms":5000}`))
	if res.IsError {
		t.Fatalf("subscribe: %s", res.Text)
	}
	if msgs, _ := decodeResult(t, res.Text)["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("subscribe did not receive the publish: %s", res.Text)
	}
	requireWaited(t, clock)
}

func TestInterruptionAsk_ParksTheCallersClockUntilAnswered(t *testing.T) {
	tool, ctx, runID, cleanup := interruptionFixture(t)
	defer cleanup()
	go func() {
		time.Sleep(siteBlocks)
		pending, err := tool.Store.InterruptListByRun(ctx, runID, store.InterruptStatusPending)
		if err != nil || len(pending) != 1 {
			t.Errorf("pending interrupts = %v, %v; want one", pending, err)
			return
		}
		id := pending[0].InterruptID
		if err := tool.Store.InterruptResolve(ctx, id, "yes", store.InterruptResolvedByWebUI, nil); err != nil {
			t.Errorf("resolve: %v", err)
			return
		}
		tool.Bus.Notify("intr:" + id)
	}()
	waitCtx, clock := withClock(ctx)
	res, _ := tool.Execute(waitCtx, json.RawMessage(`{"op":"ask","question":"proceed?","timeout_ms":5000}`))
	if res.IsError {
		t.Fatalf("ask: %s", res.Text)
	}
	requireWaited(t, clock)
}

func TestTeamDefRun_ParksTheCallersClockWhileTheTeamWorks(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) {
		time.Sleep(siteBlocks)
		return "reviewed", nil
	})
	createTeam(t, tool, ctx, "slow-team", validTeamGraph)
	waitCtx, clock := withClock(ctx)
	res, _ := tool.Execute(waitCtx, json.RawMessage(`{"op":"run","name":"slow-team","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	requireWaited(t, clock)
}

// A detached walk does not hold its caller up, so a wait inside it — here a
// member that waits on something — must not pause the caller's clock while
// the caller carries on.
func TestTeamDefRun_DetachedWalkDoesNotPauseTheCallersClock(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	ended := make(chan struct{})
	tool.WalkRun = func(c context.Context, _ WalkRunSpec) (context.Context, string, func(WalkEnd), error) {
		return context.WithoutCancel(c), "r_walk", func(WalkEnd) { close(ended) }, nil
	}
	tool.Spawn = textSpawn(func(c context.Context, _ string, _ teamrun.Prompt, _ string) (string, error) {
		endWait := providers.BeginWait(c)
		time.Sleep(siteBlocks)
		endWait()
		return "reviewed", nil
	})
	createTeam(t, tool, ctx, "detached-team", validTeamGraph)
	waitCtx, clock := withClock(ctx)
	res, _ := tool.Execute(waitCtx, json.RawMessage(`{"op":"run","name":"detached-team","input":"x","mode":"detach"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached walk never ended")
	}
	if got := clock.State().Waited; got != 0 {
		t.Fatalf("a detached walk's wait paused its caller's clock for %s", got)
	}
}
