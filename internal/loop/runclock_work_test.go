package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// workClockProvider is scriptedProvider on a run-time budget (as code-js is): the
// loop gives its runs a RunClock, and it records the clock's state at each call.
type workClockProvider struct {
	*scriptedProvider
	mu     sync.Mutex
	states []providers.RunClockState
}

func (p *workClockProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, UnboundedIterations: true}
}

func (p *workClockProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.states = append(p.states, providers.RunClockFromContext(ctx).State())
	p.mu.Unlock()
	return p.scriptedProvider.Call(ctx, req)
}

// waitTool blocks for {"ms"} as a wait on the run's clock, like a channel
// await or an Agent wait.
type waitTool struct{}

func (waitTool) Name() string                 { return "Wait" }
func (waitTool) Description() string          { return "" }
func (waitTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (waitTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		MS int `json:"ms"`
	}
	_ = json.Unmarshal(input, &args)
	endWait := providers.BeginWait(ctx)
	defer endWait()
	select {
	case <-time.After(time.Duration(args.MS) * time.Millisecond):
	case <-ctx.Done():
	}
	return tools.Result{Text: "waited"}, nil
}

// runOneTurnOfTools runs one turn issuing calls in parallel, then a final
// turn, and returns the run's clock as the final turn saw it.
func runOneTurnOfTools(t *testing.T, calls []providers.ToolUse) providers.RunClockState {
	t.Helper()
	ts := []tools.Tool{newSlowTool(), waitTool{}}
	prov := &workClockProvider{scriptedProvider: &scriptedProvider{toolCalls: calls}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, RunOptions{
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
		t.Fatalf("stop = %q, want end_turn", res.StopReason)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.states) != 2 {
		t.Fatalf("provider saw %d calls, want 2", len(prov.states))
	}
	return prov.states[1]
}

// A tool call that runs beside a wait in the same turn is charged to a
// budgeted run: the wait does not stop the clock while the other call works.
func TestRunClock_ToolBesideAWaitInOneTurnIsCharged(t *testing.T) {
	got := runOneTurnOfTools(t, []providers.ToolUse{
		{ID: "w", Name: "Wait", Input: json.RawMessage(`{"ms":300}`)},
		{ID: "s", Name: "Slow", Input: json.RawMessage(`{"id":"s","ms":300}`)},
	})
	// The Slow call's whole 300ms runs as work, so active is at least that;
	// unmarked, it hid behind the wait and active was a few ms.
	if got.Active < 300*time.Millisecond {
		t.Fatalf("active = %s, want ≥ 300ms: the call beside the wait ran free (state %+v)", got.Active, got)
	}
}

// Two waits in one turn still stop the clock, and count once.
func TestRunClock_TwoWaitsInOneTurnChargeNothing(t *testing.T) {
	got := runOneTurnOfTools(t, []providers.ToolUse{
		{ID: "a", Name: "Wait", Input: json.RawMessage(`{"ms":300}`)},
		{ID: "b", Name: "Wait", Input: json.RawMessage(`{"ms":300}`)},
	})
	if got.Active >= 250*time.Millisecond {
		t.Fatalf("active = %s, want < 250ms: waiting calls were charged (state %+v)", got.Active, got)
	}
	if got.Waited < 250*time.Millisecond || got.Waited >= 550*time.Millisecond {
		t.Fatalf("waited = %s, want ≈ 300ms (overlapping waits count once)", got.Waited)
	}
}
