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
	topics   map[string]bool
	examples map[string]string
}

func (s *stubHelp) HasHelpTopic(name string) bool { return s.topics[name] }

func (s *stubHelp) HelpExample(topic string) (string, bool) {
	ex, ok := s.examples[topic]
	return ex, ok
}

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
	return &stubHelp{pointerStub{name: "Context", desc: "Introspection.", schema: `{"type":"object"}`}, m, map[string]string{}}
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

// A tool with an article tells the model to read it BEFORE its first call, with
// a real operation topic to fetch — the first operation in the tool's own schema
// order that has an article — rather than a placeholder to fill in.
func TestSpecsFor_InstructsReadingTheOperationArticleBeforeTheFirstCall(t *testing.T) {
	path := &pointerStub{name: "Path", desc: "Names things.", schema: `{"type":"object","properties":{"op":{"type":"string","enum":["resolve","ls"]}}}`}
	ctxTool := newHelp("Path", "Path/ls")
	order := []Tool{path, ctxTool}
	got := specDesc(t, NewDispatcher(order), order, "Path")
	want := "Names things.\n\n" + `Before your first call to Path, read the call format for the operation you need: call Context with {"op":"help","topic":"Path/ls"} (put your operation in place of ls). It gives the exact arguments and an example.`
	if got != want {
		t.Errorf("description =\n%s\nwant\n%s", got, want)
	}
}

// A tool with no operations is sent to its tool article.
func TestSpecsFor_ToolWithoutOperationsIsSentToItsArticle(t *testing.T) {
	read := &pointerStub{name: "Read", desc: "Reads a file.", schema: `{"type":"object","properties":{"path":{"type":"string"}}}`}
	ctxTool := newHelp("Read")
	order := []Tool{read, ctxTool}
	got := specDesc(t, NewDispatcher(order), order, "Read")
	if !strings.HasSuffix(got, `Before your first call to Read, read its call format: call Context with {"op":"help","topic":"Read"}. It gives the exact arguments and an example.`) {
		t.Errorf("description = %q", got)
	}
}

// "Before calling Context, call Context" is a loop, not guidance.
func TestSpecsFor_TheHelpToolGetsNoInstructionToCallItself(t *testing.T) {
	ctxTool := newHelp("Context")
	order := []Tool{ctxTool}
	if got := specDesc(t, NewDispatcher(order), order, "Context"); got != "Introspection." {
		t.Errorf("Context description = %q, want it untouched", got)
	}
}

// The instruction is also returned on its own, so a surface that shortens the
// description (the stateful tool list) can keep it.
func TestSpecsFor_ExposesTheInstructionSeparately(t *testing.T) {
	read := &pointerStub{name: "Read", desc: "Reads a file.", schema: `{"type":"object"}`}
	ctxTool := newHelp("Read")
	order := []Tool{read, ctxTool}
	for _, s := range NewDispatcher(order).SpecsFor(context.Background(), order) {
		if s.Name != "Read" {
			continue
		}
		if s.Help == "" || !strings.HasSuffix(s.Description, s.Help) {
			t.Errorf("Help = %q; want the instruction the description ends with (%q)", s.Help, s.Description)
		}
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

// A failed call to a documented tool carries a CORRECT call for the operation
// it tried, taken from that operation's article, and the help call for the
// rest. Models recover from what an error says; they did not follow a pointer.
func TestExecute_FailedCallCarriesACorrectCallForTheOperation(t *testing.T) {
	path := &failingStub{pointerStub{name: "Path", schema: `{"type":"object","properties":{"op":{"type":"string","enum":["ls","mv"]},"path":{"type":"string"},"to":{"type":"string"}}}`},
		Result{Text: "move_chunk: missing required field: to", IsError: true}}
	help := newHelp("Path", "Path/mv")
	help.examples["Path/mv"] = `{"op":"mv","path":"/docs/draft","to":"/docs/final"}`
	d := NewDispatcher([]Tool{path, help})

	got := d.Execute(context.Background(), "Path", json.RawMessage(`{"op":"mv","path":"/docs/a"}`)).Text
	want := "move_chunk: missing required field: to\n\n" +
		"A correct Path/mv call looks like this (an example — use your own values):\n" +
		`{"op":"mv","path":"/docs/draft","to":"/docs/final"}` + "\n\n" +
		`Full reference: call Context with {"op":"help","topic":"Path/mv"}.`
	if got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
}

// When the op itself is what is wrong, the error lists the ops the tool has.
func TestExecute_UnknownOperationListsTheValidOnes(t *testing.T) {
	path := &failingStub{pointerStub{name: "Path", schema: `{"type":"object","properties":{"op":{"type":"string","enum":["ls","mv"]}}}`},
		Result{Text: `unknown op "list"`, IsError: true}}
	d := NewDispatcher([]Tool{path, newHelp("Path", "Path/mv")})
	for _, in := range []string{`{"op":"list"}`, `{"path":"/"}`} {
		got := d.Execute(context.Background(), "Path", json.RawMessage(in)).Text
		if !strings.Contains(got, "Valid operations for Path: ls, mv.") ||
			!strings.HasSuffix(got, `{"op":"help","topic":"Path"}.`) {
			t.Errorf("%s: text = %q, want the valid ops and the tool article", in, got)
		}
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
