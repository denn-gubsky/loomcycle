package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// crashTool panics on every call, as a parser with a bug does on a hostile
// file.
type crashTool struct{}

func (crashTool) Name() string                 { return "Crash" }
func (crashTool) Description() string          { return "" }
func (crashTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (crashTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	var m map[string]int
	m["x"] = 1 // assignment to a nil map
	return tools.Result{}, nil
}

// A tool that panics in a parallel batch costs its own call: it answers
// tool_panicked, the other tool of the batch answers as usual, and the run
// goes on to its next turn.
func TestLoop_APanickingToolInAParallelBatchBecomesAToolError(t *testing.T) {
	pending := []providers.ToolUse{
		{ID: "call_0", Name: "Crash", Input: json.RawMessage(`{}`)},
		{ID: "call_1", Name: "Slow", Input: json.RawMessage(`{"id":"call_1","ms":5}`)},
	}
	slow := newSlowTool()
	prov := &scriptedProvider{toolCalls: pending}
	ts := []tools.Tool{crashTool{}, slow}
	res, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           ts,
		Dispatcher:      tools.NewDispatcher(ts),
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 8,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn (the run should go on)", res.StopReason)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) < 2 {
		t.Fatalf("the run made %d model calls; the turn after the batch never came", len(prov.requests))
	}
	got := map[string]providers.ContentBlock{}
	for _, msg := range prov.requests[1].Messages {
		for _, c := range msg.Content {
			if c.Type == "tool_result" {
				got[c.ToolUseID] = c
			}
		}
	}
	if c := got["call_0"]; !c.IsError || !strings.Contains(c.Text, "tool_panicked") || !strings.Contains(c.Text, "Crash") {
		t.Errorf("panicking call's result = %+v", c)
	}
	if c := got["call_1"]; c.IsError || c.Text != "done:call_1" {
		t.Errorf("the other call's result = %+v", c)
	}
}

// A panic around the call on the tool goroutine (a hook, the dispatch code),
// outside the dispatcher's own guard, is recovered the same way.
func TestLoop_APanicAroundAToolCallBecomesAToolError(t *testing.T) {
	c := tools.NewPanicCounter()
	d := tools.NewDispatcher(nil)
	d.SetPanicCounter(c)
	r := recoverToolPanic(context.Background(), d, providers.ToolUse{Name: "Read", Input: json.RawMessage(`{}`)}, func() tools.Result {
		var hook func()
		hook() // a nil hook called
		return tools.Result{}
	})
	if !r.IsError || !strings.HasPrefix(r.Text, "tool_panicked:") || !strings.Contains(r.Text, "Read") || !strings.Contains(r.Text, "may have taken effect") {
		t.Errorf("result = %+v", r)
	}
	// Read is not a tool of this dispatcher, so it is counted as other.
	if c.Snapshot()["other"] != 1 {
		t.Errorf("counts = %v", c.Snapshot())
	}
	if r := recoverToolPanic(context.Background(), nil, providers.ToolUse{Name: "Read"}, func() tools.Result { panic("x") }); !r.IsError {
		t.Errorf("with no dispatcher: %+v", r)
	}
}
