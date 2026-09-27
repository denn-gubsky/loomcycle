package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// countingStub records whether the tool actually ran.
type countingStub struct {
	pointerStub
	ran int
}

func (c *countingStub) Execute(context.Context, json.RawMessage) (Result, error) {
	c.ran++
	return Result{Text: "ok"}, nil
}

const docSchema = `{"type":"object","properties":{"op":{"type":"string","enum":["create_chunk"]},"document_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"chunk_id":{"type":"string"}}}`

func docDispatcher(t *testing.T) (*Dispatcher, *countingStub) {
	t.Helper()
	doc := &countingStub{pointerStub: pointerStub{name: "Document", schema: docSchema}}
	help := newHelp("Document", "Document/create_chunk")
	help.examples["Document/create_chunk"] = `{"op":"create_chunk","document_id":"d1","title":"Timeline","body":"GA in April."}`
	return NewDispatcher([]Tool{doc, help}), doc
}

// An argument the schema does not declare is refused before the tool runs —
// it used to be dropped silently, so a create_chunk with `text` for `body`
// saved an empty body and reported success — and the refusal carries the
// operation's correct example.
func TestExecute_UnknownArgumentIsRefusedWithTheExample(t *testing.T) {
	d, doc := docDispatcher(t)
	res := d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"T","text":"hello"}`))
	if doc.ran != 0 {
		t.Fatal("the tool ran with an unknown argument")
	}
	for _, want := range []string{`unknown argument "text"`, "nothing was done", `"body":"GA in April."`} {
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("result lacks %q:\n%s", want, res.Text)
		}
	}
}

// The two shapes seen live get a specific hint: arguments wrapped in an
// envelope, and a respelling of a real argument.
func TestExecute_UnknownArgumentHints(t *testing.T) {
	d, _ := docDispatcher(t)
	for in, want := range map[string]string{
		`{"op":"create_chunk","input":{"title":"T"}}`: `Do not wrap the arguments in "input"`,
		`{"op":"create_chunk","chunkId":"c1"}`:        `Did you mean "chunk_id" instead of "chunkId"?`,
	} {
		res := d.Execute(context.Background(), "Document", json.RawMessage(in))
		if !strings.Contains(res.Text, want) {
			t.Errorf("%s: result lacks %q:\n%s", in, want, res.Text)
		}
	}
}

// Only documented tools with a closed schema are held to it: an undocumented
// tool (an MCP server's, say), a schema that allows extra properties, and a run
// without a help tool all pass unknown arguments through as before.
func TestExecute_UnknownArgumentsPassWhereNotCovered(t *testing.T) {
	cases := map[string]func() (*Dispatcher, *countingStub){
		"undocumented tool": func() (*Dispatcher, *countingStub) {
			x := &countingStub{pointerStub: pointerStub{name: "Other", schema: docSchema}}
			return NewDispatcher([]Tool{x, newHelp("Document")}), x
		},
		"open schema": func() (*Dispatcher, *countingStub) {
			x := &countingStub{pointerStub: pointerStub{name: "Document", schema: `{"type":"object","additionalProperties":true,"properties":{"op":{"type":"string"}}}`}}
			return NewDispatcher([]Tool{x, newHelp("Document")}), x
		},
		"no help tool": func() (*Dispatcher, *countingStub) {
			x := &countingStub{pointerStub: pointerStub{name: "Document", schema: docSchema}}
			return NewDispatcher([]Tool{x}), x
		},
	}
	for name, mk := range cases {
		d, x := mk()
		res := d.Execute(context.Background(), x.name, json.RawMessage(`{"op":"create_chunk","text":"hi"}`))
		if res.IsError || x.ran != 1 {
			t.Errorf("%s: result %q, ran %d; want the call passed through", name, res.Text, x.ran)
		}
	}
}

// argsHelp is a help index that also knows each operation's documented
// arguments (OpArgumentIndex). A topic absent from args has no Arguments section.
type argsHelp struct {
	*stubHelp
	args map[string][]string
}

func (a *argsHelp) HelpArguments(topic string) ([]string, bool) {
	v, ok := a.args[topic]
	return v, ok
}

const entitySchema = `{"type":"object","properties":{"op":{"type":"string","enum":["create_chunk","propose_entity","stats"]},"document_id":{"type":"string"},"title":{"type":"string"},"parent_id":{"type":"string"},"parent":{"type":"string"},"note":{"type":"string"}}}`

func entityDispatcher(t *testing.T, args map[string][]string) (*Dispatcher, *countingStub) {
	t.Helper()
	doc := &countingStub{pointerStub: pointerStub{name: "Document", schema: entitySchema}}
	help := &argsHelp{newHelp("Document", "Document/create_chunk", "Document/propose_entity", "Document/stats"), args}
	help.examples["Document/create_chunk"] = `{"op":"create_chunk","document_id":"d1","title":"Flights"}`
	return NewDispatcher([]Tool{doc, help}), doc
}

var entityArgs = map[string][]string{
	"Document/create_chunk":   {"document_id", "title", "parent_id"},
	"Document/propose_entity": {"title", "parent"},
	"Document/stats":          {},
}

// An argument the schema declares but THIS op does not take — it belongs to a
// sibling op — is refused, where it used to be dropped silently: `parent` on
// create_chunk put the chunk under the wrong parent. The refusal names whose it
// is, suggests the op's own argument, lists what the op takes, and carries the
// correct example.
func TestExecute_ASiblingOperationsArgumentIsRefused(t *testing.T) {
	d, doc := entityDispatcher(t, entityArgs)
	res := d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"Flights","parent":"c9"}`))
	if doc.ran != 0 {
		t.Fatal("the tool ran with a sibling operation's argument")
	}
	for _, want := range []string{
		`argument "parent" is not one create_chunk takes (it belongs to propose_entity)`,
		`Did you mean "parent_id"?`,
		"Nothing was done.",
		"create_chunk takes: document_id, title, parent_id.",
		`"title":"Flights"`, // the example
	} {
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("result lacks %q:\n%s", want, res.Text)
		}
	}

	// An op documented as taking nothing refuses a sibling's argument too.
	res = d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"stats","title":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "stats takes no arguments besides op") {
		t.Errorf("stats with a sibling's argument: %q", res.Text)
	}
}

// Nothing is refused where the documentation does not say whose an argument is:
// an argument no article lists, an op whose article has no Arguments section,
// and a help index that does not know arguments at all.
func TestExecute_SiblingArgumentsPassWhereUndocumented(t *testing.T) {
	d, doc := entityDispatcher(t, entityArgs)
	if res := d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"T","note":"n"}`)); res.IsError || doc.ran != 1 {
		t.Errorf("an argument no article lists was refused: %q", res.Text)
	}

	noSection := map[string][]string{"Document/propose_entity": {"title", "parent"}}
	d, doc = entityDispatcher(t, noSection)
	if res := d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"T","parent":"c9"}`)); res.IsError || doc.ran != 1 {
		t.Errorf("refused for an op with no Arguments section: %q", res.Text)
	}

	plain := &countingStub{pointerStub: pointerStub{name: "Document", schema: entitySchema}}
	d = NewDispatcher([]Tool{plain, newHelp("Document", "Document/create_chunk")})
	if res := d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"create_chunk","parent":"c9"}`)); res.IsError || plain.ran != 1 {
		t.Errorf("refused without an argument index: %q", res.Text)
	}
}
