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
	if !res.IsError || res.Text != `Document: unknown argument "text" — nothing was done.` {
		t.Errorf("error text = %q", res.Text)
	}
	info := mustValidation(t, res)
	if info.CallFormat == nil || !strings.Contains(string(info.CallFormat.Example), `"body":"GA in April."`) {
		t.Errorf("no correct example in the call format: %+v", info.CallFormat)
	}
}

// mustValidation asserts res is a non-retryable validation refusal and returns
// its info.
func mustValidation(t *testing.T, res Result) *ErrorInfo {
	t.Helper()
	if res.Error == nil || res.Error.Category != CategoryValidation || res.Error.Retryable {
		t.Fatalf("want a non-retryable validation refusal, got %+v", res.Error)
	}
	return res.Error
}

// The two shapes seen live get a specific hint, in the next step the refusal
// carries: arguments wrapped in an envelope, and a respelling of a real
// argument.
func TestExecute_UnknownArgumentHints(t *testing.T) {
	d, _ := docDispatcher(t)
	for in, want := range map[string]string{
		`{"op":"create_chunk","input":{"title":"T"}}`: `Do not wrap the arguments in "input"`,
		`{"op":"create_chunk","chunkId":"c1"}`:        `Did you mean "chunk_id" instead of "chunkId"?`,
	} {
		res := d.Execute(context.Background(), "Document", json.RawMessage(in))
		if desc := mustValidation(t, res).Description; !strings.Contains(desc, want) {
			t.Errorf("%s: the next step lacks %q:\n%s", in, want, desc)
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
		`argument "parent" is not one create_chunk takes — pass it as "parent_id" ("parent" belongs to propose_entity)`,
		"Nothing was done.",
	} {
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("error text lacks %q:\n%s", want, res.Text)
		}
	}
	info := mustValidation(t, res)
	for _, want := range []string{`Did you mean "parent_id" instead of "parent"?`, "create_chunk takes: document_id, title, parent_id."} {
		if !strings.Contains(info.Description, want) {
			t.Errorf("the next step lacks %q:\n%s", want, info.Description)
		}
	}
	if info.CallFormat == nil || !strings.Contains(string(info.CallFormat.Example), `"title":"Flights"`) {
		t.Errorf("no correct example in the call format: %+v", info.CallFormat)
	}

	// An op documented as taking nothing refuses a sibling's argument too.
	res = d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"stats","title":"x"}`))
	if desc := mustValidation(t, res).Description; !strings.Contains(desc, "stats takes no arguments besides op") {
		t.Errorf("stats with a sibling's argument: %q", desc)
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

const kbSchema = `{"type":"object","properties":{"op":{"type":"string","enum":["get_chunk","get_edges","query_documents","search","graph_recall","maintain_statement"]},"id":{"type":"string"},"document_id":{"type":"string"},"chunk_id":{"type":"string"},"seed_ids":{"type":"array"},"query":{"type":"string"},"type":{"type":"string"},"kind":{"type":"string"}}}`

var kbArgs = map[string][]string{
	"Document/get_chunk":          {"id"},
	"Document/get_edges":          {"document_id", "kind"},
	"Document/query_documents":    {"type"},
	"Document/search":             {"query"},
	"Document/graph_recall":       {"seed_ids", "query"},
	"Document/maintain_statement": {"chunk_id", "id"},
}

// The wrong id argument is the most repeated mistake agents made reading a
// knowledge base: chunk_id for get_chunk's id, seed_ids for backlinks' id, id for
// get_edges' document_id. The refusal used to lead with "it belongs to
// maintain_statement", and agents chased that op; it now names the argument
// this op takes. A text query on a listing op points at op=search.
func TestExecute_SiblingArgumentRefusalNamesTheIDThisOpTakes(t *testing.T) {
	doc := &countingStub{pointerStub: pointerStub{name: "Document", schema: kbSchema}}
	topics := []string{"Document"}
	for k := range kbArgs {
		topics = append(topics, k)
	}
	d := NewDispatcher([]Tool{doc, &argsHelp{newHelp(topics...), kbArgs}})

	for _, tc := range []struct{ in, want string }{
		{`{"op":"get_chunk","chunk_id":"c1"}`, `pass it as "id"`},
		{`{"op":"get_chunk","seed_ids":["c1"]}`, `pass it as "id"`},
		{`{"op":"get_edges","id":"d1"}`, `pass it as "document_id"`},
	} {
		res := d.Execute(context.Background(), "Document", json.RawMessage(tc.in))
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s: error lacks %q:\n%s", tc.in, tc.want, res.Text)
		}
	}
	res := d.Execute(context.Background(), "Document", json.RawMessage(`{"op":"query_documents","query":"budget"}`))
	if desc := mustValidation(t, res).Description; !strings.Contains(desc, "use op=search") {
		t.Errorf("query on query_documents does not point at op=search: %q", desc)
	}
	if doc.ran != 0 {
		t.Errorf("the tool ran %d times on refused calls", doc.ran)
	}
}
