package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// modalStub is a ModalTool whose op "get" reads and every other op writes. It
// counts its runs, so a test can tell a refusal before dispatch from a call
// the tool itself refused.
type modalStub struct {
	name string
	runs int
}

func (m *modalStub) Name() string                 { return m.name }
func (m *modalStub) Description() string          { return "" }
func (m *modalStub) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (m *modalStub) Execute(context.Context, json.RawMessage) (Result, error) {
	m.runs++
	return Result{Text: "ran"}, nil
}
func (m *modalStub) CallIsRead(in json.RawMessage) (string, bool) {
	var c struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(in, &c) != nil {
		return "", false
	}
	return c.Op, c.Op == "get"
}

func TestDispatcher_ReadModeRefusesAWriteBeforeTheToolRuns(t *testing.T) {
	doc := &modalStub{name: "Doc"}
	d := NewDispatcher([]Tool{doc})
	d.SetToolModes(map[string]string{"Doc": "read"})

	r := d.Execute(context.Background(), "Doc", json.RawMessage(`{"op":"put"}`))
	if !r.IsError || r.Error == nil || r.Error.Category != CategoryPermission {
		t.Fatalf("write in read mode = %+v, want a permission refusal", r)
	}
	for _, want := range []string{"op put", "read mode", "Nothing was changed"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("refusal %q does not say %q", r.Text, want)
		}
	}
	if doc.runs != 0 {
		t.Fatalf("the tool ran %d times; the write must be refused before dispatch", doc.runs)
	}
	if r := d.Execute(context.Background(), "Doc", json.RawMessage(`{"op":"get"}`)); r.IsError || doc.runs != 1 {
		t.Fatalf("read in read mode = %+v (runs %d), want it to run", r, doc.runs)
	}
}

// A mode on a tool that cannot classify its calls refuses them all: the mode
// was meant to narrow, and running the call whole would widen it back.
func TestDispatcher_ReadModeOnAnUnclassifiableToolRefusesEveryCall(t *testing.T) {
	plain := &stubTool{name: "Plain"}
	d := NewDispatcher([]Tool{plain})
	d.SetToolModes(map[string]string{"Plain": "read"})
	if r := d.Execute(context.Background(), "Plain", json.RawMessage(`{"op":"get"}`)); !r.IsError {
		t.Fatalf("call = %+v, want refused", r)
	}
}

func TestDispatcher_AnUnknownModeRefusesEvenAReadCall(t *testing.T) {
	doc := &modalStub{name: "Doc"}
	d := NewDispatcher([]Tool{doc})
	d.SetToolModes(map[string]string{"Doc": "readonly"})
	if r := d.Execute(context.Background(), "Doc", json.RawMessage(`{"op":"get"}`)); !r.IsError || doc.runs != 0 {
		t.Fatalf("call = %+v (runs %d), want refused before dispatch", r, doc.runs)
	}
}

func TestDispatcher_NoModesHoldsEveryToolWhole(t *testing.T) {
	doc := &modalStub{name: "Doc"}
	d := NewDispatcher([]Tool{doc})
	d.SetToolModes(nil)
	if r := d.Execute(context.Background(), "Doc", json.RawMessage(`{"op":"put"}`)); r.IsError || doc.runs != 1 {
		t.Fatalf("write without modes = %+v (runs %d), want it to run", r, doc.runs)
	}
}

// The dispatcher keeps its own copy: a caller changing its map afterwards does
// not change what the run holds.
func TestDispatcher_SetToolModesKeepsItsOwnCopy(t *testing.T) {
	doc := &modalStub{name: "Doc"}
	d := NewDispatcher([]Tool{doc})
	modes := map[string]string{"Doc": "read"}
	d.SetToolModes(modes)
	delete(modes, "Doc")
	if r := d.Execute(context.Background(), "Doc", json.RawMessage(`{"op":"put"}`)); !r.IsError {
		t.Fatalf("write = %+v, want still refused", r)
	}
}

// A spawned child holds every read mode of its parent on top of its own, so a
// read-mode run cannot write through a child whose definition holds the tool
// whole; a parent with no modes leaves the child's own.
func TestRunPolicy_InheritKeepsTheParentsReadModes(t *testing.T) {
	parent := RunPolicy{ToolModes: map[string]string{"Document": "read"}}
	got := parent.Inherit(ChildGrant{Own: RunPolicy{ToolModes: map[string]string{"Path": "read"}}}).ToolModes
	if got["Document"] != "read" || got["Path"] != "read" || len(got) != 2 {
		t.Fatalf("child modes = %v, want Document and Path read", got)
	}
	if got := parent.Inherit(ChildGrant{}).ToolModes; got["Document"] != "read" {
		t.Fatalf("a child with no modes of its own = %v, want the parent's Document read", got)
	}
	own := map[string]string{"Path": "read"}
	if got := (RunPolicy{}).Inherit(ChildGrant{Own: RunPolicy{ToolModes: own}}).ToolModes; len(got) != 1 || got["Path"] != "read" {
		t.Fatalf("child of a parent with no modes = %v, want its own", got)
	}
}
