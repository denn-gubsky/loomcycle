package otel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestDispatcherExecute_EmitsToolCallSpan integration-tests the wire-up
// in internal/tools/tool.go:Dispatcher.Execute. A registered tool's
// Execute returns IsError=true — the span should be marked Error and
// the tool name attribute should be set.
func TestDispatcherExecute_EmitsToolCallSpan(t *testing.T) {
	exp := withInMemoryExporter(t)
	d := tools.NewDispatcher([]tools.Tool{&fakeTool{name: "TestTool", res: tools.Result{Text: "ok"}}})
	res := d.Execute(context.Background(), "TestTool", json.RawMessage(`{}`))
	if res.IsError {
		t.Fatalf("expected ok, got error: %s", res.Text)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != lcotel.SpanToolCall {
		t.Fatalf("got %d spans, want 1 tool.call: %+v", len(spans), spans)
	}
	for _, kv := range spans[0].Attributes {
		if string(kv.Key) == lcotel.AttrTool && kv.Value.AsString() == "TestTool" {
			return
		}
	}
	t.Errorf("loomcycle.tool attribute missing or wrong: %+v", spans[0].Attributes)
}

func TestDispatcherExecute_IsErrorMarksSpanError(t *testing.T) {
	exp := withInMemoryExporter(t)
	d := tools.NewDispatcher([]tools.Tool{&fakeTool{name: "BadTool", res: tools.Result{Text: "tool refused: scope=x", IsError: true}}})
	res := d.Execute(context.Background(), "BadTool", json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("expected IsError, got: %s", res.Text)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	// Span status should be Error per SetSpanErrorMessage contract.
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("span status = %v, want Error (IsError=true should mark span)",
			spans[0].Status.Code)
	}
}

func TestDispatcherExecute_UnknownToolMarksSpanError(t *testing.T) {
	exp := withInMemoryExporter(t)
	d := tools.NewDispatcher([]tools.Tool{})
	res := d.Execute(context.Background(), "NotARegistered", json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("expected IsError for unknown tool")
	}
	if !strings.Contains(res.Text, "tool not found") {
		t.Errorf("res.Text = %q, want contains 'tool not found'", res.Text)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("unknown-tool span status = %v, want Error", spans[0].Status.Code)
	}
}

func TestDispatcherExecute_GoErrorMarksSpanError(t *testing.T) {
	exp := withInMemoryExporter(t)
	d := tools.NewDispatcher([]tools.Tool{&fakeTool{name: "ErrTool", err: errors.New("provider 503")}})
	res := d.Execute(context.Background(), "ErrTool", json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("expected IsError, got %+v", res)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("Go-error span status = %v, want Error", spans[0].Status.Code)
	}
	// Error event should also be recorded (RecordError path).
	if len(spans[0].Events) == 0 {
		t.Errorf("no error event recorded on tool span")
	}
}

// fakeTool implements the tools.Tool interface for these dispatcher
// integration tests. Mirrors the scriptedProvider pattern in
// internal/api/http/agent_subagent_test.go — minimal, focused.
type fakeTool struct {
	name string
	res  tools.Result
	err  error
}

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Description() string          { return "fake" }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeTool) Execute(_ context.Context, _ json.RawMessage) (tools.Result, error) {
	return f.res, f.err
}
