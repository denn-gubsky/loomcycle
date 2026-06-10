package loop

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// steerProvider records each call's Messages. When `inject` is non-nil it
// pushes `injected` into that channel on turn 0 and returns tool_use (so the
// operator "steers" during a tool round); otherwise it returns end_turn.
type steerProvider struct {
	mu       sync.Mutex
	requests [][]providers.Message
	inject   chan<- steer.Message
	injected steer.Message
	turn     int
}

func (p *steerProvider) ID() string                                   { return "steer-test" }
func (p *steerProvider) Probe(context.Context) error                  { return nil }
func (p *steerProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *steerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *steerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]providers.Message(nil), req.Messages...))
	turn := p.turn
	p.turn++
	p.mu.Unlock()

	ch := make(chan providers.Event, 3)
	if turn == 0 && p.inject != nil {
		p.inject <- p.injected // simulate an operator steering during the tool round
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "t", Name: "Noop", Input: json.RawMessage(`{}`)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

func steerSegs() []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
}

// A pre-queued steering message is drained at the top of the first iteration
// and appears as a user turn in that iteration's provider request.
func TestRun_DrainSteer_AppendsUserTurnBeforeNextCall(t *testing.T) {
	q := make(chan steer.Message, 4)
	q <- steer.Message{Text: "steer-A"}

	var seen []steer.Message
	prov := &steerProvider{} // inject nil → returns end_turn on turn 0
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		SteerQueue: q,
		OnSteer:    func(m steer.Message) { seen = append(seen, m) },
	})
	if err != nil {
		t.Fatalf("run errored: %v", err)
	}
	if len(prov.requests) == 0 {
		t.Fatal("provider never called")
	}
	if !hasUserText(prov.requests[0], "steer-A") {
		t.Errorf("iteration-0 request did not contain the steered user turn; messages=%+v", prov.requests[0])
	}
	if len(seen) != 1 || seen[0].Text != "steer-A" {
		t.Errorf("OnSteer fired %v, want one steer-A", seen)
	}
}

// The 400-prevention guard: a steer drained AFTER a tool round must land as a
// user turn AFTER the tool_results user turn — never inserted between the
// tool_use assistant turn and its tool_results (which orphans the tool_use and
// 400s the provider).
func TestRun_DrainSteer_OrderingWithToolResults(t *testing.T) {
	q := make(chan steer.Message, 4)
	prov := &steerProvider{inject: q, injected: steer.Message{Text: "steer-B"}}

	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		SteerQueue: q,
	})
	if err != nil {
		t.Fatalf("run errored: %v", err)
	}
	if len(prov.requests) < 2 {
		t.Fatalf("expected ≥2 provider calls (tool round + resume), got %d", len(prov.requests))
	}
	msgs := prov.requests[1] // iteration 1: after the tool round + steer drain

	// Find the assistant turn carrying the tool_use; the NEXT message must be
	// the tool_results user turn, and the steer must come strictly after it.
	toolUseIdx := -1
	for i, m := range msgs {
		if m.Role == "assistant" && hasToolUse(m) {
			toolUseIdx = i
			break
		}
	}
	if toolUseIdx < 0 || toolUseIdx+1 >= len(msgs) {
		t.Fatalf("no assistant tool_use turn followed by a result; messages=%+v", msgs)
	}
	next := msgs[toolUseIdx+1]
	if next.Role != "user" || !hasToolResult(next) {
		t.Errorf("turn after tool_use is %+v, want a user turn with tool_results (steer must not split it)", next)
	}
	steerIdx := userTextIdx(msgs, "steer-B")
	if steerIdx <= toolUseIdx+1 {
		t.Errorf("steer turn at idx %d, want strictly after the tool_results turn at %d", steerIdx, toolUseIdx+1)
	}
}

// endTurnProvider always returns end_turn and counts its calls — for the
// persistent-park tests.
type endTurnProvider struct {
	mu sync.Mutex
	n  int
}

func (p *endTurnProvider) ID() string                                   { return "endturn" }
func (p *endTurnProvider) Probe(context.Context) error                  { return nil }
func (p *endTurnProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *endTurnProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *endTurnProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.n }
func (p *endTurnProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// A persistent interactive run parks at end_turn instead of terminating, and
// resumes on the next operator steering message.
func TestRun_Interactive_ParksAtEndTurnUntilInput(t *testing.T) {
	q := make(chan steer.Message, 4)
	parked := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &endTurnProvider{}
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventAwaitingInput {
					parked <- struct{}{}
				}
			},
		})
		close(done)
	}()

	// First end_turn → parked (not terminated).
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not park at end_turn")
	}
	if got := prov.calls(); got != 1 {
		t.Errorf("calls = %d, want 1 before resume", got)
	}
	select {
	case <-done:
		t.Fatal("run terminated instead of parking")
	default:
	}

	// Resume on a steering message → another turn → parks again.
	q <- steer.Message{Text: "keep going"}
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not resume on steering input")
	}
	if got := prov.calls(); got != 2 {
		t.Errorf("calls = %d, want 2 after resume", got)
	}

	// Cancel ends the persistent run.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not terminate after cancel")
	}
}

// Cancelling a parked interactive run terminates it promptly (no hang).
func TestRun_Interactive_CancelWhileParked(t *testing.T) {
	q := make(chan steer.Message, 1)
	parked := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	prov := &endTurnProvider{}
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventAwaitingInput {
					parked <- struct{}{}
				}
			},
		})
		close(done)
	}()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not park")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock the parked run")
	}
}

// While parked, the run pulses OnHeartbeat so the staleness sweeper doesn't
// reap it. Lower the interval so the test doesn't wait the production 30s.
func TestRun_Interactive_HeartbeatFiresWhileParked(t *testing.T) {
	orig := parkHeartbeatInterval
	parkHeartbeatInterval = 5 * time.Millisecond
	defer func() { parkHeartbeatInterval = orig }()

	q := make(chan steer.Message, 1)
	parked := make(chan struct{}, 1)
	var hb int32
	ctx, cancel := context.WithCancel(context.Background())
	prov := &endTurnProvider{}
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			OnHeartbeat: func() { atomic.AddInt32(&hb, 1) },
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventAwaitingInput {
					select {
					case parked <- struct{}{}:
					default:
					}
				}
			},
		})
		close(done)
	}()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not park")
	}
	// Give the park heartbeat ticker a few cycles.
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	// At least the per-iteration heartbeat (1) plus several park ticks.
	if got := atomic.LoadInt32(&hb); got < 2 {
		t.Errorf("heartbeat fired %d times, want ≥2 (park ticker not pulsing)", got)
	}
}

func hasUserText(msgs []providers.Message, text string) bool {
	return userTextIdx(msgs, text) >= 0
}

func userTextIdx(msgs []providers.Message, text string) int {
	for i, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && c.Text == text {
				return i
			}
		}
	}
	return -1
}

func hasToolUse(m providers.Message) bool {
	for _, c := range m.Content {
		if c.ToolName != "" || c.ToolInput != nil {
			return true
		}
	}
	return false
}

func hasToolResult(m providers.Message) bool {
	for _, c := range m.Content {
		if c.ToolUseID != "" {
			return true
		}
	}
	return false
}
