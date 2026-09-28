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

// The same help topic asked for more than once is kept ONCE, however the call
// was written: Context resolves "Memory.set", an alias or a different key order
// to the same article, and names it in its response.
func TestContextMemo_KeepsEachHelpTopicOnce(t *testing.T) {
	article := func(name, body string) string {
		b, _ := json.Marshal(map[string]string{"name": name, "content": body})
		return string(b)
	}
	m := &contextMemo{}
	m.add(json.RawMessage(`{"op":"help","topic":"Memory/set"}`), article("Memory/set", "first read"))
	m.add(json.RawMessage(`{"op":"help","topic":"Memory.set","query":""}`), article("Memory/set", "second read"))
	m.add(json.RawMessage(`{"topic":"Memory/set","op":"help"}`), article("Memory/set", "third read"))
	m.add(json.RawMessage(`{"op":"help","topic":"Memory/get"}`), article("Memory/get", "get article"))
	m.add(json.RawMessage(`{"op":"help","query":"how do I save"}`), `{"results":[]}`)
	if len(m.entries) != 3 {
		t.Fatalf("entries = %d, want 3 (Memory/set once, Memory/get, the search)", len(m.entries))
	}
	got := m.render("Context", "")
	if n := strings.Count(got, " returned:\n"); n != 3 {
		t.Errorf("rendered %d results, want 3:\n%s", n, got)
	}
	if strings.Contains(got, "first read") || strings.Contains(got, "second read") || !strings.Contains(got, "third read") {
		t.Errorf("want Memory/set once, as its latest read:\n%s", got)
	}
}

// Any other op is the same call when its arguments are, whatever their order
// or empty extras.
func TestMemoKey_OtherOpsIgnoreKeyOrderAndEmptyArguments(t *testing.T) {
	a := memoKey(json.RawMessage(`{"op":"doc","name":"Memory"}`), `{}`)
	b := memoKey(json.RawMessage(`{"name":"Memory","op":"doc","prefix":""}`), `{}`)
	c := memoKey(json.RawMessage(`{"op":"doc","name":"Document"}`), `{}`)
	if a != b {
		t.Errorf("same call, different key order: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different calls share a key: %q", a)
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
