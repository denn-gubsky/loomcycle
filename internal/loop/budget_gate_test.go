package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// budgetAfter is a BudgetGate that allows n checks and refuses from then on.
func budgetAfter(n int, refusal error) (gate func() error, asked *int) {
	asked = new(int)
	return func() error {
		*asked++
		if *asked > n {
			return refusal
		}
		return nil
	}, asked
}

// A stateful run asks its budget before every model call, as an append run
// does: the two loops are separate code, and a check in one says nothing
// about the other.
func TestRun_Stateful_StopsAtTheBudgetBeforeTheNextModelCall(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{},"done":true,"final":"fin"}`,
	}}
	echo := &echoTool{reply: "ok"}
	spent := errors.New("budget spent")
	gate, asked := budgetAfter(1, spent)
	var errEvents []string
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		BudgetGate: gate,
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventError {
				errEvents = append(errEvents, ev.Error)
			}
		},
	})
	if !errors.Is(err, spent) {
		t.Fatalf("Run = %v, want the gate's error", err)
	}
	if res.StopReason != StopReasonTokenLimit {
		t.Errorf("stop = %q, want %q", res.StopReason, StopReasonTokenLimit)
	}
	if *asked != 2 {
		t.Errorf("the budget was asked %d times, want 2 (once per model call attempted)", *asked)
	}
	if n := len(prov.requests); n != 1 {
		t.Errorf("model calls = %d, want 1", n)
	}
	if got, _ := res.State["count"].(float64); got != 1 {
		t.Errorf("state = %v, want what the run had reached", res.State)
	}
	if len(errEvents) != 1 || errEvents[0] != spent.Error() {
		t.Errorf("error events = %q, want the one refusal", errEvents)
	}
}

// An append run that was allowed its first call and refused its second ends
// there, with what it had spent.
func TestRun_StopsAtTheBudgetBeforeTheNextModelCall(t *testing.T) {
	echo := &echoTool{reply: "ok"}
	prov := &toolThenEndProvider{tool: "Echo", usage: providers.Usage{InputTokens: 7}}
	spent := errors.New("budget spent")
	gate, _ := budgetAfter(1, spent)
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:           []tools.Tool{echo},
		Dispatcher:      tools.NewDispatcher([]tools.Tool{echo}),
		Segments:        userSeg("go"),
		ToolParallelism: 8,
		BudgetGate:      gate,
	})
	if !errors.Is(err, spent) {
		t.Fatalf("Run = %v, want the gate's error", err)
	}
	if res.StopReason != StopReasonTokenLimit || res.Usage.InputTokens != 7 {
		t.Errorf("result = %+v, want stop %q with the first call's usage", res, StopReasonTokenLimit)
	}
	if prov.calls != 1 {
		t.Errorf("model calls = %d, want 1", prov.calls)
	}
}

// toolThenEndProvider calls one tool on its first call and answers on every
// later one.
type toolThenEndProvider struct {
	tool  string
	usage providers.Usage
	calls int
}

func (p *toolThenEndProvider) ID() string                                   { return "scripted" }
func (p *toolThenEndProvider) Probe(context.Context) error                  { return nil }
func (p *toolThenEndProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *toolThenEndProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *toolThenEndProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.calls++
	u := p.usage
	evs := []providers.Event{{Type: providers.EventText, Text: "fin"}, {Type: providers.EventDone, StopReason: "end_turn", Usage: &u}}
	if p.calls == 1 {
		evs = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: p.tool, Input: []byte(`{}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &u},
		}
	}
	ch := make(chan providers.Event, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// A stateful step re-prompts a model that did not answer in the required
// form, inside one iteration. Each re-prompt is a paid call, so each asks.
func TestRun_Stateful_AskForTheBudgetBeforeAReprompt(t *testing.T) {
	prov := &proseThenComplyProvider{
		proseTurns: 5,
		prose:      "Hello! I can search the web.",
		script:     `{"patch":{},"done":true,"final":"hi"}`,
	}
	spent := errors.New("budget spent")
	gate, _ := budgetAfter(1, spent)
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		BudgetGate: gate,
	})
	if !errors.Is(err, spent) || res.StopReason != StopReasonTokenLimit {
		t.Fatalf("Run = %q, %v; want %q and the gate's error", res.StopReason, err, StopReasonTokenLimit)
	}
	if n := prov.calls(); n != 1 {
		t.Errorf("model calls = %d, want 1: the re-prompt was sent over budget", n)
	}
}

// A call that failed and is retried on the same provider does not count as
// an attempt, so a check keyed on the attempt number would skip it.
func TestRun_Stateful_AskForTheBudgetBeforeARetriedCall(t *testing.T) {
	prov := &flakyStatefulProvider{id: "p1", fail: 1, err: errors.New("fake 429: rate_limited")}
	spent := errors.New("budget spent")
	gate, _ := budgetAfter(1, spent)
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Segments:               statefulTaskSegs(),
		Context:                statefulCtx(nil),
		MaxSameProviderRetries: 2,
		BudgetGate:             gate,
		OnEvent:                func(providers.Event) {},
	})
	if !errors.Is(err, spent) || res.StopReason != StopReasonTokenLimit {
		t.Fatalf("Run = %q, %v; want %q and the gate's error", res.StopReason, err, StopReasonTokenLimit)
	}
	if prov.calls != 1 {
		t.Errorf("model calls = %d, want 1: the retry was sent over budget", prov.calls)
	}
}
