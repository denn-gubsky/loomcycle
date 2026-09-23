package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// turnsProvider plays one scripted turn per call — each turn's tool calls, or a
// final answer when a turn has none — and records every request it received.
// forcing controls whether it claims the wire parameter.
type turnsProvider struct {
	mu       sync.Mutex
	turns    [][]providers.ToolUse
	requests []providers.Request
	forcing  bool
}

func (p *turnsProvider) ID() string                                   { return "turns" }
func (p *turnsProvider) Probe(context.Context) error                  { return nil }
func (p *turnsProvider) ListModels(context.Context) ([]string, error) { return []string{"m"}, nil }
func (p *turnsProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsToolChoice: p.forcing}
}
func (p *turnsProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	turn := len(p.requests)
	p.requests = append(p.requests, req)
	ch := make(chan providers.Event, 8)
	if turn < len(p.turns) && len(p.turns[turn]) > 0 {
		for _, tu := range p.turns[turn] {
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

func call(name string) providers.ToolUse {
	return providers.ToolUse{ID: "id_" + name, Name: name, Input: json.RawMessage(`{}`)}
}

func runWithChoice(t *testing.T, prov *turnsProvider, tc *config.ToolChoice) ([]providers.Event, error) {
	t.Helper()
	search := &fakeWebFetch{result: tools.Result{Text: "page"}}
	echo := &echoTool{reply: "echoed"}
	var events []providers.Event
	var mu sync.Mutex
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "m",
		Tools:      []tools.Tool{search, echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{search, echo}),
		Segments:   []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolChoice: tc,
		OnEvent: func(ev providers.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	return events, err
}

func choices(prov *turnsProvider) []providers.ToolChoice {
	out := make([]providers.ToolChoice, len(prov.requests))
	for i, r := range prov.requests {
		out[i] = r.ToolChoice
	}
	return out
}

// first_call forces the run's first call and releases every call after it, so
// the model can finish.
func TestToolChoice_FirstCallAppliesOnceThenReleases(t *testing.T) {
	prov := &turnsProvider{forcing: true, turns: [][]providers.ToolUse{{call("WebFetch")}}}
	if _, err := runWithChoice(t, prov, &config.ToolChoice{Mode: "tool", Name: "WebFetch"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := choices(prov)
	if len(got) != 2 || got[0] != (providers.ToolChoice{Mode: "tool", Name: "WebFetch"}) || got[1].Forces() {
		t.Errorf("per-call choices = %+v, want [tool:WebFetch, auto]", got)
	}
}

// until_called keeps forcing while the model calls OTHER tools, and releases on
// the first turn that makes the call it asked for.
func TestToolChoice_UntilCalledHoldsUntilSatisfied(t *testing.T) {
	prov := &turnsProvider{forcing: true, turns: [][]providers.ToolUse{{call("Echo")}, {call("WebFetch")}}}
	if _, err := runWithChoice(t, prov, &config.ToolChoice{Mode: "tool", Name: "WebFetch", Until: "until_called"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := choices(prov)
	want := []bool{true, true, false}
	if len(got) != len(want) {
		t.Fatalf("made %d calls, want %d: %+v", len(got), len(want), got)
	}
	for i, forced := range want {
		if got[i].Forces() != forced {
			t.Errorf("call %d choice = %+v, want forced=%v", i, got[i], forced)
		}
	}
}

// A choice naming a tool the run lacks could never be satisfied; the run is
// refused before any model call rather than spending one on it.
func TestToolChoice_UnknownToolIsRefusedBeforeAnyCall(t *testing.T) {
	prov := &turnsProvider{forcing: true}
	_, err := runWithChoice(t, prov, &config.ToolChoice{Mode: "tool", Name: "Bash"})
	if err == nil || !strings.Contains(err.Error(), `"Bash"`) {
		t.Errorf("err = %v, want a refusal naming the missing tool", err)
	}
	if len(prov.requests) != 0 {
		t.Errorf("made %d model calls before refusing", len(prov.requests))
	}
}

// A target that cannot enforce the choice still runs — forcing is an
// optimisation of what the prompt asks — but says so, ONCE, naming the gate.
func TestToolChoice_UnenforceableTargetIsReportedOnce(t *testing.T) {
	prov := &turnsProvider{forcing: false, turns: [][]providers.ToolUse{{call("Echo")}, {call("WebFetch")}}}
	events, err := runWithChoice(t, prov, &config.ToolChoice{Mode: "tool", Name: "WebFetch", Until: "until_called"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var reports []*providers.CapabilityInertInfo
	for _, ev := range events {
		if ev.Type == providers.EventCapabilityInert {
			reports = append(reports, ev.CapabilityInert)
		}
	}
	if len(reports) != 1 || reports[0].Gate != "tool_choice" || reports[0].Tool != "WebFetch" {
		t.Errorf("reports = %+v, want exactly one tool_choice report for WebFetch", reports)
	}
	if len(prov.requests) < 2 {
		t.Errorf("the run stopped after %d calls; an unenforced choice must not stop it", len(prov.requests))
	}
}

// Without a tool_choice nothing changes: no forced choice on the wire, and no
// report.
func TestToolChoice_UnsetLeavesEveryCallAuto(t *testing.T) {
	prov := &turnsProvider{forcing: false, turns: [][]providers.ToolUse{{call("Echo")}}}
	events, err := runWithChoice(t, prov, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, c := range choices(prov) {
		if c.Forces() {
			t.Errorf("call %d carried %+v with no tool_choice set", i, c)
		}
	}
	for _, ev := range events {
		if ev.Type == providers.EventCapabilityInert {
			t.Errorf("unexpected report: %+v", ev.CapabilityInert)
		}
	}
}

// A stateful run already forces its own state tool on every step, so a
// tool_choice has nothing to apply to — said, not silently dropped.
func TestToolChoice_StatefulRunReportsItIsNotApplied(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{`{"patch":{"n":1},"done":true,"final":"ok"}`}}
	echo := &echoTool{reply: "observed"}
	var reported bool
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		ToolChoice: &config.ToolChoice{Mode: "required"},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventCapabilityInert && ev.CapabilityInert.Gate == "tool_choice" {
				reported = true
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reported {
		t.Error("a stateful run with a tool_choice did not report that it ignores it")
	}
}
