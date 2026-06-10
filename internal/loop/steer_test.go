package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

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
