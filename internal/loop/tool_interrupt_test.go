package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// interruptGate is a pause gate whose runtime is shutting down: it asks for a
// pause once its tool interrupt fires, and records the park.
type interruptGate struct {
	interrupt context.Context
	parked    chan struct{}
	once      sync.Once
}

func (g *interruptGate) PauseRequested() bool { return g.interrupt.Err() != nil }

func (g *interruptGate) Park(ctx context.Context) error {
	g.once.Do(func() { close(g.parked) })
	<-ctx.Done()
	return ctx.Err()
}

func (g *interruptGate) ToolInterrupt() context.Context { return g.interrupt }

// A run whose tool call is interrupted by a shutdown records the call as
// failed — saying why, in the persisted event the resumed run replays — and
// parks at its next boundary instead of calling the model again.
func TestRun_ToolInterruptFailsTheCallAndParks(t *testing.T) {
	interrupt, fire := context.WithCancel(context.Background())
	defer fire()
	gate := &interruptGate{interrupt: interrupt, parked: make(chan struct{})}
	sink := newEventSink()
	started := make(chan struct{}, 1)
	prov := &heldProvider{
		resumeText: "should not be called",
		turn0: func(_ context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{ID: "t1", Name: "Block", Input: json.RawMessage(`{}`)}}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 5}}
		},
	}
	ts := []tools.Tool{blockingTool{started: started}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:   prov,
			Model:      "x",
			Tools:      ts,
			Dispatcher: dispatcherFor(ts),
			Segments:   userSeg("go"),
			OnEvent:    sink.emit,
			PauseGate:  gate,
		})
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool never started")
	}
	fire()

	res := sink.waitFor(t, providers.EventToolResult, 5*time.Second)
	if !res.IsError || !strings.Contains(res.Text, "shut down") {
		t.Errorf("interrupted tool result = %+v, want an error naming the shutdown", res)
	}
	select {
	case <-gate.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not park after its tool call was interrupted")
	}
	if n := prov.callCount(); n != 1 {
		t.Errorf("model called %d times, want 1 (the run parks before its next call)", n)
	}
	cancel()
	<-done
}

// A run whose interrupt has already fired when its model asks for a tool does
// not start the tool: the call fails at once.
func TestRun_ToolInterruptAlreadyFiredRunsNoTool(t *testing.T) {
	interrupt, fire := context.WithCancel(context.Background())
	fire()
	gate := &interruptGate{interrupt: interrupt, parked: make(chan struct{})}
	sink := newEventSink()
	ran := make(chan struct{}, 1)
	prov := &heldProvider{
		resumeText: "should not be called",
		turn0: func(_ context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{ID: "t1", Name: "Probe", Input: json.RawMessage(`{}`)}}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
		},
	}
	ts := []tools.Tool{startProbeTool{ran: ran}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The gate pauses from the start, so the run would park before its first
	// call; this gate only asks once a tool result is in.
	g := &firstCallThenPause{interruptGate: gate}
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:   prov,
			Model:      "x",
			Tools:      ts,
			Dispatcher: dispatcherFor(ts),
			Segments:   userSeg("go"),
			OnEvent:    sink.emit,
			PauseGate:  g,
		})
		close(done)
	}()
	res := sink.waitFor(t, providers.EventToolResult, 5*time.Second)
	if !res.IsError || !strings.Contains(res.Text, "shut down") {
		t.Errorf("tool result = %+v, want an error naming the shutdown", res)
	}
	select {
	case <-ran:
		t.Error("the tool ran with its interrupt already fired")
	default:
	}
	cancel()
	<-done
}

// firstCallThenPause asks for no pause before the first model call.
type firstCallThenPause struct {
	*interruptGate
	asked int
}

func (g *firstCallThenPause) PauseRequested() bool {
	g.asked++
	return g.asked > 1
}

// startProbeTool runs only when its ctx is live, as a command started with a
// cancelled ctx is never started.
type startProbeTool struct{ ran chan struct{} }

func (startProbeTool) Name() string                 { return "Probe" }
func (startProbeTool) Description() string          { return "records whether it ran" }
func (startProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p startProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{Text: "not started: " + err.Error(), IsError: true}, nil
	}
	p.ran <- struct{}{}
	return tools.Result{Text: "ran"}, nil
}

// A gate with no interrupter leaves tool calls as they were.
func TestRun_NoToolInterrupterRunsToolsUnchanged(t *testing.T) {
	sink := newEventSink()
	ran := make(chan struct{}, 1)
	prov := &heldProvider{
		resumeText: "done",
		turn0: func(_ context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{ID: "t1", Name: "Probe", Input: json.RawMessage(`{}`)}}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
		},
	}
	ts := []tools.Tool{startProbeTool{ran: ran}}
	res, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      ts,
		Dispatcher: dispatcherFor(ts),
		Segments:   userSeg("go"),
		OnEvent:    sink.emit,
		PauseGate:  newFakePauseGateNever(),
	})
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	select {
	case <-ran:
	default:
		t.Error("the tool did not run")
	}
}

type neverPausingGate struct{}

func (neverPausingGate) PauseRequested() bool       { return false }
func (neverPausingGate) Park(context.Context) error { return nil }

func newFakePauseGateNever() PauseGate { return neverPausingGate{} }

// The stateful twin: a stateful run's action is interrupted the same way —
// the call fails naming the shutdown, and the run parks before its next step.
func TestRun_Stateful_ToolInterruptFailsTheActionAndParks(t *testing.T) {
	interrupt, fire := context.WithCancel(context.Background())
	defer fire()
	gate := &interruptGate{interrupt: interrupt, parked: make(chan struct{})}
	sink := newEventSink()
	started := make(chan struct{}, 1)
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"step":0},"action":{"tool":"Block","input":{}}}`,
		`{"patch":{"step":1},"done":true,"final":"should not be reached"}`,
	}}
	ts := []tools.Tool{blockingTool{started: started}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:   prov,
			Model:      "x",
			Tools:      ts,
			Dispatcher: dispatcherFor(ts),
			Segments:   statefulTaskSegs(),
			Context:    statefulCtx(nil),
			OnEvent:    sink.emit,
			PauseGate:  gate,
		})
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the action never started")
	}
	fire()

	res := sink.waitFor(t, providers.EventToolResult, 5*time.Second)
	if !res.IsError || !strings.Contains(res.Text, "shut down") {
		t.Errorf("interrupted action result = %+v, want an error naming the shutdown", res)
	}
	select {
	case <-gate.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the stateful run did not park after its action was interrupted")
	}
	if n := prov.calls(); n != 1 {
		t.Errorf("model called %d times, want 1 (the run parks before its next step)", n)
	}
	cancel()
	<-done
}
