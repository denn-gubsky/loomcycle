package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// noopTool is a trivial dispatchable tool: it returns {} and records nothing.
type noopTool struct{}

func (noopTool) Name() string                 { return "Noop" }
func (noopTool) Description() string          { return "noop" }
func (noopTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (noopTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: `{}`}, nil
}

// iterCounterProvider emits a tool_use on the first `target` turns, then
// end_turn — i.e. run() "wants" `target` sequential tool calls. unbounded
// controls the UnboundedIterations capability (code-js sets it true).
type iterCounterProvider struct {
	target    int
	unbounded bool
	mu        sync.Mutex
	calls     int
}

func (p *iterCounterProvider) ID() string                  { return "iter-counter" }
func (p *iterCounterProvider) Probe(context.Context) error { return nil }
func (p *iterCounterProvider) ListModels(context.Context) ([]string, error) {
	return []string{}, nil
}
func (p *iterCounterProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, UnboundedIterations: p.unbounded}
}

func (p *iterCounterProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	turn := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan providers.Event, 3)
	if turn < p.target {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
			ID: "t", Name: "Noop", Input: json.RawMessage(`{}`),
		}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "done"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

// A provider that declares UnboundedIterations is NOT capped at MaxIterations:
// it makes far more sequential tool calls than the cap and still completes with
// end_turn. The control case (capability off) stops at the cap. This pins the
// code-js exemption (RFC J) — capping a code-agent's sequential tool calls at
// 16 was unusable; the run is bounded by the provider's timeout instead.
func TestRun_UnboundedIterations_ExemptFromMaxIterations(t *testing.T) {
	const cap, want = 3, 10 // run() wants 10 calls; MaxIterations is 3

	segs := []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}

	// Capability ON → not capped → completes all 10 calls with end_turn.
	provUnbounded := &iterCounterProvider{target: want, unbounded: true}
	res, err := Run(context.Background(), RunOptions{
		Provider:      provUnbounded,
		Model:         "x",
		Tools:         []tools.Tool{noopTool{}},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:      segs,
		MaxIterations: cap,
	})
	if err != nil {
		t.Fatalf("unbounded run errored: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("unbounded: stop = %q, want end_turn (exemption not honored)", res.StopReason)
	}
	if provUnbounded.calls < want {
		t.Errorf("unbounded: made %d calls, want ≥ %d (capped despite UnboundedIterations)", provUnbounded.calls, want)
	}

	// Capability OFF (default, LLM driver) → capped at MaxIterations.
	provCapped := &iterCounterProvider{target: want, unbounded: false}
	res2, _ := Run(context.Background(), RunOptions{
		Provider:      provCapped,
		Model:         "x",
		Tools:         []tools.Tool{noopTool{}},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:      segs,
		MaxIterations: cap,
	})
	if res2.StopReason != "max_iterations" {
		t.Errorf("capped: stop = %q, want max_iterations (cap should still apply to LLM drivers)", res2.StopReason)
	}
	if provCapped.calls > cap {
		t.Errorf("capped: made %d calls, want ≤ %d (cap not enforced)", provCapped.calls, cap)
	}
}
