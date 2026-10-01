package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestContextMemo_KeepsEachCallOnceAndItsLatestResult(t *testing.T) {
	m := &contextMemo{}
	m.add(json.RawMessage(`{"op":"help", "topic":"Memory/set"}`), "old article")
	m.add(json.RawMessage(`{"op":"doc","name":"Memory"}`), "doc result")
	m.add(json.RawMessage(`{"op":"help","topic":"Memory/set"}`), "new article") // same call, other spacing
	if len(m.entries) != 2 {
		t.Fatalf("entries = %d, want 2 (the repeated call replaces its entry)", len(m.entries))
	}
	got := m.render("Context", "")
	if strings.Contains(got, "old article") || !strings.Contains(got, "new article") {
		t.Errorf("want only the latest result of a repeated call:\n%s", got)
	}
	if strings.Index(got, "doc result") > strings.Index(got, "new article") {
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

// A live op's result (the clock, Σ, the window's fill, the channel list) is
// stale by the next step, and the memo tells the model not to call it again,
// so the model would work from an old clock or a Σ that contradicts "Current
// state:". Only reference ops are kept.
func TestContextMemo_KeepsOnlyReferenceOps(t *testing.T) {
	m := &contextMemo{}
	m.add(json.RawMessage(`{"op":"help","topic":"Memory/set"}`), "HELP ARTICLE")
	m.add(json.RawMessage(`{"op":"guide"}`), "GUIDE TEXT")
	m.add(json.RawMessage(`{"op":"time"}`), `{"now_rfc3339":"2026-09-30T06:00:00Z"}`)
	m.add(json.RawMessage(`{"op":"state"}`), `{"state":{"step":1}}`)
	m.add(json.RawMessage(`{"op":"self"}`), `{"context":{"used_pct":12}}`)
	got := m.render("Context", "")
	for _, live := range []string{`{"op":"time"}`, `{"op":"state"}`, `{"op":"self"}`, "now_rfc3339", "used_pct"} {
		if strings.Contains(got, live) {
			t.Errorf("a live Context result is kept as if it were current (%s):\n%s", live, got)
		}
	}
	if !strings.Contains(got, "HELP ARTICLE") || !strings.Contains(got, "GUIDE TEXT") {
		t.Errorf("a reference result was dropped:\n%s", got)
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

// docMaker is a stand-in for a tool whose result the next step needs: it
// returns an id.
type docMaker struct{}

func (docMaker) Name() string                 { return "Document" }
func (docMaker) Description() string          { return "documents" }
func (docMaker) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (docMaker) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: `{"document_id":"DOC-ID-FROM-CREATE"}`}, nil
}

// Measured in the 1.100.0 eval: a cloud model created a document, read a help
// article, and at the next step saw only the article, so the id create_document
// had returned was gone and it created a second document. The result of the
// last action that was not a Context call must still be shown after a Context
// call replaces it as the observation.
func TestRun_Stateful_TheLastActionResultIsKeptBesideAContextResult(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Document","input":{"op":"create_document","title":"T"}}}`,
		`{"patch":{},"action":{"tool":"Context","input":{"op":"help","topic":"Document/create_chunk"}}}`,
		`{"patch":{},"action":{"tool":"Memory","input":{"op":"get","key":"k"}}}`,
		`{"patch":{},"done":true,"final":"ok"}`,
	}}
	order := []tools.Tool{docMaker{}, &countingNoop{}, helpDoc{}}
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
	if len(prov.requests) < 4 {
		t.Fatalf("%d model calls, want 4", len(prov.requests))
	}
	step := func(i int) string {
		msgs := prov.requests[i]
		var b strings.Builder
		for _, c := range msgs[len(msgs)-1].Content {
			b.WriteString(c.Text)
		}
		return b.String()
	}
	s3 := step(2)
	if !strings.Contains(s3, "MEMORY SET ARTICLE") {
		t.Errorf("step 3 does not show the article it just read:\n%s", s3)
	}
	if !strings.Contains(s3, `Document {"op":"create_document","title":"T"}, returned:`) || !strings.Contains(s3, "DOC-ID-FROM-CREATE") {
		t.Errorf("step 3 lost the create_document result the Context call replaced:\n%s", s3)
	}
	// Once another action runs, it is the last action: the older one is gone,
	// and the new one is the observation, so it is not shown twice.
	s4 := step(3)
	if strings.Contains(s4, "DOC-ID-FROM-CREATE") {
		t.Errorf("step 4 still shows an action that is no longer the last:\n%s", s4)
	}
	if strings.Contains(s4, "Your last action") {
		t.Errorf("step 4 repeats the latest observation as the last action:\n%s", s4)
	}
}

// A kept action that failed is not done. After the usual fail → read help →
// retry sequence, the retry step must not be told the failed call is done.
func TestContextMemo_RendersAFailedLastActionAsFailed(t *testing.T) {
	m := &contextMemo{}
	m.addWork("Document", json.RawMessage(`{"op":"create_chunk","document_id":"d"}`), `{"isError":true,"error":"missing body"}`, true)
	got := m.render("Context", "the help article")
	if !strings.Contains(got, "FAILED") || !strings.Contains(got, "missing body") {
		t.Errorf("a failed last action is not shown as failed:\n%s", got)
	}
	if strings.Contains(got, "do not repeat it") || strings.Contains(got, "it is done") {
		t.Errorf("a failed last action is shown as done:\n%s", got)
	}
}

func TestContextMemo_RendersASuccessfulLastActionAsDone(t *testing.T) {
	m := &contextMemo{}
	m.addWork("Document", json.RawMessage(`{"op":"create_document"}`), `{"document_id":"D1"}`, false)
	got := m.render("Context", "the help article")
	if !strings.Contains(got, `Your last action (it is done; do not repeat it), Document {"op":"create_document"}, returned:`) {
		t.Errorf("a successful last action lost its wording:\n%s", got)
	}
	if strings.Contains(got, "FAILED") {
		t.Errorf("a successful last action is shown as failed:\n%s", got)
	}
}

// failingDoc is a Document stand-in whose every call fails.
type failingDoc struct{}

func (failingDoc) Name() string                 { return "Document" }
func (failingDoc) Description() string          { return "documents" }
func (failingDoc) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (failingDoc) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "create_chunk: missing required field: body", IsError: true}, nil
}

// The failure flag must cross from the dispatched result into the memo: a
// failed action, then a help read, and the next step shows the failure as
// failed rather than done.
func TestRun_Stateful_AFailedLastActionIsNotShownAsDone(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{},"action":{"tool":"Document","input":{"op":"create_chunk","document_id":"d"}}}`,
		`{"patch":{},"action":{"tool":"Context","input":{"op":"help","topic":"Document/create_chunk"}}}`,
		`{"patch":{},"done":true,"final":"ok"}`,
	}}
	order := []tools.Tool{failingDoc{}, helpDoc{}}
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
	msgs := prov.requests[2]
	var b strings.Builder
	for _, c := range msgs[len(msgs)-1].Content {
		b.WriteString(c.Text)
	}
	s := b.String()
	if !strings.Contains(s, "missing required field: body") {
		t.Fatalf("step 3 does not show the failed action at all:\n%s", s)
	}
	if !strings.Contains(s, "FAILED") || strings.Contains(s, "do not repeat it") {
		t.Errorf("step 3 shows the failed action as done:\n%s", s)
	}
}

// hintedFailure is a failed call's result as a stateful run renders it: the
// in-band JSON whose hint carries the whole help article.
func hintedFailure(article string) string {
	return renderToolResultText(tools.Result{
		Text:    "set: missing required field: value",
		IsError: true,
		Error: &tools.ErrorInfo{
			CallFormat: &tools.CallFormat{Tool: "Memory", Op: "set"},
			Hint:       "You have not read the help for Memory/set in this run. Its article follows — read it before your next call to Memory.\n\n" + article,
		},
	})
}

// Once the model reads the help a kept failure's hint pointed at, the kept
// failure must not show the article a second time, nor still say the help is
// unread. The rest of the error stays.
func TestContextMemo_KeptFailureDropsHintOnceHelpIsRead(t *testing.T) {
	const article = "ARTICLE-MARKER how to call Memory set"
	m := &contextMemo{}
	m.addWork("Memory", json.RawMessage(`{"op":"set","key":"k"}`), hintedFailure(article), true)
	helpObs := `{"name":"Memory/set","content":"` + article + `"}`
	m.add(json.RawMessage(`{"op":"help","topic":"Memory/set"}`), helpObs)

	got := m.render("Context", helpObs)
	if strings.Contains(got, "You have not read the help") {
		t.Errorf("the kept failure still says the help is unread:\n%s", got)
	}
	if strings.Contains(got, "ARTICLE-MARKER") {
		t.Errorf("the article appears beside the observation that already is it:\n%s", got)
	}
	if !strings.Contains(got, "FAILED") || !strings.Contains(got, "missing required field: value") || !strings.Contains(got, "correctCallFormat") {
		t.Errorf("the kept failure lost its error:\n%s", got)
	}
}

// Before the help is read, the hint is the one place the model sees the
// article, so the kept failure keeps it. Help for another tool whose name
// merely starts with this one's does not count as read.
func TestContextMemo_KeptFailureKeepsHintUntilHelpIsRead(t *testing.T) {
	const article = "ARTICLE-MARKER how to call Memory set"
	m := &contextMemo{}
	m.addWork("Memory", json.RawMessage(`{"op":"set","key":"k"}`), hintedFailure(article), true)
	m.add(json.RawMessage(`{"op":"help","topic":"MemoryBackendDef"}`), `{"name":"MemoryBackendDef","content":"other"}`)

	got := m.render("Context", "some other observation")
	if !strings.Contains(got, "You have not read the help for Memory/set") || !strings.Contains(got, "ARTICLE-MARKER") {
		t.Errorf("the hint is gone before the help was read:\n%s", got)
	}
}

// A kept action's result is capped, so one huge tool output cannot carry the
// stateful prompt past the memo's budget.
func TestContextMemo_CapsAnOversizedLastActionResult(t *testing.T) {
	m := &contextMemo{}
	huge := strings.Repeat("x", 3*contextMemoBudget)
	m.addWork("Read", json.RawMessage(`{"path":"big"}`), huge, false)

	got := m.render("Context", "")
	if len(got) > workTextCap+500 {
		t.Errorf("rendered %d bytes for a %d-byte result, want at most about %d", len(got), len(huge), workTextCap)
	}
	if !strings.Contains(got, "[truncated: ") {
		t.Errorf("a cut result does not say it was cut:\n%.300s", got)
	}
}

func TestCapText_CutsAtARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got := capText(s, 5)
	cut, _, _ := strings.Cut(got, "\n[truncated: ")
	if cut != "éé" || !utf8.ValidString(got) {
		t.Errorf("capText = %q, want two whole runes and a marker", got)
	}
	if capText("short", 5) != "short" {
		t.Error("a result within the cap was changed")
	}
}
