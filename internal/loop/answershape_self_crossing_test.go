// An EXTERNAL test package, for the same reason as the stateful crossing test:
// it runs the loop with the real Context tool, which reaches this package
// through the error classifier.
package loop_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// selfThenAnswer calls Context op=self on its first turn and answers on the
// second. It claims no tool-choice or structured-output support, so the run's
// choice is only asked for and its schema goes in the prompt.
type selfThenAnswer struct {
	mu    sync.Mutex
	calls int
}

func (p *selfThenAnswer) ID() string                                   { return "self-then-answer" }
func (p *selfThenAnswer) Probe(context.Context) error                  { return nil }
func (p *selfThenAnswer) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *selfThenAnswer) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *selfThenAnswer) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	first := p.calls == 0
	p.calls++
	p.mu.Unlock()
	ch := make(chan providers.Event, 3)
	if first {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
			ID: "t1", Name: "Context", Input: json.RawMessage(`{"op":"self"}`)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: `{"city":"Kyiv"}`}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

// THE CROSSING: the loop holds the run's tool_choice and output_format, and
// the real Context tool must report them. Each half can be tested alone and
// both pass while op=self says nothing, which is the gap this closes.
//
// The first call was forced and op=self runs after it, so the choice must read
// as SPENT — a value captured before the model call would still say in effect.
func TestRun_ContextSelfReportsToolChoiceAndOutputFormat(t *testing.T) {
	ctxTool := &builtin.Context{}
	order := []tools.Tool{ctxTool}
	var mu sync.Mutex
	var selfText string
	_, err := loop.Run(context.Background(), loop.RunOptions{
		Provider:   &selfThenAnswer{},
		Model:      "m",
		Tools:      order,
		Dispatcher: tools.NewDispatcher(order),
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "where?"}}}},
		ToolChoice:   &config.ToolChoice{Mode: "tool", Name: "Context"},
		OutputFormat: &config.OutputFormat{Name: "place", Schema: map[string]any{"type": "object"}},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventToolResult && ev.ToolUse != nil && ev.ToolUse.Name == "Context" {
				mu.Lock()
				selfText = ev.Text
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var out struct {
		ToolChoice   *tools.ToolChoiceReport   `json:"tool_choice"`
		OutputFormat *tools.OutputFormatReport `json:"output_format"`
	}
	if err := json.Unmarshal([]byte(selfText), &out); err != nil {
		t.Fatalf("op=self result is not JSON: %v\n%s", err, selfText)
	}
	want := tools.ToolChoiceReport{Mode: "tool", Name: "Context", Until: "first_call", InEffect: false, Enforced: false}
	if out.ToolChoice == nil || *out.ToolChoice != want {
		t.Errorf("op=self tool_choice = %+v, want %+v", out.ToolChoice, want)
	}
	if of := out.OutputFormat; of == nil || of.Name != "place" || of.Type != "json_schema" ||
		of.Enforcement != tools.EnforcementPrompt || of.Schema["type"] != "object" {
		t.Errorf("op=self output_format = %+v, want place/json_schema enforced by the prompt", of)
	}
}

// A run with neither set reports neither: the keys are for runs that have them.
func TestRun_ContextSelfOmitsAnUnsetShape(t *testing.T) {
	ctxTool := &builtin.Context{}
	order := []tools.Tool{ctxTool}
	var selfText string
	var mu sync.Mutex
	if _, err := loop.Run(context.Background(), loop.RunOptions{
		Provider:   &selfThenAnswer{},
		Model:      "m",
		Tools:      order,
		Dispatcher: tools.NewDispatcher(order),
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "where?"}}}},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventToolResult && ev.ToolUse != nil && ev.ToolUse.Name == "Context" {
				mu.Lock()
				selfText = ev.Text
				mu.Unlock()
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(selfText), &out); err != nil {
		t.Fatalf("op=self result is not JSON: %v\n%s", err, selfText)
	}
	for _, k := range []string{"tool_choice", "output_format"} {
		if _, ok := out[k]; ok {
			t.Errorf("op=self reported %s for a run that has none: %v", k, out[k])
		}
	}
}
