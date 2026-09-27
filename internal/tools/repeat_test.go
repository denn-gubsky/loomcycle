package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// flakyStub fails while fail is true and counts real runs.
type flakyStub struct {
	pointerStub
	fail bool
	ran  int
}

func (f *flakyStub) Execute(context.Context, json.RawMessage) (Result, error) {
	f.ran++
	if f.fail {
		return Result{Text: `Memory tool: scope "tenant" not in this agent's memory_scopes [user]`, IsError: true}, nil
	}
	return Result{Text: "ok"}, nil
}

// The same failed call runs twice — a transient failure may pass the second
// time — and after that it is refused without running; after three such
// refusals the run is flagged to stop. Key order in the arguments does not make
// a repeat look new.
func TestExecute_ARepeatedFailedCallIsRefusedThenStopsTheRun(t *testing.T) {
	mem := &flakyStub{pointerStub: pointerStub{name: "Memory", schema: `{"type":"object"}`}, fail: true}
	d := NewDispatcher([]Tool{mem})
	call := func(in string) Result {
		return d.Execute(context.Background(), "Memory", json.RawMessage(in))
	}
	call(`{"op":"set","scope":"tenant","key":"k"}`)
	call(`{"key":"k","op":"set","scope":"tenant"}`) // same call, reordered
	if mem.ran != 2 {
		t.Fatalf("the first two failures should run; ran %d", mem.ran)
	}
	res := call(`{"op":"set","scope":"tenant","key":"k"}`)
	if mem.ran != 2 || !res.IsError || !strings.Contains(res.Text, "was NOT run again") || res.Error == nil || res.Error.Retryable {
		t.Errorf("third identical failure: ran %d, result %+v; want refused, not run, not retryable", mem.ran, res)
	}
	if _, stop := d.RepeatedFailure(); stop {
		t.Fatal("one refusal must not stop the run")
	}
	call(`{"op":"set","scope":"tenant","key":"k"}`)
	call(`{"op":"set","scope":"tenant","key":"k"}`)
	why, stop := d.RepeatedFailure()
	if !stop || !strings.Contains(why, "Memory") {
		t.Errorf("after three refusals: stop=%v why=%q", stop, why)
	}
}

// Changed arguments are a different call, and a success clears the count: the
// failure is then not a fixed fact.
func TestExecute_ADifferentOrSucceedingCallIsNotARepeat(t *testing.T) {
	mem := &flakyStub{pointerStub: pointerStub{name: "Memory", schema: `{"type":"object"}`}, fail: true}
	d := NewDispatcher([]Tool{mem})
	same := json.RawMessage(`{"op":"set","scope":"tenant","key":"k"}`)
	d.Execute(context.Background(), "Memory", same)
	d.Execute(context.Background(), "Memory", same)
	d.Execute(context.Background(), "Memory", json.RawMessage(`{"op":"set","scope":"user","key":"k"}`))
	if mem.ran != 3 {
		t.Errorf("a call with changed arguments was refused; ran %d", mem.ran)
	}
	mem.fail = false
	d.Execute(context.Background(), "Memory", same) // refused: still counted as failed twice
	fresh := NewDispatcher([]Tool{mem})
	other := json.RawMessage(`{"op":"get","scope":"user","key":"k"}`)
	mem.fail = true
	fresh.Execute(context.Background(), "Memory", same)
	mem.fail = false
	fresh.Execute(context.Background(), "Memory", same) // succeeds, clears
	// A different call between them, so the three-in-a-row guard stays out of
	// what this test is about.
	fresh.Execute(context.Background(), "Memory", other)
	mem.fail = true
	before := mem.ran
	fresh.Execute(context.Background(), "Memory", same)
	fresh.Execute(context.Background(), "Memory", same)
	if mem.ran != before+2 {
		t.Errorf("a success did not clear the count: ran %d more, want 2", mem.ran-before)
	}
}

// The third identical call in a row is refused without running, even though
// every earlier one succeeded — measured live, one successful tenant save was
// re-sent 36 times back to back. The refusal carries no "correct call" example:
// the call's shape was never the problem.
func TestExecute_TheThirdIdenticalCallInARowIsRefused(t *testing.T) {
	mem := &flakyStub{pointerStub: pointerStub{name: "Memory", schema: `{"type":"object"}`}}
	help := newHelp("Memory", "Memory/set")
	help.examples["Memory/set"] = `{"op":"set","scope":"user","key":"k","value":"v"}`
	d := NewDispatcher([]Tool{mem, help})
	call := func(in string) Result {
		return d.Execute(context.Background(), "Memory", json.RawMessage(in))
	}
	call(`{"op":"set","scope":"tenant","key":"k","value":"v"}`)
	call(`{"value":"v","key":"k","op":"set","scope":"tenant"}`) // the same call, reordered
	res := call(`{"op":"set","scope":"tenant","key":"k","value":"v"}`)
	if mem.ran != 2 {
		t.Fatalf("the tool ran %d times; want 2 (the third in a row is refused)", mem.ran)
	}
	if !res.IsError || !strings.Contains(res.Text, "this exact call 2 times in a row") ||
		res.Error == nil || res.Error.Retryable {
		t.Errorf("third call: %+v; want a non-retryable refusal", res)
	}
	if strings.Contains(res.Text, "A correct") {
		t.Errorf("the refusal carries a call example, which reads as if the call were malformed:\n%s", res.Text)
	}
}

// Only a back-to-back streak counts: another call in between, or a change to
// the arguments, starts it over.
func TestExecute_AnotherCallOrArgumentBreaksTheSequence(t *testing.T) {
	mem := &flakyStub{pointerStub: pointerStub{name: "Memory", schema: `{"type":"object"}`}}
	read := &flakyStub{pointerStub: pointerStub{name: "Read", schema: `{"type":"object"}`}}
	d := NewDispatcher([]Tool{mem, read})
	a := json.RawMessage(`{"op":"get","scope":"user","key":"k"}`)
	d.Execute(context.Background(), "Memory", a)
	d.Execute(context.Background(), "Memory", a)
	d.Execute(context.Background(), "Read", json.RawMessage(`{"path":"/notes.md"}`))
	d.Execute(context.Background(), "Memory", a)
	d.Execute(context.Background(), "Memory", json.RawMessage(`{"op":"get","scope":"user","key":"other"}`))
	d.Execute(context.Background(), "Memory", a)
	if mem.ran != 5 || read.ran != 1 {
		t.Errorf("ran Memory %d, Read %d; want every call run (5, 1): nothing was three in a row", mem.ran, read.ran)
	}
}

// A model that will not break the loop keeps being refused, and the failed-call
// guard then ends the run — without the tool running again.
func TestExecute_ALoopThatWillNotBreakStopsTheRun(t *testing.T) {
	mem := &flakyStub{pointerStub: pointerStub{name: "Memory", schema: `{"type":"object"}`}}
	d := NewDispatcher([]Tool{mem})
	same := json.RawMessage(`{"op":"set","scope":"tenant","key":"k","value":"v"}`)
	for i := 0; i < 7; i++ {
		d.Execute(context.Background(), "Memory", same)
	}
	if mem.ran != 2 {
		t.Errorf("the tool ran %d times; want 2", mem.ran)
	}
	if why, stop := d.RepeatedFailure(); !stop || !strings.Contains(why, "Memory") {
		t.Errorf("after 7 identical calls in a row: stop=%v why=%q; want the run flagged to stop", stop, why)
	}
}
