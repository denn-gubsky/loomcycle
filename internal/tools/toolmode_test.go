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
