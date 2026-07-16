package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// heldProvider runs a caller-supplied turn0 function on the first Call (so a
// test can hold the model mid-generation), then emits resumeText + end_turn on
// every later turn. It records each call's Messages so a test can assert the
// history the NEXT turn was handed.
type heldProvider struct {
	mu         sync.Mutex
	requests   [][]providers.Message
	turn       int
	turn0      func(ctx context.Context, ch chan<- providers.Event)
	resumeText string
}

func (p *heldProvider) ID() string                                   { return "scripted" }
func (p *heldProvider) Probe(context.Context) error                  { return nil }
func (p *heldProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *heldProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *heldProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]providers.Message(nil), req.Messages...))
	turn := p.turn
	p.turn++
	p.mu.Unlock()

	ch := make(chan providers.Event)
	go func() {
		defer close(ch)
		if turn == 0 && p.turn0 != nil {
			p.turn0(ctx, ch)
			return
		}
		ch <- providers.Event{Type: providers.EventText, Text: p.resumeText}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}()
	return ch, nil
}

func (p *heldProvider) reqAt(i int) []providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.requests) {
		return nil
	}
	return p.requests[i]
}

func (p *heldProvider) callCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.requests) }

// blockingTool blocks in Execute until its ctx is cancelled, then returns an
// error-shaped result — the shape a ctx-cancelled tool produces under a
// turn-cancel mid-dispatch. It signals `started` once so the test can fire the
// cancel only after dispatch is genuinely in-flight.
type blockingTool struct{ started chan struct{} }

func (blockingTool) Name() string                 { return "Block" }
func (blockingTool) Description() string          { return "blocks until ctx is cancelled" }
func (blockingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b blockingTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return tools.Result{Text: "tool cancelled: " + ctx.Err().Error(), IsError: true}, nil
}

// eventSink buffers events off the loop goroutine and lets a test wait for a
// specific type without blocking the loop.
type eventSink struct{ ch chan providers.Event }

func newEventSink() *eventSink { return &eventSink{ch: make(chan providers.Event, 512)} }
func (s *eventSink) emit(ev providers.Event) {
	select {
	case s.ch <- ev:
	default: // never wedge the loop if a test stops draining
	}
}

// waitFor drains events until one of type want arrives, returning it. Fails the
// test on timeout.
func (s *eventSink) waitFor(t *testing.T, want providers.EventType, timeout time.Duration) providers.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-s.ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}

// assertNoDanglingToolUse asserts every assistant tool_use block has a matching
// tool_result in the immediately-following user turn — the invariant a
// turn-cancel must preserve so the next model call doesn't 400 (RFC BH §9.1).
func assertNoDanglingToolUse(t *testing.T, msgs []providers.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		var ids []string
		for _, c := range m.Content {
			if c.Type == "tool_use" {
				ids = append(ids, c.ToolUseID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].Role != "user" {
			t.Fatalf("assistant tool_use at %d not followed by a user tool_result turn; msgs=%+v", i, msgs)
		}
		got := map[string]bool{}
		for _, c := range msgs[i+1].Content {
			if c.Type == "tool_result" {
				got[c.ToolUseID] = true
			}
		}
		for _, id := range ids {
			if !got[id] {
				t.Fatalf("tool_use %q has no tool_result in the next turn; msgs=%+v", id, msgs)
			}
		}
	}
}

func turnCancelOpts(prov providers.Provider, tools []tools.Tool, q chan steer.Message, armCh chan context.CancelCauseFunc, sink *eventSink) RunOptions {
	return RunOptions{
		Provider:    prov,
		Model:       "x",
		Tools:       tools,
		Dispatcher:  dispatcherFor(tools),
		Segments:    steerSegs(),
		SteerQueue:  q,
		Interactive: true,
		OnEvent:     sink.emit,
		ArmTurnCancel: func(tc context.CancelCauseFunc) func() {
			armCh <- tc
			return func() {}
		},
	}
}

func dispatcherFor(ts []tools.Tool) *tools.Dispatcher { return tools.NewDispatcher(ts) }

// A turn-cancel mid-generation parks the run at awaiting_input (it does NOT
// terminate), keeps the partial assistant output, emits turn_cancelled, and the
// next operator message continues the run.
func TestRun_TurnCancel_MidGeneration_ParksAndResumes(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	started := make(chan struct{})
	prov := &heldProvider{
		resumeText: "resumed",
		turn0: func(ctx context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventText, Text: "partial answer"}
			close(started)
			<-ctx.Done() // hold until the operator turn-cancels this turn
			ch <- providers.Event{Type: providers.EventError, Error: "context canceled"}
		},
	}
	ts := []tools.Tool{noopTool{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, turnCancelOpts(prov, ts, q, armCh, sink)); close(done) }()

	tc0 := <-armCh // turn 0's armed token
	<-started      // generation is in-flight
	tc0(ErrTurnCancelled)

	ev := sink.waitFor(t, providers.EventTurnCancelled, 2*time.Second)
	if ev.TurnCancelled == nil {
		t.Fatal("turn_cancelled event carried no payload")
	}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second) // parked, not terminated

	select {
	case <-done:
		t.Fatal("run terminated on a turn-cancel; it must park")
	default:
	}

	// The operator's next message continues the run.
	q <- steer.Message{Text: "continue"}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second) // turn 1 ran, parked again

	if prov.callCount() < 2 {
		t.Fatalf("provider called %d times, want >=2 (resume ran)", prov.callCount())
	}
	req1 := prov.reqAt(1)
	if !hasUserText(req1, "continue") {
		t.Errorf("resume request missing the operator's continue turn; msgs=%+v", req1)
	}
	// Partial assistant output is preserved across the cancel.
	foundPartial := false
	for _, m := range req1 {
		if m.Role == "assistant" {
			for _, c := range m.Content {
				if c.Type == "text" && c.Text == "partial answer" {
					foundPartial = true
				}
			}
		}
	}
	if !foundPartial {
		t.Errorf("partial assistant output not preserved after turn-cancel; msgs=%+v", req1)
	}
	assertNoDanglingToolUse(t, req1)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not terminate after whole-run cancel")
	}
}

// A turn-cancel mid-generation with a tool_use already streamed synthesizes a
// cancelled tool_result so the resumed conversation has no dangling tool_use.
func TestRun_TurnCancel_MidGeneration_SynthesizesToolResult(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	started := make(chan struct{})
	prov := &heldProvider{
		resumeText: "resumed",
		turn0: func(ctx context.Context, ch chan<- providers.Event) {
			// A tool_use streamed but no EventDone / dispatch before the cancel.
			ch <- providers.Event{Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{ID: "t1", Name: "Noop", Input: json.RawMessage(`{}`)}}
			close(started)
			<-ctx.Done()
			ch <- providers.Event{Type: providers.EventError, Error: "context canceled"}
		},
	}
	ts := []tools.Tool{noopTool{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, turnCancelOpts(prov, ts, q, armCh, sink)); close(done) }()

	tc0 := <-armCh
	<-started
	tc0(ErrTurnCancelled)
	sink.waitFor(t, providers.EventTurnCancelled, 2*time.Second)
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)

	q <- steer.Message{Text: "continue"}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)

	req1 := prov.reqAt(1)
	assertNoDanglingToolUse(t, req1) // the synthesized tool_result pairs the tool_use
	// The synthesized result is error-shaped ("cancelled by operator").
	foundCancelled := false
	for _, m := range req1 {
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.ToolUseID == "t1" && c.IsError {
				foundCancelled = true
			}
		}
	}
	if !foundCancelled {
		t.Errorf("no error-shaped cancelled tool_result for the started tool_use; msgs=%+v", req1)
	}

	cancel()
	<-done
}

// A turn-cancel mid-tool-dispatch parks with a VALID history: executePendingTools
// returns one result per pending tool (the cancelled one error-shaped), so the
// next model call has no dangling tool_use.
func TestRun_TurnCancel_MidToolDispatch_ParksWithValidHistory(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	toolStarted := make(chan struct{}, 1)
	prov := &heldProvider{
		resumeText: "resumed",
		turn0: func(_ context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{ID: "t1", Name: "Block", Input: json.RawMessage(`{}`)}}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 5}}
		},
	}
	ts := []tools.Tool{blockingTool{started: toolStarted}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, turnCancelOpts(prov, ts, q, armCh, sink)); close(done) }()

	tc0 := <-armCh
	<-toolStarted // tool dispatch is in-flight
	tc0(ErrTurnCancelled)
	sink.waitFor(t, providers.EventTurnCancelled, 2*time.Second)
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)

	select {
	case <-done:
		t.Fatal("run terminated on a mid-dispatch turn-cancel; it must park")
	default:
	}

	q <- steer.Message{Text: "continue"}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)

	req1 := prov.reqAt(1)
	assertNoDanglingToolUse(t, req1)
	if !hasUserText(req1, "continue") {
		t.Errorf("resume request missing the operator's continue turn; msgs=%+v", req1)
	}

	cancel()
	<-done
}

// Arming a run but never firing the token leaves behavior byte-identical: the
// interactive run parks at end_turn and resumes on input exactly as an
// un-armed run does (the wrapping turn ctx is inert).
func TestRun_TurnCancel_ArmedButNeverFired_BehavesNormally(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	prov := &heldProvider{resumeText: "ok"} // turn0 nil → end_turn immediately
	ts := []tools.Tool{noopTool{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, turnCancelOpts(prov, ts, q, armCh, sink)); close(done) }()

	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second) // parked at first end_turn
	q <- steer.Message{Text: "again"}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second) // resumed + parked again
	if prov.callCount() < 2 {
		t.Fatalf("armed-but-unfired run did not resume normally: calls=%d", prov.callCount())
	}
	cancel()
	<-done
}

// A WHOLE-RUN cancel during generation still TERMINATES the run — it is not a
// turn-cancel (its cause is not ErrTurnCancelled) and must never be mistaken for
// one even when the run is turn-cancellable.
func TestRun_WholeRunCancel_DuringGeneration_Terminates(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	started := make(chan struct{})
	prov := &heldProvider{
		turn0: func(ctx context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventText, Text: "partial"}
			close(started)
			<-ctx.Done()
			ch <- providers.Event{Type: providers.EventError, Error: "context canceled"}
		},
	}
	ts := []tools.Tool{noopTool{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, turnCancelOpts(prov, ts, q, armCh, sink)); close(done) }()

	<-armCh
	<-started
	cancel() // whole-run cancel, NOT the turn token

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("whole-run cancel did not terminate a turn-cancellable run")
	}
	// It must not have parked / emitted a turn_cancelled marker.
	for {
		select {
		case ev := <-sink.ch:
			if ev.Type == providers.EventTurnCancelled {
				t.Fatal("whole-run cancel emitted turn_cancelled (mistaken for a turn-cancel)")
			}
			if ev.Type == providers.EventAwaitingInput {
				t.Fatal("whole-run cancel parked the run instead of terminating it")
			}
		default:
			return
		}
	}
}

// A turn-cancel on a NON-interactive run cannot park (stopping its only turn
// would end it), so the loop's safety net terminates it. The server 409s this
// case before firing, so this only exercises the loop guard.
func TestRun_TurnCancel_NonInteractive_Terminates(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	started := make(chan struct{})
	prov := &heldProvider{
		turn0: func(ctx context.Context, ch chan<- providers.Event) {
			ch <- providers.Event{Type: providers.EventText, Text: "partial"}
			close(started)
			<-ctx.Done()
			ch <- providers.Event{Type: providers.EventError, Error: "context canceled"}
		},
	}
	ts := []tools.Tool{noopTool{}}
	opts := turnCancelOpts(prov, ts, q, armCh, sink)
	opts.Interactive = false // non-interactive, but still armable
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, opts); close(done) }()

	tc0 := <-armCh
	<-started
	tc0(ErrTurnCancelled)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("turn-cancel on a non-interactive run did not terminate (safety net)")
	}
}
