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
	mem.fail = true
	fresh.Execute(context.Background(), "Memory", same)
	mem.fail = false
	fresh.Execute(context.Background(), "Memory", same) // succeeds, clears
	mem.fail = true
	before := mem.ran
	fresh.Execute(context.Background(), "Memory", same)
	fresh.Execute(context.Background(), "Memory", same)
	if mem.ran != before+2 {
		t.Errorf("a success did not clear the count: ran %d more, want 2", mem.ran-before)
	}
}
