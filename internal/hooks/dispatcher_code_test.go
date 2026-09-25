package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeCodeRunner returns a scripted decision and records what it was asked.
type fakeCodeRunner struct {
	decide func(ctx context.Context) (CodeDecision, error)
	events []string
}

func (f *fakeCodeRunner) Compile(string) error { return nil }
func (f *fakeCodeRunner) Run(ctx context.Context, _ *Hook, event string, _ any) (CodeDecision, error) {
	f.events = append(f.events, event)
	return f.decide(ctx)
}

func codeDispatcher(t *testing.T, run CodeRunner, hs ...*Hook) *Dispatcher {
	t.Helper()
	r := NewRegistry()
	for _, h := range hs {
		mustRegister(t, r, h)
	}
	d := NewDispatcher(r, nil)
	if run != nil {
		d.SetCodeRunner(run)
	}
	return d
}

// A code hook's decision goes through the same chain as a webhook's: a deny
// stops the call and is reported, a rewrite changes the input.
func TestDispatcher_ACodeHookDecidesThroughTheRunner(t *testing.T) {
	deny := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{Decision: "deny", Reason: "not today"}, nil
	}}
	d := codeDispatcher(t, deny, &Hook{Owner: "x", Name: "gate", Phase: PhasePre, Code: "function hook(ev) {}"})
	out := d.RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"})
	if out.Deny == nil || out.Deny.Text != "not today" || !out.Deny.IsError {
		t.Fatalf("deny = %+v", out.Deny)
	}
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "deny" {
		t.Errorf("decisions = %+v", out.Decisions)
	}
	if len(deny.events) != 1 || deny.events[0] != "pre_tool_use" {
		t.Errorf("events = %v", deny.events)
	}

	rewrite := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{UpdatedOutput: &ToolResult{Text: "redacted"}, AdditionalContext: "checked"}, nil
	}}
	d = codeDispatcher(t, rewrite, &Hook{Owner: "x", Name: "scrub", Phase: PhasePostFailure, Code: "function hook(ev) {}"})
	post := d.RunPost(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"},
		ToolResult{Text: "secret", IsError: true, Error: &ToolError{Category: "auth"}})
	if post.Result.Text != "redacted" || post.Result.IsError || post.Result.Error != nil {
		t.Errorf("result = %+v, want the hook's success with the tool's classification dropped", post.Result)
	}
	if len(post.AdditionalContext) != 1 || post.AdditionalContext[0] != "checked" {
		t.Errorf("context = %v", post.AdditionalContext)
	}
	if len(rewrite.events) != 1 || rewrite.events[0] != "post_tool_use_failure" {
		t.Errorf("events = %v", rewrite.events)
	}
}

// A field that does not apply to the hook's phase is a mistake in the body:
// reported as the hook failing, and the fail mode decides.
func TestDispatcher_ACodeDecisionOutsideItsPhaseFails(t *testing.T) {
	wrong := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{UpdatedOutput: &ToolResult{Text: "x"}}, nil
	}}
	d := codeDispatcher(t, wrong, &Hook{Owner: "x", Name: "g", Phase: PhasePre, Code: "function hook(ev) {}", FailMode: FailClosed})
	out := d.RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"})
	if out.Deny == nil || len(out.Decisions) != 1 || out.Decisions[0].Kind != "unavailable" ||
		!strings.Contains(out.Decisions[0].Reason, "post hooks only") {
		t.Fatalf("outcome = %+v", out)
	}
	allowOnPost := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{Decision: "deny"}, nil
	}}
	d = codeDispatcher(t, allowOnPost, &Hook{Owner: "x", Name: "g", Phase: PhasePost, Code: "function hook(ev) {}"})
	post := d.RunPost(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"}, ToolResult{Text: "ok"})
	if post.Result.Text != "ok" || len(post.Decisions) != 1 || post.Decisions[0].Kind != "unavailable" {
		t.Fatalf("post outcome = %+v", post)
	}
}

// With no runner installed (code hooks disabled, a hook reloaded from the
// database) a code hook is unavailable, and its fail mode decides.
func TestDispatcher_ACodeHookWithoutARunnerIsUnavailable(t *testing.T) {
	d := codeDispatcher(t, nil, &Hook{Owner: "x", Name: "g", Phase: PhasePre, Code: "function hook(ev) {}", FailMode: FailClosed})
	out := d.RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"})
	if out.Deny == nil || len(out.Decisions) != 1 || !strings.Contains(out.Decisions[0].Reason, "not enabled") {
		t.Fatalf("outcome = %+v", out)
	}
}

// A run cancelled while a hook was deciding never runs its tool, even when
// the hook fails open: fail-open is for a hook that is down, not a run that
// is over.
func TestDispatcher_ACancelledRunIsNeverFailedOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waits := &fakeCodeRunner{decide: func(ctx context.Context) (CodeDecision, error) {
		cancel()
		<-ctx.Done()
		return CodeDecision{}, ctx.Err()
	}}
	d := codeDispatcher(t, waits, &Hook{Owner: "x", Name: "g", Phase: PhasePre, Code: "function hook(ev) {}", FailMode: FailOpen})
	out := d.RunPre(ctx, Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read"})
	if out.Deny == nil || !strings.Contains(out.Deny.Text, "cancelled") {
		t.Fatalf("outcome = %+v, want the call stopped", out)
	}
}

// A code hook's timeout bounds each run of its code, so no deadline sits on
// the context it is given (that would count a person's answer against it).
func TestDispatcher_ACodeHookGetsNoDeadline(t *testing.T) {
	var deadline bool
	probe := &fakeCodeRunner{decide: func(ctx context.Context) (CodeDecision, error) {
		_, deadline = ctx.Deadline()
		return CodeDecision{}, nil
	}}
	d := codeDispatcher(t, probe, &Hook{Owner: "x", Name: "g", Phase: PhasePre, Code: "function hook(ev) {}"})
	d.RunPre(context.Background(), Identity{Agent: "a"}, ToolCall{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)})
	if deadline {
		t.Error("the runner was given a deadline")
	}
}

// A hook has exactly one body, a code body is bounded in size, and its
// timeout defaults tight and is capped at a second.
func TestRegistry_ACodeHookHasOneBodyAndATightTimeout(t *testing.T) {
	r := NewRegistry()
	for name, h := range map[string]*Hook{
		"both":     {Owner: "x", Name: "b", Phase: PhasePre, CallbackURL: "http://e.test/h", Code: "function hook(ev) {}"},
		"neither":  {Owner: "x", Name: "n", Phase: PhasePre},
		"too long": {Owner: "x", Name: "l", Phase: PhasePre, Code: strings.Repeat("x", MaxCodeBytes+1)},
	} {
		if _, err := r.Register(h); !errors.Is(err, ErrInvalidRegistration) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for ms, want := range map[int]time.Duration{0: 50 * time.Millisecond, 200: 200 * time.Millisecond, 5000: time.Second} {
		h := &Hook{Owner: "x", Name: "t", Phase: PhasePre, Code: "function hook(ev) {}", TimeoutMs: ms}
		mustRegister(t, r, h)
		if h.Timeout != want {
			t.Errorf("timeout_ms %d → %s, want %s", ms, h.Timeout, want)
		}
	}
}

// A tenant's code hook — one persisted before the operator required the
// tenant opt-in, say — does not run unless tenant code hooks are allowed; an
// operator-global code hook runs either way.
func TestDispatcher_ATenantCodeHookRunsOnlyWhenTenantCodeHooksAreAllowed(t *testing.T) {
	run := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{Decision: "deny", Reason: "ran"}, nil
	}}
	d := codeDispatcher(t, run, &Hook{Tenant: "acme", Owner: "t", Name: "gate", Phase: PhasePre, Code: "function hook(ev) {}"})
	ident := Identity{Agent: "a", Tenant: "acme"}
	out := d.RunPre(context.Background(), ident, ToolCall{ID: "t1", Name: "Read"})
	if len(run.events) != 0 || len(out.Decisions) != 1 || out.Decisions[0].Kind != "unavailable" ||
		!strings.Contains(out.Decisions[0].Reason, "registered by a tenant") {
		t.Fatalf("without the opt-in: ran %d times, decisions %+v", len(run.events), out.Decisions)
	}
	d.AllowTenantCodeHooks(true)
	if out := d.RunPre(context.Background(), ident, ToolCall{ID: "t1", Name: "Read"}); out.Deny == nil || out.Deny.Text != "ran" {
		t.Errorf("with the opt-in: %+v", out)
	}

	global := codeDispatcher(t, run, &Hook{Owner: "op", Name: "gate", Phase: PhasePre, Code: "function hook(ev) {}"})
	if out := global.RunPre(context.Background(), ident, ToolCall{ID: "t1", Name: "Read"}); out.Deny == nil {
		t.Errorf("an operator-global code hook did not run: %+v", out)
	}
}
