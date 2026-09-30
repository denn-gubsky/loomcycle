package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// refusingTool always fails, as a scope refusal does.
type refusingTool struct{ calls int }

func (r *refusingTool) Name() string                 { return "Memory" }
func (r *refusingTool) Description() string          { return "stores things" }
func (r *refusingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *refusingTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	r.calls++
	return tools.Result{Text: `scope "tenant" not granted`, IsError: true}, nil
}

// A stateful run that keeps sending the same refused call ends with
// repeated_failed_call instead of spinning to its deadline — measured live, a
// refused tenant write re-sent until a 10-minute timeout.
func TestRun_Stateful_ARunRepeatingAFailedCallIsStopped(t *testing.T) {
	step := `{"patch":{},"action":{"tool":"Memory","input":{"op":"set","scope":"tenant","key":"k"}}}`
	scripts := make([]string, 20)
	for i := range scripts {
		scripts[i] = step
	}
	prov := &statefulScriptProvider{scripts: scripts}
	mem := &refusingTool{}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{mem},
		Dispatcher: tools.NewDispatcher([]tools.Tool{mem}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	})
	if err == nil || res.StopReason != StopReasonRepeatedFailedCall || !strings.Contains(err.Error(), "same failed Memory call") {
		t.Fatalf("stop=%q err=%v; want repeated_failed_call", res.StopReason, err)
	}
	if mem.calls != 2 {
		t.Errorf("the tool ran %d times; want 2 (the repeats after that are refused, not run)", mem.calls)
	}
	if prov.calls() > 6 {
		t.Errorf("%d model calls; the run should stop soon after the refusals, not at its cap", prov.calls())
	}
}

// countingNoop succeeds every time and counts its runs.
type countingNoop struct{ calls int }

func (c *countingNoop) Name() string                 { return "Memory" }
func (c *countingNoop) Description() string          { return "stores things" }
func (c *countingNoop) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *countingNoop) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	c.calls++
	return tools.Result{Text: `{"ok":true}`}, nil
}

// The measured case: a stateful run re-sending the same SUCCESSFUL save, back
// to back (36 times live, until the deadline). The third in a row is refused
// unrun, and a model that keeps sending it is stopped rather than left to spin.
func TestRun_Stateful_ARunRepeatingASuccessfulCallIsStopped(t *testing.T) {
	step := `{"patch":{},"action":{"tool":"Memory","input":{"op":"set","scope":"tenant","key":"k","value":"v"}}}`
	scripts := make([]string, 30)
	for i := range scripts {
		scripts[i] = step
	}
	prov := &statefulScriptProvider{scripts: scripts}
	mem := &countingNoop{}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{mem},
		Dispatcher: tools.NewDispatcher([]tools.Tool{mem}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	})
	if err == nil || res.StopReason != StopReasonRepeatedFailedCall {
		t.Fatalf("stop=%q err=%v; want the run stopped", res.StopReason, err)
	}
	if mem.calls != 2 {
		t.Errorf("the tool ran %d times; want 2 (every repeat after that is refused, not run)", mem.calls)
	}
	if prov.calls() > 10 {
		t.Errorf("%d model calls; the run should stop soon after the refusals, not at its cap", prov.calls())
	}
}

// turnScriptProvider sends one assistant turn per entry of turns — the tool
// calls in it — and then ends the run with a text answer.
type turnScriptProvider struct {
	mu    sync.Mutex
	turns [][]providers.ToolUse
	turn  int
}

func (p *turnScriptProvider) ID() string                                   { return "turn-script" }
func (p *turnScriptProvider) Probe(context.Context) error                  { return nil }
func (p *turnScriptProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *turnScriptProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *turnScriptProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	i := p.turn
	p.turn++
	p.mu.Unlock()
	ch := make(chan providers.Event, 16)
	if i < len(p.turns) {
		for _, tu := range p.turns[i] {
			tu := tu
			ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &tu}
		}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "done"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

// sameCallTurns is n turns of one identical call each.
func sameCallTurns(n int, name, input string) [][]providers.ToolUse {
	turns := make([][]providers.ToolUse, n)
	for i := range turns {
		turns[i] = []providers.ToolUse{{ID: fmt.Sprintf("t%d", i), Name: name, Input: json.RawMessage(input)}}
	}
	return turns
}

// pollingChild is a resident sub-agent seen through Agent op=poll: its output
// grows on every poll until its turn is done.
type pollingChild struct {
	mu     sync.Mutex
	calls  int
	doneAt int
}

func (p *pollingChild) Name() string                 { return "Agent" }
func (p *pollingChild) Description() string          { return "runs sub-agents" }
func (p *pollingChild) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *pollingChild) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	state := "running"
	if p.calls >= p.doneAt {
		state = "awaiting_input"
	}
	return tools.Result{Text: fmt.Sprintf(`{"child_run_id":"r_1","state":%q,"output":"%d sections written"}`, state, p.calls)}, nil
}

// A parent waiting on a resident child polls it with the same arguments, as the
// poll help says to, and gets a new answer each time. Every poll runs, and the
// run ends on the model's answer — measured before, the third poll was refused
// and the seventh ended the run repeated_failed_call.
func TestRun_APollRepeatedWithChangingResultsRunsEveryTime(t *testing.T) {
	const polls = 8
	child := &pollingChild{doneAt: polls}
	prov := &turnScriptProvider{turns: sameCallTurns(polls, "Agent", `{"op":"poll","child_run_id":"r_1","timeout_ms":60000}`)}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{child},
		Dispatcher: tools.NewDispatcher([]tools.Tool{child}),
		Segments:   statefulTaskSegs(),
	})
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("stop=%q err=%v; want end_turn", res.StopReason, err)
	}
	if child.calls != polls {
		t.Errorf("the poll ran %d times; want %d (every poll returned something new)", child.calls, polls)
	}
}

// The case the guard exists for, in the ordinary loop: the same successful
// save, with the same result every time. The third is refused unrun and a
// model that keeps sending it is stopped.
func TestRun_ARepeatedCallWithTheSameResultIsStillStopped(t *testing.T) {
	mem := &countingNoop{}
	prov := &turnScriptProvider{turns: sameCallTurns(20, "Memory", `{"op":"set","scope":"tenant","key":"k","value":"v"}`)}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{mem},
		Dispatcher: tools.NewDispatcher([]tools.Tool{mem}),
		Segments:   statefulTaskSegs(),
	})
	if err == nil || res.StopReason != StopReasonRepeatedFailedCall {
		t.Fatalf("stop=%q err=%v; want repeated_failed_call", res.StopReason, err)
	}
	if mem.calls != 2 {
		t.Errorf("the tool ran %d times; want 2 (the repeats after that are refused, not run)", mem.calls)
	}
}

// lockedNoop is countingNoop safe for a turn's concurrent calls.
type lockedNoop struct {
	mu    sync.Mutex
	calls int
}

func (c *lockedNoop) Name() string                 { return "Memory" }
func (c *lockedNoop) Description() string          { return "stores things" }
func (c *lockedNoop) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *lockedNoop) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return tools.Result{Text: `{"ok":true}`}, nil
}

// Three identical calls in ONE turn all run: the model sent them together,
// before seeing any result, so none is a repeat of a result it already had.
// Sending the same call again in the next turn is refused. One call at a time,
// so each has finished before the next starts: run concurrently, all three
// could start before any result is in, and the test would pass either way.
func TestRun_IdenticalCallsInOneTurnAllRun(t *testing.T) {
	const in = `{"op":"set","scope":"tenant","key":"k","value":"v"}`
	mem := &lockedNoop{}
	calls := sameCallTurns(4, "Memory", in)
	prov := &turnScriptProvider{turns: [][]providers.ToolUse{
		{calls[0][0], calls[1][0], calls[2][0]},
		calls[3],
	}}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{mem},
		Dispatcher: tools.NewDispatcher([]tools.Tool{mem}),
		Segments:   statefulTaskSegs(),
		// See above.
		ToolParallelism: 1,
	})
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("stop=%q err=%v; want end_turn", res.StopReason, err)
	}
	if mem.calls != 3 {
		t.Errorf("the tool ran %d times; want 3 (the whole first turn, not the repeat after it)", mem.calls)
	}
}
