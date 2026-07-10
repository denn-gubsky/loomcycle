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
	// The Phase-1 production runner errors on a parallel handler; the engine
	// must surface that error and stop, not swallow it.
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
