package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// panicStub is a tool whose parser has a bug: any call makes it panic.
type panicStub struct{ pointerStub }

func (p *panicStub) Execute(context.Context, json.RawMessage) (Result, error) {
	var cells []string
	_ = cells[3] // index out of range, as a malformed file can make a parser do
	return Result{}, nil
}

// A tool that panics costs the call, not the process: the dispatcher answers
// with a tool_panicked failure that names the tool and a reference, never the
// stack, and the caller goes on.
func TestDispatcher_APanickingToolBecomesAToolError(t *testing.T) {
	tl := &panicStub{pointerStub{name: "File", schema: `{"type":"object"}`}}
	d := NewDispatcher([]Tool{tl})

	res, goErr := d.Call(context.Background(), "File", json.RawMessage(`{}`))
	if goErr != nil {
		t.Errorf("Call returned a Go error for a panic: %v", goErr)
	}
	if !res.IsError || !strings.HasPrefix(res.Text, "tool_panicked:") || !strings.Contains(res.Text, "File") {
		t.Fatalf("result = %+v; want a tool_panicked failure naming the tool", res)
	}
	if strings.Contains(res.Text, "goroutine") || strings.Contains(res.Text, ".go:") || strings.Contains(res.Text, "index out of range") {
		t.Errorf("the model-visible text carries the stack or the panic: %q", res.Text)
	}
	if res.Error == nil || res.Error.Category != CategoryBusiness || res.Error.Retryable {
		t.Errorf("classification = %+v; want business, not retryable", res.Error)
	}
	// The dispatcher is still usable, and the same bug reached another way
	// carries the same reference.
	again := d.Execute(context.Background(), "File", json.RawMessage(`{}`))
	if !again.IsError || again.Text != res.Text {
		t.Errorf("second call = %q, first = %q", again.Text, res.Text)
	}
}

// Each recovered panic is counted by tool, for /metrics.
func TestDispatcher_APanicIsCountedByTool(t *testing.T) {
	tl := &panicStub{pointerStub{name: "File", schema: `{"type":"object"}`}}
	c := NewPanicCounter()
	d := NewDispatcher([]Tool{tl})
	d.SetPanicCounter(c)
	d.Execute(context.Background(), "File", json.RawMessage(`{}`))
	d.Execute(context.Background(), "File", json.RawMessage(`{"a":1}`))
	if got := c.Snapshot()["File"]; got != 2 {
		t.Errorf("counted %d panics, want 2 (%v)", got, c.Snapshot())
	}
	var nilCounter *PanicCounter
	if nilCounter.Snapshot() != nil {
		t.Error("a nil counter reported counts")
	}
}

// A panic in the fallback that resolves an unknown name (a lazily mounted
// MCP server) is recovered too, and is not reported as an unknown tool.
func TestDispatcher_APanickingFallbackBecomesAToolError(t *testing.T) {
	d := NewDispatcherWithFallback(nil, func(context.Context, string, json.RawMessage) (Result, bool) {
		var p *Result
		return *p, true
	})
	res := d.Execute(context.Background(), "mcp__x__y", json.RawMessage(`{}`))
	if !res.IsError || !strings.HasPrefix(res.Text, "tool_panicked:") {
		t.Errorf("result = %+v", res)
	}
}
