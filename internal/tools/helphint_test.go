package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// articleHelp is a Context stand-in that also serves article text.
type articleHelp struct {
	*stubHelp
	articles map[string]string
}

func (a *articleHelp) HelpContent(topic string) (string, bool) {
	c, ok := a.articles[topic]
	return c, ok
}

func (a *articleHelp) HelpTopicTool(topic string) string {
	tool, _, _ := strings.Cut(topic, "/")
	if _, ok := a.articles[tool]; ok {
		return tool
	}
	return ""
}

// failingTool fails every call with the given category.
type failingTool struct {
	pointerStub
	cat ErrorCategory
}

func (f *failingTool) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{Text: "bad call", IsError: true, Error: &ErrorInfo{Category: f.cat}}, nil
}

// key is declared so a call reaches the tool: an undeclared argument would be
// refused by the dispatcher before the tool ran, whatever the tool returns.
const memorySchema = `{"type":"object","properties":{"op":{"type":"string","enum":["get","set"]},"key":{"type":"string"}}}`

// hintFixture is a run dispatcher with Memory (documented: a tool article and
// set/get articles) and a Context that serves them.
func hintFixture(cat ErrorCategory) *Dispatcher {
	mem := &failingTool{pointerStub{name: "Memory", desc: "Stores values.", schema: memorySchema}, cat}
	help := &articleHelp{newHelp("Memory", "Memory/set", "Memory/get"), map[string]string{
		"Memory":     "MEMORY TOOL ARTICLE",
		"Memory/set": "MEMORY SET ARTICLE",
		"Memory/get": "MEMORY GET ARTICLE",
	}}
	d := NewDispatcher([]Tool{mem, help})
	d.EnableHelpHints()
	return d
}

func hintOf(r Result) string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Hint
}

func call(d *Dispatcher, name, input string) Result {
	return d.Execute(context.Background(), name, json.RawMessage(input))
}

// A run that skipped Memory's help gets the failing operation's article with
// its first shape failure, and not again for the same operation: in the append
// loop every result stays in the context, so a second copy only grows it.
func TestHelpHint_OnceForEachOperationWhileHelpIsUnread(t *testing.T) {
	d := hintFixture(CategoryValidation)

	first := hintOf(call(d, "Memory", `{"op":"set","key":"a"}`))
	if !strings.Contains(first, "MEMORY SET ARTICLE") || strings.Contains(first, "MEMORY TOOL ARTICLE") {
		t.Fatalf("first set failure: want the set article only, got %q", first)
	}
	if h := hintOf(call(d, "Memory", `{"op":"set","key":"b"}`)); h != "" {
		t.Errorf("second set failure: the article was already given, got %q", h)
	}
	if h := hintOf(call(d, "Memory", `{"op":"get","key":"a"}`)); !strings.Contains(h, "MEMORY GET ARTICLE") {
		t.Errorf("first get failure: want the get article, got %q", h)
	}
}

// When the operation itself is wrong there is no operation article to give, so
// the hint is the tool's article, which lists the operations.
func TestHelpHint_AWrongOperationGetsTheToolArticle(t *testing.T) {
	d := hintFixture(CategoryValidation)
	if h := hintOf(call(d, "Memory", `{"op":"fetch"}`)); !strings.Contains(h, "MEMORY TOOL ARTICLE") {
		t.Errorf("unknown op: want the tool article, got %q", h)
	}
}

// A model that read the tool's help has the article already. Reading ANY of the
// tool's articles counts: the gate is per tool.
func TestHelpHint_NoneOnceTheRunHasReadTheToolsHelp(t *testing.T) {
	d := hintFixture(CategoryValidation)
	if r := call(d, "Context", `{"op":"help","topic":"Memory/get"}`); r.IsError {
		t.Fatalf("help call failed: %s", r.Text)
	}
	if h := hintOf(call(d, "Memory", `{"op":"set","key":"a"}`)); h != "" {
		t.Errorf("the run read Memory's help, but the failure still carries a hint: %q", h)
	}
}

// The stateful loop shows its model only the latest observation, so there the
// hint repeats on every shape failure until the model reads the help.
func TestHelpHint_UntilReadRepeatsUntilTheHelpIsRead(t *testing.T) {
	d := hintFixture(CategoryValidation)
	d.SetHelpHintsUntilRead(true)
	for i := 0; i < 2; i++ {
		if h := hintOf(call(d, "Memory", `{"op":"set","key":"k`+string(rune('a'+i))+`"}`)); !strings.Contains(h, "MEMORY SET ARTICLE") {
			t.Fatalf("failure %d: want the set article every time, got %q", i+1, h)
		}
	}
	call(d, "Context", `{"op":"help","topic":"Memory"}`)
	if h := hintOf(call(d, "Memory", `{"op":"set","key":"z"}`)); h != "" {
		t.Errorf("after reading the help: want no hint, got %q", h)
	}
}

// Only where the call's shape may be the cause: a business, permission or
// transient failure gets no article, as it gets no call format.
func TestHelpHint_NoneOnAFailureTheShapeDidNotCause(t *testing.T) {
	for _, cat := range []ErrorCategory{CategoryBusiness, CategoryPermission} {
		d := hintFixture(cat)
		if h := hintOf(call(d, "Memory", `{"op":"set","key":"a"}`)); h != "" {
			t.Errorf("%s failure carries a hint: %q", cat, h)
		}
	}
}

// The commonest measured shape failure is refused by the dispatcher before the
// tool runs: an argument the tool does not take (small's `body` on
// create_document, medium's `parent` on create_chunk). It carries the hint too.
func TestHelpHint_AnUnknownArgumentRefusalCarriesTheArticle(t *testing.T) {
	d := hintFixture(CategoryValidation)
	r := call(d, "Memory", `{"op":"set","parent":"x"}`)
	if !r.IsError || !strings.Contains(r.Text, "unknown argument") {
		t.Fatalf("want the unknown-argument refusal, got %q", r.Text)
	}
	if h := hintOf(r); !strings.Contains(h, "MEMORY SET ARTICLE") {
		t.Errorf("the refusal carries no set article: %q", h)
	}
}

// A dispatcher built for one call outside a run has no history to gate on, so
// it attaches no hints unless the run enables them.
func TestHelpHint_NoneUnlessEnabled(t *testing.T) {
	mem := &failingTool{pointerStub{name: "Memory", desc: "Stores values.", schema: memorySchema}, CategoryValidation}
	help := &articleHelp{newHelp("Memory", "Memory/set"), map[string]string{"Memory": "T", "Memory/set": "S"}}
	d := NewDispatcher([]Tool{mem, help})
	if h := hintOf(call(d, "Memory", `{"op":"set","key":"a"}`)); h != "" {
		t.Errorf("a dispatcher without EnableHelpHints attached a hint: %q", h)
	}
}
