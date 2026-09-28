// An EXTERNAL test package: it runs the stateful loop with the real Context
// tool and help corpus, and builtin reaches this package through the error
// classifier (builtin → errclassify → runner → loop), so an in-package import
// of builtin would be a cycle.
package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// stepScript answers each stateful step with the next emit_state call and
// records every request.
type stepScript struct {
	mu       sync.Mutex
	steps    []string
	requests [][]providers.Message
}

func (p *stepScript) ID() string                                   { return "step-script" }
func (p *stepScript) Probe(context.Context) error                  { return nil }
func (p *stepScript) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *stepScript) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *stepScript) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	i := len(p.requests)
	p.requests = append(p.requests, req.Messages)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if i < len(p.steps) {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: fmt.Sprintf("t%d", i), Name: "emit_state", Input: json.RawMessage(p.steps[i])}}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

// okMemory answers every call.
type okMemory struct{}

func (okMemory) Name() string                 { return "Memory" }
func (okMemory) Description() string          { return "stores things" }
func (okMemory) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (okMemory) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: `{"value":"Helix"}`}, nil
}

// The crossing: the real Context resolves "Memory.set" and "Memory/set" to one
// article and names it in its response, which is what the stateful loop keys
// its kept results on. Read twice, the article appears once on a later step.
func TestRun_Stateful_OneHelpTopicReadTwiceIsKeptOnce(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	art, ok := set.Get("Memory/set")
	if !ok {
		t.Fatal("no Memory/set article")
	}
	firstLine := strings.SplitN(strings.TrimSpace(art.Content), "\n", 2)[0]

	prov := &stepScript{steps: []string{
		`{"patch":{},"action":{"tool":"Context","input":{"op":"help","topic":"Memory.set"}}}`,
		`{"patch":{},"action":{"tool":"Context","input":{"topic":"Memory/set","op":"help"}}}`,
		`{"patch":{},"action":{"tool":"Memory","input":{"op":"get","key":"k"}}}`,
		`{"patch":{},"done":true,"final":"ok"}`,
	}}
	order := []tools.Tool{okMemory{}, &builtin.Context{Help: set}}
	mode := config.ContextModeStateful
	if _, err := loop.Run(context.Background(), loop.RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      order,
		Dispatcher: tools.NewDispatcher(order),
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "save a note"}}}},
		Context: &config.Context{Mode: &mode},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) < 4 {
		t.Fatalf("%d model calls, want 4", len(prov.requests))
	}
	step4 := prov.requests[3]
	var text strings.Builder
	for _, c := range step4[len(step4)-1].Content {
		text.WriteString(c.Text)
	}
	// The article is JSON-escaped inside the kept result, so look for a stretch
	// of its first line without quote characters.
	probe := strings.Split(firstLine, "`")[0]
	if probe == "" {
		probe = firstLine
	}
	if n := strings.Count(text.String(), probe); n != 1 {
		t.Errorf("the Memory/set article appears %d times on step 4, want 1:\n%s", n, text.String())
	}
}
