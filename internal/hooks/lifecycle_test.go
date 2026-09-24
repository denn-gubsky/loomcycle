package hooks

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func lifecycleDispatcher(t *testing.T, hs ...*Hook) *Dispatcher {
	t.Helper()
	r := NewRegistry()
	for _, h := range hs {
		mustRegister(t, r, h)
	}
	return NewDispatcher(r, nil)
}

// An agent_start hook sees the run, and may add context to its prompt or deny
// it; the first deny stops the chain.
func TestDispatcher_AgentStartAddsContextOrDenies(t *testing.T) {
	ctxHook := newFakeHook(t, `{"additional_context":"the user is on the free plan"}`)
	deny := newFakeHook(t, `{"decision":"deny","reason":"agent disabled for this tenant"}`)
	after := newFakeHook(t, `{}`)
	d := lifecycleDispatcher(t,
		&Hook{Owner: "x", Name: "ctx", Phase: PhaseAgentStart, CallbackURL: ctxHook.srv.URL},
		&Hook{Owner: "x", Name: "no", Phase: PhaseAgentStart, CallbackURL: deny.srv.URL},
		&Hook{Owner: "x", Name: "after", Phase: PhaseAgentStart, CallbackURL: after.srv.URL})
	out := d.RunAgentStart(context.Background(), Identity{Agent: "a", RunID: "r1"})
	if !out.Denied || out.Reason != "agent disabled for this tenant" || out.AdditionalContext != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if len(after.bodies) != 0 {
		t.Error("a hook after the deny ran")
	}
	if !strings.Contains(ctxHook.bodies[0], `"phase":"agent_start"`) || !strings.Contains(ctxHook.bodies[0], `"run_id":"r1"`) {
		t.Errorf("payload = %s", ctxHook.bodies[0])
	}
	if len(out.Decisions) != 2 || out.Decisions[0].Kind != "context" || out.Decisions[1].Kind != "deny" {
		t.Errorf("decisions = %+v", out.Decisions)
	}

	d = lifecycleDispatcher(t, &Hook{Owner: "x", Name: "ctx", Phase: PhaseAgentStart, CallbackURL: ctxHook.srv.URL})
	out = d.RunAgentStart(context.Background(), Identity{Agent: "a"})
	if out.Denied || len(out.AdditionalContext) != 1 || out.AdditionalContext[0] != "the user is on the free plan" {
		t.Errorf("context outcome = %+v", out)
	}
}

// A failed agent_start hook denies the run when it fails closed and is
// skipped when it fails open; a decision that does not apply is a failure.
func TestDispatcher_AgentStartFailModes(t *testing.T) {
	block := newFakeHook(t, `{"decision":"block","reason":"x"}`)
	for mode, wantDenied := range map[FailMode]bool{FailClosed: true, FailOpen: false} {
		d := lifecycleDispatcher(t, &Hook{Owner: "x", Name: "bad", Phase: PhaseAgentStart, CallbackURL: block.srv.URL, FailMode: mode})
		out := d.RunAgentStart(context.Background(), Identity{Agent: "a"})
		if out.Denied != wantDenied || len(out.Decisions) != 1 || out.Decisions[0].Kind != "unavailable" ||
			!strings.Contains(out.Decisions[0].Reason, "does not apply to agent_start") {
			t.Errorf("%s: outcome = %+v", mode, out)
		}
	}
}

// agent_stop precedence: the first block wins and stops the chain; a hold
// does not stop it, so a later block still wins over it.
func TestDispatcher_AgentStopBlockBeatsHold(t *testing.T) {
	hold := newFakeHook(t, `{"decision":"hold","reason":"a person should see this"}`)
	block := newFakeHook(t, `{"decision":"block","reason":"cite a source"}`)
	later := newFakeHook(t, `{"decision":"hold"}`)
	d := lifecycleDispatcher(t,
		&Hook{Owner: "x", Name: "hold", Phase: PhaseAgentStop, CallbackURL: hold.srv.URL},
		&Hook{Owner: "x", Name: "check", Phase: PhaseAgentStop, CallbackURL: block.srv.URL},
		&Hook{Owner: "x", Name: "later", Phase: PhaseAgentStop, CallbackURL: later.srv.URL})
	out := d.RunAgentStop(context.Background(), Identity{Agent: "a"}, StopInfo{FinalText: "the answer", StopReason: "end_turn", StopBlocks: 2})
	if out.Kind != StopBlock || out.Reason != "cite a source" || out.By != "x/check" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(later.bodies) != 0 {
		t.Error("a hook after the block ran")
	}
	for _, want := range []string{`"final_text":"the answer"`, `"stop_reason":"end_turn"`, `"stop_hook_active":true`, `"stop_blocks":2`} {
		if !strings.Contains(hold.bodies[0], want) {
			t.Errorf("payload lacks %s: %s", want, hold.bodies[0])
		}
	}

	d = lifecycleDispatcher(t, &Hook{Owner: "x", Name: "hold", Phase: PhaseAgentStop, CallbackURL: hold.srv.URL})
	out = d.RunAgentStop(context.Background(), Identity{Agent: "a"}, StopInfo{})
	if out.Kind != StopHold || out.Reason != "a person should see this" || out.By != "x/hold" {
		t.Errorf("hold outcome = %+v", out)
	}
}

// A failed agent_stop hook holds the answer for a person when it fails
// closed, and lets it finish when it fails open. A block with no reason is a
// failure: the reason is what the model is told to fix.
func TestDispatcher_AgentStopFailModes(t *testing.T) {
	noReason := newFakeHook(t, `{"decision":"block"}`)
	for mode, want := range map[FailMode]string{FailClosed: StopHold, FailOpen: StopAllow} {
		d := lifecycleDispatcher(t, &Hook{Owner: "x", Name: "bad", Phase: PhaseAgentStop, CallbackURL: noReason.srv.URL, FailMode: mode})
		out := d.RunAgentStop(context.Background(), Identity{Agent: "a"}, StopInfo{})
		if out.Kind != want || len(out.Decisions) != 1 || !strings.Contains(out.Decisions[0].Reason, "needs a reason") {
			t.Errorf("%s: outcome = %+v", mode, out)
		}
	}
}

// A code body decides agent_start and agent_stop too, through the same checks.
func TestDispatcher_ACodeHookDecidesTheRunLifecycle(t *testing.T) {
	run := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{Decision: "block", Reason: "add a summary"}, nil
	}}
	d := codeDispatcher(t, run, &Hook{Owner: "x", Name: "g", Phase: PhaseAgentStop, Code: "function hook(ev) {}"})
	if out := d.RunAgentStop(context.Background(), Identity{Agent: "a"}, StopInfo{}); out.Kind != StopBlock || out.Reason != "add a summary" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(run.events) != 1 || run.events[0] != "agent_stop" {
		t.Errorf("events = %v", run.events)
	}
	wrong := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{UpdatedOutput: &ToolResult{Text: "x"}}, nil
	}}
	d = codeDispatcher(t, wrong, &Hook{Owner: "x", Name: "g", Phase: PhaseAgentStart, Code: "function hook(ev) {}", FailMode: FailClosed})
	if out := d.RunAgentStart(context.Background(), Identity{Agent: "a"}); !out.Denied || !strings.Contains(out.Decisions[0].Reason, "tool hooks only") {
		t.Errorf("start outcome = %+v", out)
	}
}

// A lifecycle hook is selected by agent only: a tools selector is refused.
func TestRegistry_ALifecycleHookTakesNoToolsSelector(t *testing.T) {
	r := NewRegistry()
	_, err := r.Register(&Hook{Owner: "x", Name: "s", Phase: PhaseAgentStop, Tools: []string{"Bash"}, CallbackURL: "http://e.test/h"})
	if !errors.Is(err, ErrInvalidRegistration) || !strings.Contains(err.Error(), "agents only") {
		t.Fatalf("err = %v", err)
	}
	mustRegister(t, r, &Hook{Owner: "x", Name: "s", Phase: PhaseAgentStop, Agents: []string{"writer"}, CallbackURL: "http://e.test/h"})
	d := NewDispatcher(r, nil)
	if !d.Matches(Identity{Agent: "writer"}, PhaseAgentStop) || d.Matches(Identity{Agent: "other"}, PhaseAgentStop) {
		t.Error("the agents selector did not select")
	}
}
