package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// shapeFailingMemory fails every call as a validation error, as a malformed
// call does.
type shapeFailingMemory struct{}

func (shapeFailingMemory) Name() string        { return "Memory" }
func (shapeFailingMemory) Description() string { return "stores things" }
func (shapeFailingMemory) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","enum":["set"]},"key":{"type":"string"}}}`)
}
func (shapeFailingMemory) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "set: missing required field: value", IsError: true,
		Error: &tools.ErrorInfo{Category: tools.CategoryValidation}}, nil
}

// articleContext is a Context stand-in that serves Memory's articles.
type articleContext struct{}

func (articleContext) Name() string                 { return "Context" }
func (articleContext) Description() string          { return "introspection" }
func (articleContext) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (articleContext) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "{}"}, nil
}
func (articleContext) HasHelpTopic(n string) bool        { return n == "Memory" || n == "Memory/set" }
func (articleContext) HelpExample(string) (string, bool) { return "", false }
func (articleContext) HelpTopicTool(string) string       { return "Memory" }
func (articleContext) HelpContent(t string) (string, bool) {
	if t == "Memory/set" {
		return "MEMORY SET ARTICLE", true
	}
	return "", false
}

// The stateful model reads only its latest observation, so a hint shown once is
// gone a step later. Two shape failures of the same operation in a row: BOTH
// observations must carry the article (in the append loop the second would
// not, since the first is still in the context there).
func TestRun_Stateful_EveryShapeFailureCarriesTheHintUntilHelpIsRead(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Memory","input":{"op":"set","key":"k1"}}}`,
		`{"patch":{},"action":{"tool":"Memory","input":{"op":"set","key":"k2"}}}`,
		`{"patch":{},"done":true,"final":"gave up"}`,
	}}
	order := []tools.Tool{shapeFailingMemory{}, articleContext{}}
	d := tools.NewDispatcher(order)
	d.EnableHelpHints()
	if _, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      order,
		Dispatcher: d,
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) < 3 {
		t.Fatalf("%d model calls, want 3", len(prov.requests))
	}
	for step := 1; step <= 2; step++ {
		msgs := prov.requests[step]
		last := msgs[len(msgs)-1]
		var text strings.Builder
		for _, c := range last.Content {
			text.WriteString(c.Text)
		}
		if !strings.Contains(text.String(), "MEMORY SET ARTICLE") {
			t.Errorf("step %d's observation lacks the article:\n%s", step+1, text.String())
		}
	}
}
