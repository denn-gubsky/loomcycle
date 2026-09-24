package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type pointerStub struct {
	name, desc, schema string
}

func (s *pointerStub) Name() string                 { return s.name }
func (s *pointerStub) Description() string          { return s.desc }
func (s *pointerStub) InputSchema() json.RawMessage { return json.RawMessage(s.schema) }
func (s *pointerStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{}, nil
}

// stubHelp is a Context stand-in: a tool that is also a HelpIndex.
type stubHelp struct {
	pointerStub
	topics map[string]bool
}

func (s *stubHelp) HasHelpTopic(name string) bool { return s.topics[name] }

type stubScoped struct {
	pointerStub
	fields []ScopeField
}

func (s *stubScoped) ScopeGrants(context.Context) []ScopeField { return s.fields }

func newHelp(topics ...string) *stubHelp {
	m := map[string]bool{}
	for _, t := range topics {
		m[t] = true
	}
	return &stubHelp{pointerStub{name: "Context", desc: "Introspection.", schema: `{"type":"object"}`}, m}
}

func specDesc(t *testing.T, d *Dispatcher, order []Tool, name string) string {
	t.Helper()
	for _, s := range d.SpecsFor(context.Background(), order) {
		if s.Name == name {
			return s.Description
		}
	}
	t.Fatalf("no spec for %s", name)
	return ""
}

// A tool with an article points at it, with a concrete operation to read next —
// the first operation in the tool's own schema order that has an article.
func TestSpecsFor_PointsAtTheToolsArticleAndAnOperation(t *testing.T) {
	path := &pointerStub{name: "Path", desc: "Names things.", schema: `{"type":"object","properties":{"op":{"type":"string","enum":["resolve","ls"]}}}`}
	ctxTool := newHelp("Path", "Path/ls")
	order := []Tool{path, ctxTool}
	got := specDesc(t, NewDispatcher(order), order, "Path")
	want := "Names things.\n\n" + `Usage and examples: call Context with {"op":"help","topic":"Path"}; for one operation: {"op":"help","topic":"Path/ls"}.`
	if got != want {
		t.Errorf("description =\n%s\nwant\n%s", got, want)
	}
}

// A tool with no operations gets only the article pointer.
func TestSpecsFor_ToolWithoutOperationsGetsOnlyTheArticle(t *testing.T) {
	read := &pointerStub{name: "Read", desc: "Reads a file.", schema: `{"type":"object","properties":{"path":{"type":"string"}}}`}
	ctxTool := newHelp("Read")
	order := []Tool{read, ctxTool}
	got := specDesc(t, NewDispatcher(order), order, "Read")
	if !strings.HasSuffix(got, `Usage and examples: call Context with {"op":"help","topic":"Read"}.`) {
		t.Errorf("description = %q", got)
	}
}

// The pointer names a call the run can make, so a run without Context — or one
// whose dispatcher would not dispatch it — gets none. A tool with no article
// gets none either.
func TestSpecsFor_NoPointerWithoutContextOrArticle(t *testing.T) {
	path := &pointerStub{name: "Path", desc: "Names things.", schema: `{"type":"object"}`}
	other := &pointerStub{name: "Other", desc: "Other.", schema: `{"type":"object"}`}
	ctxTool := newHelp("Path")

	if got := specDesc(t, NewDispatcher([]Tool{path}), []Tool{path}, "Path"); got != "Names things." {
		t.Errorf("no Context in the run: description = %q, want it untouched", got)
	}
	// Context listed in the order but not dispatchable: still no pointer.
	if got := specDesc(t, NewDispatcher([]Tool{path}), []Tool{path, ctxTool}, "Path"); got != "Names things." {
		t.Errorf("Context not dispatchable: description = %q, want it untouched", got)
	}
	order := []Tool{other, ctxTool}
	if got := specDesc(t, NewDispatcher(order), order, "Other"); got != "Other." {
		t.Errorf("no article: description = %q, want it untouched", got)
	}
}

// A scoped tool says which scopes this run may use, per field, and points at the
// scopes primer.
func TestSpecsFor_ScopedToolListsThisRunsGrants(t *testing.T) {
	mem := &stubScoped{
		pointerStub: pointerStub{name: "Memory", desc: "Stores things.", schema: `{"type":"object"}`},
		fields: []ScopeField{
			{Grants: []ScopeGrant{{Scope: "agent"}, {Scope: "user", Granted: true}, {Scope: "tenant", Granted: true}}},
			{Applies: "sql_* ops", Grants: []ScopeGrant{{Scope: "agent"}, {Scope: "user"}}},
		},
	}
	ctxTool := newHelp("scopes")
	order := []Tool{mem, ctxTool}
	got := specDesc(t, NewDispatcher(order), order, "Memory")
	want := "Stores things.\n\n" + `Scopes this run may use: user, tenant (not agent); for sql_* ops: none. What scopes mean: {"op":"help","topic":"scopes"}.`
	if got != want {
		t.Errorf("description =\n%s\nwant\n%s", got, want)
	}
}

// The tool array is part of the cached prompt prefix: two builds for the same
// run must be byte-identical.
func TestSpecsFor_IsDeterministic(t *testing.T) {
	path := &pointerStub{name: "Path", desc: "Names things.", schema: `{"type":"object","properties":{"op":{"type":"string","enum":["resolve","ls"]}}}`}
	ctxTool := newHelp("Path", "Path/ls", "Path/resolve")
	order := []Tool{path, ctxTool}
	d := NewDispatcher(order)
	a, _ := json.Marshal(d.SpecsFor(context.Background(), order))
	for i := 0; i < 20; i++ {
		b, _ := json.Marshal(d.SpecsFor(context.Background(), order))
		if string(a) != string(b) {
			t.Fatal("SpecsFor output differs between builds")
		}
	}
}

// failingStub fails every call with the configured result.
type failingStub struct {
	pointerStub
	res Result
}

func (f *failingStub) Execute(context.Context, json.RawMessage) (Result, error) { return f.res, nil }

// A failed call to a documented tool points at the article for the operation
// it tried; an operation without an article falls back to the tool's.
func TestExecute_FailedCallPointsAtTheOperationsArticle(t *testing.T) {
	path := &failingStub{pointerStub{name: "Path", schema: `{"type":"object"}`}, Result{Text: "destination already exists: /docs/b", IsError: true}}
	d := NewDispatcher([]Tool{path, newHelp("Path", "Path/mv")})

	got := d.Execute(context.Background(), "Path", json.RawMessage(`{"op":"mv","path":"/docs/a","to":"/docs/b"}`)).Text
	want := "destination already exists: /docs/b\n\n" + `How to call it: call Context with {"op":"help","topic":"Path/mv"}.`
	if got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	got = d.Execute(context.Background(), "Path", json.RawMessage(`{"op":"frobnicate"}`)).Text
	if !strings.HasSuffix(got, `{"op":"help","topic":"Path"}.`) {
		t.Errorf("unknown op: text = %q, want the tool article", got)
	}
}

// Successes, undocumented tools, dispatchers without Context and retryable
// failures are returned exactly as the tool produced them.
func TestExecute_NoPointerWhereItWouldNotHelp(t *testing.T) {
	fail := Result{Text: "boom", IsError: true}
	cases := []struct {
		name string
		res  Result
		help *pointerHelp
	}{
		{"success", Result{Text: "ok"}, newPointerHelp("Path")},
		{"undocumented tool", fail, newPointerHelp("Other")},
		{"no Context in the run", fail, nil},
		{"retryable failure", Result{Text: "upstream timed out", IsError: true, Error: &ErrorInfo{Category: "transient", Retryable: true}}, newPointerHelp("Path")},
	}
	for _, c := range cases {
		tl := &failingStub{pointerStub{name: "Path", schema: `{"type":"object"}`}, c.res}
		ts := []Tool{tl}
		if c.help != nil {
			ts = append(ts, c.help.stubHelp)
		}
		got := NewDispatcher(ts).Execute(context.Background(), "Path", json.RawMessage(`{"op":"ls"}`))
		if got.Text != c.res.Text {
			t.Errorf("%s: text = %q, want it untouched (%q)", c.name, got.Text, c.res.Text)
		}
	}
}

type pointerHelp struct{ stubHelp *stubHelp }

func newPointerHelp(topics ...string) *pointerHelp { return &pointerHelp{newHelp(topics...)} }
