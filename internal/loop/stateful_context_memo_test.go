package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestContextMemo_KeepsEachCallOnceAndItsLatestResult(t *testing.T) {
	m := &contextMemo{}
	m.add(json.RawMessage(`{"op":"help", "topic":"Memory/set"}`), "old article")
	m.add(json.RawMessage(`{"op":"self"}`), "self result")
	m.add(json.RawMessage(`{"op":"help","topic":"Memory/set"}`), "new article") // same call, other spacing
	if len(m.entries) != 2 {
		t.Fatalf("entries = %d, want 2 (the repeated call replaces its entry)", len(m.entries))
	}
	got := m.render("Context", "")
	if strings.Contains(got, "old article") || !strings.Contains(got, "new article") {
		t.Errorf("want only the latest result of a repeated call:\n%s", got)
	}
	if strings.Index(got, "self result") > strings.Index(got, "new article") {
		t.Errorf("the repeated call should move to the end:\n%s", got)
	}
}

func TestContextMemo_DropsTheOldestPastTheBudget(t *testing.T) {
	m := &contextMemo{}
	big := strings.Repeat("x", contextMemoBudget/2+1)
	m.add(json.RawMessage(`{"op":"help","topic":"A"}`), "A"+big)
	m.add(json.RawMessage(`{"op":"help","topic":"B"}`), "B"+big)
	if len(m.entries) != 1 || m.entries[0].call != `{"op":"help","topic":"B"}` {
		t.Errorf("want only the newest entry kept, got %d entries", len(m.entries))
	}
	// A single result larger than the budget is still kept: it is what the
	// model just asked for.
	m.add(json.RawMessage(`{"op":"help","topic":"C"}`), strings.Repeat("y", contextMemoBudget*2))
	if len(m.entries) != 1 || m.entries[0].call != `{"op":"help","topic":"C"}` {
		t.Errorf("an oversized newest result was not kept")
	}
}

func TestContextMemo_LeavesOutTheLatestObservation(t *testing.T) {
	m := &contextMemo{}
	m.add(json.RawMessage(`{"op":"help","topic":"A"}`), "article A")
	if got := m.render("Context", "article A"); got != "" {
		t.Errorf("the entry that is the latest observation is shown twice:\n%s", got)
	}
}

// helpDoc is a Context stand-in whose help call returns a recognisable article.
type helpDoc struct{}

func (helpDoc) Name() string                 { return "Context" }
func (helpDoc) Description() string          { return "introspection" }
func (helpDoc) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (helpDoc) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: `{"content":"MEMORY SET ARTICLE: pass value as a plain string"}`}, nil
}
func (helpDoc) HasHelpTopic(string) bool          { return true }
func (helpDoc) HelpExample(string) (string, bool) { return "", false }

// Measured: stateful models re-read the same help article again and again,
// because each step shows only the latest observation. The article read at
// step 1 must still be in the prompt at step 3, after another tool's result
// has replaced it as the observation.
func TestRun_Stateful_AContextResultIsKeptOnLaterSteps(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Context","input":{"op":"help","topic":"Memory/set"}}}`,
		`{"patch":{},"action":{"tool":"Memory","input":{"op":"get","key":"k"}}}`,
		`{"patch":{},"done":true,"final":"ok"}`,
	}}
	order := []tools.Tool{&countingNoop{}, helpDoc{}}
	if _, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      order,
		Dispatcher: tools.NewDispatcher(order),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) < 3 {
		t.Fatalf("%d model calls, want 3", len(prov.requests))
	}
	step3 := prov.requests[2]
	var text strings.Builder
	for _, c := range step3[len(step3)-1].Content {
		text.WriteString(c.Text)
	}
	s := text.String()
	if !strings.Contains(s, "MEMORY SET ARTICLE") {
		t.Errorf("step 3 no longer shows the article read at step 1:\n%s", s)
	}
	if !strings.Contains(s, `Context {"op":"help","topic":"Memory/set"} returned:`) {
		t.Errorf("the kept result does not name the call it came from:\n%s", s)
	}
}
