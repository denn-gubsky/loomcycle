package teamrun

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// fakeRunner returns a canned Outcome per state id. A state absent from outcomes
// returns success with an echoed output; a state in errs returns that error.
type fakeRunner struct {
	outcomes map[string]Outcome
	errs     map[string]error
	calls    []string // state ids run, in order
}

func (f *fakeRunner) RunHandler(_ context.Context, st teamgraph.State, input string) (Outcome, error) {
	f.calls = append(f.calls, st.ID)
	if err := f.errs[st.ID]; err != nil {
		return Outcome{}, err
	}
	if o, ok := f.outcomes[st.ID]; ok {
		return o, nil
	}
	return Outcome{Output: input + ">" + st.ID}, nil
}

func mustParse(t *testing.T, s string) teamgraph.Definition {
	t.Helper()
	d, err := teamgraph.Parse([]byte(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := teamgraph.Validate(d); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return d
}

const linearJSON = `{
  "entry":"a",
  "states":[
    {"state":"a","handler":{"kind":"agent","agent":"agent-a"}},
    {"state":"b","handler":{"kind":"agent","agent":"agent-b"}},
    {"state":"c","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"a","to":"b","on":"success"},
    {"from":"b","to":"c","on":"success"}
  ]}`

func TestWalk_LinearReachesTerminal(t *testing.T) {
	d := mustParse(t, linearJSON)
	r := &fakeRunner{}
	task := &Task{Input: "seed"}
	trace, err := Walk(context.Background(), d, task, r)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := len(trace); got != 2 {
		t.Fatalf("trace len = %d, want 2 (terminal not executed)", got)
	}
	if r.calls[0] != "a" || r.calls[1] != "b" || len(r.calls) != 2 {
		t.Errorf("ran %v, want [a b]", r.calls)
	}
	if task.State != "c" {
		t.Errorf("final state = %q, want c (terminal)", task.State)
	}
	// Output threads: seed → a → b.
	if task.Input != "seed>a>b" {
		t.Errorf("final input = %q, want seed>a>b (output threaded)", task.Input)
	}
	if trace[0].Agent != "agent-a" || trace[1].Edge != "success" {
		t.Errorf("trace metadata wrong: %+v", trace)
	}
}

func TestWalk_StartsAtEntryAndIsResumable(t *testing.T) {
	d := mustParse(t, linearJSON)

	// Empty State → starts at entry (a).
	r1 := &fakeRunner{}
	if _, err := Walk(context.Background(), d, &Task{}, r1); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(r1.calls) == 0 || r1.calls[0] != "a" {
		t.Errorf("empty task should start at entry a, ran %v", r1.calls)
	}

	// Preset State → resumes there (b), does NOT restart at entry. Proves the
	// walk is resumable from a persisted chunk position.
	r2 := &fakeRunner{}
	if _, err := Walk(context.Background(), d, &Task{State: "b"}, r2); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(r2.calls) != 1 || r2.calls[0] != "b" {
		t.Errorf("preset state=b should resume at b only, ran %v", r2.calls)
	}
}

func TestWalk_PushbackLoopHitsIterationCap(t *testing.T) {
	// a --success--> b ; b --pushback:redo--> a ; b --success--> done.
	// The fake b always pushes back, so the loop never converges and the cap
	// must fire (deterministic: entry state a is entered first each cycle).
	d := mustParse(t, `{
	  "entry":"a",
	  "max_iterations":3,
	  "states":[
	    {"state":"a","handler":{"kind":"agent","agent":"a"}},
	    {"state":"b","handler":{"kind":"agent","agent":"b"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[
	    {"from":"a","to":"b","on":"success"},
	    {"from":"b","to":"a","on":"pushback:redo"},
	    {"from":"b","to":"done","on":"success"}
	  ]}`)
	r := &fakeRunner{outcomes: map[string]Outcome{
		"b": {Output: "needs work", Edge: "pushback:redo"},
	}}
	_, err := Walk(context.Background(), d, &Task{}, r)
	var cap *ErrIterationCap
	if !errors.As(err, &cap) {
		t.Fatalf("want *ErrIterationCap, got %v", err)
	}
	if cap.State != "a" || cap.Max != 3 || cap.Count != 4 {
		t.Errorf("cap = %+v, want state=a max=3 count=4", cap)
	}
}

func TestWalk_UnknownEdgeErrors(t *testing.T) {
	d := mustParse(t, linearJSON)
	r := &fakeRunner{outcomes: map[string]Outcome{
		"a": {Edge: "pushback:nope"}, // no such transition from a
	}}
	_, err := Walk(context.Background(), d, &Task{}, r)
	if err == nil || !contains(err.Error(), "no matching transition") {
		t.Fatalf("want no-matching-transition error, got %v", err)
	}
}

func TestWalk_HandlerErrorPropagates(t *testing.T) {
	// A handler that returns an error (e.g. a fan-out that can't meet its wait
	// threshold) must be surfaced by the engine and stop the walk, not swallowed.
	d := mustParse(t, `{
	  "entry":"fan",
	  "states":[
	    {"state":"fan","handler":{"kind":"parallel","agents":["x","y"],"consolidator":"c"}},
	    {"state":"end","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"fan","to":"end","on":"success"}]}`)
	r := &fakeRunner{errs: map[string]error{"fan": errors.New("parallel not supported in phase 1")}}
	_, err := Walk(context.Background(), d, &Task{}, r)
	if err == nil || !contains(err.Error(), "parallel not supported") {
		t.Fatalf("want handler error propagated, got %v", err)
	}
}

func TestWalk_ContextCancelAborts(t *testing.T) {
	d := mustParse(t, linearJSON)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Walk(ctx, d, &Task{}, &fakeRunner{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// pingPongJSON is an a↔b success loop with no terminal + cap 2 — it never
// converges, so the per-state cap always fires (the OnCap escalation seam).
const pingPongJSON = `{
  "entry":"a",
  "max_iterations":2,
  "states":[
    {"state":"a","handler":{"kind":"agent","agent":"a"}},
    {"state":"b","handler":{"kind":"agent","agent":"b"}}
  ],
  "transitions":[
    {"from":"a","to":"b","on":"success"},
    {"from":"b","to":"a","on":"success"}
  ]}`

// rerouteJSON is the a↔b loop plus a terminal `done` reachable via a
// never-taken pushback edge — the fake always takes success, so `done` is only
// reached when an OnCap reroute jumps to it.
const rerouteJSON = `{
  "entry":"a",
  "max_iterations":2,
  "states":[
    {"state":"a","handler":{"kind":"agent","agent":"a"}},
    {"state":"b","handler":{"kind":"agent","agent":"b"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"a","to":"b","on":"success"},
    {"from":"b","to":"a","on":"success"},
    {"from":"b","to":"done","on":"pushback:stop"}
  ]}`

// TestWalk_OnEnterStateObservesEveryEnteredState pins the board-persistence seam:
// OnEnterState fires once per entered state, in order, INCLUDING the terminal —
// so a board-bound run's chunk.status lands on the end state when the walk
// completes.
func TestWalk_OnEnterStateObservesEveryEnteredState(t *testing.T) {
	d := mustParse(t, linearJSON) // a → b → c(terminal)
	var entered []string
	if _, err := Walk(context.Background(), d, &Task{Input: "seed"}, &fakeRunner{},
		OnEnterState(func(_ context.Context, s string) error {
			entered = append(entered, s)
			return nil
		})); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if want := []string{"a", "b", "c"}; !equalStrs(entered, want) {
		t.Errorf("entered %v, want %v (terminal observed too)", entered, want)
	}
}

// TestWalk_OnEnterStateFiresOnResume proves a resumed walk (preset State) re-
// observes from the persisted position, not the entry — so a board-bound run
// continues where it left off.
func TestWalk_OnEnterStateFiresOnResume(t *testing.T) {
	d := mustParse(t, linearJSON)
	var entered []string
	if _, err := Walk(context.Background(), d, &Task{State: "b"}, &fakeRunner{},
		OnEnterState(func(_ context.Context, s string) error {
			entered = append(entered, s)
			return nil
		})); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if want := []string{"b", "c"}; !equalStrs(entered, want) {
		t.Errorf("entered %v, want %v (resumed at b, no a)", entered, want)
	}
}

// TestWalk_OnEnterStateErrorAborts: a persistence failure aborts the walk before
// the failing state's handler runs (durability is load-bearing — a walk that
// can't record its position must not advance).
func TestWalk_OnEnterStateErrorAborts(t *testing.T) {
	d := mustParse(t, linearJSON)
	r := &fakeRunner{}
	sentinel := errors.New("persist failed")
	_, err := Walk(context.Background(), d, &Task{}, r,
		OnEnterState(func(_ context.Context, s string) error {
			if s == "b" {
				return sentinel
			}
			return nil
		}))
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != "a" {
		t.Errorf("ran %v, want [a] (b handler must not run after its persist fails)", r.calls)
	}
}

// TestWalk_OnCapContinueGrantsAnotherWindow: a CapContinue ruling resets the
// capped state's window and keeps walking; a later CapAbort still terminates with
// *ErrIterationCap. Proves the human-in-the-loop escalation is bounded by the
// human's answers, never infinite.
func TestWalk_OnCapContinueGrantsAnotherWindow(t *testing.T) {
	d := mustParse(t, pingPongJSON)
	calls := 0
	_, err := Walk(context.Background(), d, &Task{}, &fakeRunner{},
		OnCap(func(_ context.Context, _ *ErrIterationCap) (CapDecision, error) {
			calls++
			if calls == 1 {
				return CapDecision{Action: CapContinue}, nil
			}
			return CapDecision{Action: CapAbort}, nil
		}))
	var cap *ErrIterationCap
	if !errors.As(err, &cap) {
		t.Fatalf("want *ErrIterationCap after abort, got %v", err)
	}
	if calls != 2 {
		t.Errorf("onCap called %d times, want 2 (continue once, then abort)", calls)
	}
}

// TestWalk_OnCapRerouteJumpsToTarget: a CapReroute ruling jumps the walk to the
// named state without running the capped state's handler; rerouting to a terminal
// completes the walk cleanly.
func TestWalk_OnCapRerouteJumpsToTarget(t *testing.T) {
	d := mustParse(t, rerouteJSON)
	task := &Task{}
	_, err := Walk(context.Background(), d, task, &fakeRunner{},
		OnCap(func(_ context.Context, _ *ErrIterationCap) (CapDecision, error) {
			return CapDecision{Action: CapReroute, Reroute: "done"}, nil
		}))
	if err != nil {
		t.Fatalf("reroute to a terminal should complete, got %v", err)
	}
	if task.State != "done" {
		t.Errorf("final state = %q, want done (rerouted)", task.State)
	}
}

// TestWalk_OnCapAbortReturnsCapErr: an explicit CapAbort (and the zero-value
// decision) terminates with *ErrIterationCap — the same outcome as no observer.
func TestWalk_OnCapAbortReturnsCapErr(t *testing.T) {
	d := mustParse(t, pingPongJSON)
	_, err := Walk(context.Background(), d, &Task{}, &fakeRunner{},
		OnCap(func(_ context.Context, _ *ErrIterationCap) (CapDecision, error) {
			return CapDecision{Action: CapAbort}, nil
		}))
	var cap *ErrIterationCap
	if !errors.As(err, &cap) {
		t.Fatalf("abort should return *ErrIterationCap, got %v", err)
	}
}

// TestWalk_OnCapErrorPropagates: an error from the OnCap observer aborts the walk
// with that error (e.g. the interruption machinery is unreachable).
func TestWalk_OnCapErrorPropagates(t *testing.T) {
	d := mustParse(t, pingPongJSON)
	sentinel := errors.New("interruption bus down")
	_, err := Walk(context.Background(), d, &Task{}, &fakeRunner{},
		OnCap(func(_ context.Context, _ *ErrIterationCap) (CapDecision, error) {
			return CapDecision{}, sentinel
		}))
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
