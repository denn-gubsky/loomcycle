package teamrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// stuckForever is how long a fake member runs when nothing cancels it. Long
// enough that finishing on its own cannot pass for a timeout, short enough that
// a missing timeout fails the test instead of hanging it.
const stuckForever = 3 * time.Second

// blockUntilCancelled is a member that never finishes its work: it returns only
// when its run is cancelled (or, with no timeout at all, after stuckForever).
func blockUntilCancelled(ctx context.Context) (SpawnResult, error) {
	select {
	case <-ctx.Done():
		return SpawnResult{RunID: "r_stuck", Status: "cancelled"}, ctx.Err()
	case <-time.After(stuckForever):
		return SpawnResult{Output: "finished late", RunID: "r_stuck", Status: "completed"}, nil
	}
}

const timedAgentJSON = `{
  "entry":"work",
  "states":[
    {"state":"work","handler":{"kind":"agent","agent":"slow","timeout_ms":50}},
    {"state":"after","handler":{"kind":"agent","agent":"next"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"work","to":"after","on":"success"},
    {"from":"after","to":"done","on":"success"}
  ]}`

// An agent state whose agent never returns fails once timeout_ms has passed:
// its run is cancelled, the reason names the state and the timeout, and the
// walk ends there as it does for any handler failure — the next state never
// runs.
func TestHandlerTimeout_AgentThatNeverReturnsFailsTheStatePromptly(t *testing.T) {
	d := mustParse(t, timedAgentJSON)
	var ran []string
	var mu sync.Mutex
	var cancelled atomic.Bool
	spawn := func(ctx context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
		mu.Lock()
		ran = append(ran, agent)
		mu.Unlock()
		if agent == "slow" {
			sp, err := blockUntilCancelled(ctx)
			cancelled.Store(ctx.Err() != nil)
			return sp, err
		}
		return SpawnResult{Output: "ok"}, nil
	}

	task := &Task{Input: "go"}
	start := time.Now()
	trace, err := Walk(context.Background(), d, task, NewAgentRunner(spawn))
	elapsed := time.Since(start)

	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("walk error = %v, want a *TimeoutError", err)
	}
	if te.State != "work" || te.TimeoutMS != 50 {
		t.Errorf("timeout = %+v, want state work, 50ms", te)
	}
	if msg := err.Error(); !strings.Contains(msg, `"work"`) || !strings.Contains(msg, "timeout_ms=50") {
		t.Errorf("error %q does not name the state and the timeout", msg)
	}
	if elapsed > time.Second {
		t.Errorf("the walk took %v to fail a 50ms timeout", elapsed)
	}
	if !cancelled.Load() {
		t.Error("the timed-out agent's run was not cancelled")
	}
	if len(trace) != 0 || task.State != "work" || len(ran) != 1 {
		t.Errorf("trace=%+v state=%q ran=%v, want the walk to stop at work", trace, task.State, ran)
	}
}

// timeout_ms 0 (unset) is no timeout: a slow agent finishes and the walk goes
// on, and the run is handed nothing that could stop a clock.
func TestHandlerTimeout_ZeroIsNoTimeout(t *testing.T) {
	d := mustParse(t, strings.Replace(timedAgentJSON, `,"timeout_ms":50`, "", 1))
	spawn := func(ctx context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
		if HoldObserver(ctx) != nil {
			t.Error("a run with no timeout was handed a hold observer")
		}
		if agent == "slow" {
			time.Sleep(100 * time.Millisecond)
		}
		return SpawnResult{Output: agent + "-out"}, nil
	}
	task := &Task{}
	if _, err := Walk(context.Background(), d, task, NewAgentRunner(spawn)); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if task.State != "done" {
		t.Errorf("final state = %q, want done", task.State)
	}
}

// A parallel state's timeout covers the whole fan-out: every member still out
// is cancelled and the state fails with the timeout.
func TestHandlerTimeout_ParallelCancelsEveryMember(t *testing.T) {
	d := mustParse(t, `{
	  "entry":"fan",
	  "states":[
	    {"state":"fan","handler":{"kind":"parallel","agents":["a","b"],"consolidator":"c","timeout_ms":50}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"fan","to":"done","on":"success"}]}`)
	var cancelled atomic.Int32
	spawn := func(ctx context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
		sp, err := blockUntilCancelled(ctx)
		if ctx.Err() != nil {
			cancelled.Add(1)
		}
		return sp, err
	}
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	var te *TimeoutError
	if !errors.As(err, &te) || te.State != "fan" {
		t.Fatalf("walk error = %v, want the fan state's timeout", err)
	}
	if cancelled.Load() != 2 {
		t.Errorf("%d of 2 members were cancelled", cancelled.Load())
	}
}

// Time a run spends held for a verdict is not counted: a member held for
// longer than the whole timeout is not cancelled while held, and the budget it
// has left still applies once the hold ends.
func TestHandlerTimeout_HeldTimeIsNotCounted(t *testing.T) {
	d := mustParse(t, strings.Replace(timedAgentJSON, `"timeout_ms":50`, `"timeout_ms":150`, 1))
	spawn := func(ctx context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
		if agent != "slow" {
			return SpawnResult{Output: "ok"}, nil
		}
		held := HoldObserver(ctx)
		if held == nil {
			t.Error("a timed run was handed no hold observer")
			return SpawnResult{}, errors.New("no observer")
		}
		held(true)
		select {
		case <-ctx.Done():
			t.Error("cancelled while held")
		case <-time.After(400 * time.Millisecond): // well past timeout_ms
		}
		held(false)
		return blockUntilCancelled(ctx) // then runs out what is left
	}
	start := time.Now()
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("walk error = %v, want the timeout once the hold ended", err)
	}
	if e := time.Since(start); e > 2*time.Second {
		t.Errorf("took %v: the budget left after the hold was not enforced", e)
	}
}

// A walk nested inside a member (say, a member that runs another team) must
// not stop its parent's clock: a run that has no clock of its own is handed no
// observer, even when its ctx inherited one.
func TestHandlerTimeout_NestedWalkDoesNotInheritTheObserver(t *testing.T) {
	ctx := WithHoldObserver(context.Background(), func(bool) { t.Error("a nested run reported to its parent's clock") })
	st := teamgraph.State{ID: "s", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "a"}}
	r := NewAgentRunner(func(ctx context.Context, _ string, _ Prompt, _ string) (SpawnResult, error) {
		if held := HoldObserver(ctx); held != nil {
			held(true)
		}
		return SpawnResult{Output: "ok"}, nil
	})
	if _, err := r.RunHandler(ctx, st, &Task{}); err != nil {
		t.Fatal(err)
	}
}

func timedStarter(ms int) teamgraph.State {
	st := starterState()
	st.Handler.TimeoutMS = ms
	return st
}

// A Starter's timeout_ms bounds EACH run: one that outlives it is cancelled
// and publishes status "timeout" — one message per run, as always — while its
// sibling's result stands, and a timed-out run does not count toward the wait.
func TestStarterTimeout_ARunThatOutlivesItPublishesTimeout(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(2)}
	r := starterRunner(ch, func(ctx context.Context, _ string, p Prompt, _ string) (SpawnResult, error) {
		if strings.Contains(p.DataSlots[StarterMessageSlot], `"i":0`) {
			return SpawnResult{Output: "in time", RunID: "r_ok", Status: "completed"}, nil
		}
		return blockUntilCancelled(ctx)
	})
	// Review armed, so the first success does not short-circuit the wave and
	// cancel the other run for a reason that is not its timeout.
	arming := &liveArming{}
	arming.on.Store(true)
	WithMemberReview(arming, 0)(r)
	st := timedStarter(50)
	st.Handler.Fanout.Wait = teamgraph.WaitAny

	start := time.Now()
	out, err := r.RunHandler(context.Background(), st, &Task{})
	if err != nil {
		t.Fatalf("wait:any with one run in time = %v", err)
	}
	if e := time.Since(start); e > time.Second {
		t.Errorf("the wave took %v with a 50ms timeout", e)
	}
	var ok, timedOut int
	for _, m := range ch.sinks(t) {
		switch m.Status {
		case SinkOK:
			ok++
		case SinkTimeout:
			timedOut++
			if m.Output != "" || !strings.Contains(m.Error, "timeout_ms=50") || m.RunID != "r_stuck" {
				t.Errorf("timeout sink message = %+v, want the reason, the run, and no output", m)
			}
		}
	}
	if ok != 1 || timedOut != 1 {
		t.Errorf("sink = %d ok / %d timeout, want 1 / 1", ok, timedOut)
	}
	if !strings.Contains(out.Output, `"status":"timeout"`) {
		t.Errorf("envelope = %s, want the timed-out run marked", out.Output)
	}

	// wait:all — the timed-out run leaves the wave one short.
	ch2 := &fakeChannels{inbox: inbox(2)}
	r.channels = ch2
	st.Handler.Fanout.Wait = teamgraph.WaitAll
	if _, err := r.RunHandler(context.Background(), st, &Task{}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("wait:all with a timed-out run = %v, want the wave short with the reason", err)
	}
}

// A Starter run held for review longer than timeout_ms is not timed out while
// held; once released it still has the time it had left.
func TestStarterTimeout_HeldRunIsNotTimedOutWhileHeld(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(1)}
	r := starterRunner(ch, func(ctx context.Context, _ string, _ Prompt, _ string) (SpawnResult, error) {
		held := HoldObserver(ctx)
		if held == nil {
			t.Error("a timed starter run was handed no hold observer")
			return SpawnResult{}, errors.New("no observer")
		}
		held(true)
		select {
		case <-ctx.Done():
			held(false)
			return SpawnResult{RunID: "r_held", Status: "cancelled"}, ctx.Err()
		case <-time.After(300 * time.Millisecond): // six times timeout_ms
		}
		held(false)
		return SpawnResult{Output: "approved answer", RunID: "r_held", Status: "completed"}, nil
	})
	if _, err := r.RunHandler(context.Background(), timedStarter(50), &Task{}); err != nil {
		t.Fatalf("a run held past timeout_ms was timed out: %v", err)
	}
	if sinks := ch.sinks(t); len(sinks) != 1 || sinks[0].Status != SinkOK {
		t.Errorf("sink = %+v, want one ok", sinks)
	}
}

// The clock stops only while EVERY run in flight is held: a sibling still
// working keeps it going.
func TestHeldClock_StopsOnlyWhileEveryLiveRunIsHeld(t *testing.T) {
	cause := errors.New("out of time")
	ctx, clk := startClock(context.Background(), 80*time.Millisecond, cause)
	defer clk.finish()
	a, b := clk.join(), clk.join()
	a.setHeld(true) // b still working: the clock runs
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the clock stopped while one run was still working")
	}
	if !errors.Is(context.Cause(ctx), cause) || !clk.timedOut() {
		t.Errorf("cause = %v, want the clock's", context.Cause(ctx))
	}
	a.leave()
	b.leave()

	ctx2, clk2 := startClock(context.Background(), 80*time.Millisecond, cause)
	defer clk2.finish()
	c, d := clk2.join(), clk2.join()
	c.setHeld(true)
	d.setHeld(true) // every run held: the clock stops
	select {
	case <-ctx2.Done():
		t.Fatal("the clock ran while every run was held")
	case <-time.After(300 * time.Millisecond):
	}
	d.leave() // the only run left is held: still stopped
	select {
	case <-ctx2.Done():
		t.Fatal("the clock ran while the only live run was held")
	case <-time.After(150 * time.Millisecond):
	}
	c.setHeld(false)
	select {
	case <-ctx2.Done():
	case <-time.After(time.Second):
		t.Fatal("the clock did not resume when the hold ended")
	}
}
