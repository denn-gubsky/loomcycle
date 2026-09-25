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
