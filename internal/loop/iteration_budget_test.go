package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// budgetProbeTool records the iteration budget each dispatch sees.
type budgetProbeTool struct {
	seen []tools.IterationBudgetValue
}

func (t *budgetProbeTool) Name() string                 { return "Echo" }
func (t *budgetProbeTool) Description() string          { return "" }
func (t *budgetProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *budgetProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	b, ok := tools.IterationBudget(ctx)
	if !ok {
		b = tools.IterationBudgetValue{Used: -1}
	}
	t.seen = append(t.seen, b)
	return tools.Result{Text: "ok"}, nil
}

// echoTurnsProvider calls Echo on each of its first n turns, then ends.
type echoTurnsProvider struct {
	n, calls int
}

func (p *echoTurnsProvider) ID() string                    { return "scripted" }
func (p *echoTurnsProvider) Probe(_ context.Context) error { return nil }
func (p *echoTurnsProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"scripted-model"}, nil
}
func (p *echoTurnsProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *echoTurnsProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	turn := p.calls
	p.calls++
	ch := make(chan providers.Event, 3)
	if turn < p.n {
		// A distinct input per turn: the loop refuses a repeat of the same call.
		in := json.RawMessage(fmt.Sprintf(`{"turn":%d}`, turn))
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: fmt.Sprintf("call_%d", turn), Name: "Echo", Input: in}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "done"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

// A tool dispatched in iteration k sees k iterations used against the cap the
// loop enforces, so Context op=self can say how many turns are left.
func TestRun_ToolSeesTheIterationBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		max       int
		unbounded bool
		wantMax   int
	}{
		{"capped", 5, false, 5},
		{"default cap", 0, false, DefaultMaxIterations},
		{"unbounded", 5, true, maxIterationsHardCeiling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &budgetProbeTool{}
			if _, err := Run(context.Background(), RunOptions{
				Provider:            &echoTurnsProvider{n: 3},
				Model:               "scripted-model",
				Tools:               []tools.Tool{probe},
				Dispatcher:          tools.NewDispatcher([]tools.Tool{probe}),
				Segments:            []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
				MaxIterations:       tc.max,
				UnboundedIterations: tc.unbounded,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(probe.seen) != 3 {
				t.Fatalf("dispatches = %d, want 3", len(probe.seen))
			}
			for i, b := range probe.seen {
				want := tools.IterationBudgetValue{Used: i + 1, Max: tc.wantMax, Unbounded: tc.unbounded}
				if b != want {
					t.Errorf("dispatch %d saw %+v, want %+v", i, b, want)
				}
			}
		})
	}
}

// A stateful run's actions see the same budget: the step counter against the
// cap Run handed it.
func TestRun_Stateful_ActionSeesTheIterationBudget(t *testing.T) {
	probe := &budgetProbeTool{}
	prov := &actionScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Echo","input":{"step":1}}}`,
		`{"patch":{},"action":{"tool":"Echo","input":{"step":2}}}`,
		`{"patch":{},"done":true,"final":"ok"}`,
	}}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:         []tools.Tool{probe},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{probe}),
		Segments:      statefulTaskSegs(),
		Context:       statefulCtx(nil),
		MaxIterations: 7,
		OnEvent:       func(providers.Event) {},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []tools.IterationBudgetValue{{Used: 1, Max: 7}, {Used: 2, Max: 7}}
	if len(probe.seen) != len(want) {
		t.Fatalf("actions = %d (%+v), want %d", len(probe.seen), probe.seen, len(want))
	}
	for i := range want {
		if probe.seen[i] != want[i] {
			t.Errorf("action %d saw %+v, want %+v", i, probe.seen[i], want[i])
		}
	}
}
