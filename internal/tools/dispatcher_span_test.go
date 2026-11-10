package tools_test

// These tests lived in internal/otel until it became its own Go module (RFC
// FE): that module cannot import this one, and the code under test is the
// dispatcher's span wire-up, so they belong here.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// withInMemoryExporter installs a synchronous in-memory tracer provider for
// the test and restores the previous one when it ends.
func withInMemoryExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	t.Cleanup(func() {
		cleanup()
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

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

// The dispatcher names the run's tenant and the model's tool_use id on the
// span: the tenant from the run identity (authoritative, and present on the
// MCP / HTTP / gRPC tool surfaces where no run span set one), the call id as
// gen_ai.tool.call.id.
func TestDispatcherCall_SpanCarriesRunTenantAndToolUseID(t *testing.T) {
	exp := withInMemoryExporter(t)
	d := tools.NewDispatcher([]tools.Tool{&fakeTool{name: "TestTool", res: tools.Result{Text: "ok"}}})
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "alice"})
	ctx = tools.WithToolUseID(ctx, "toolu_7")
	d.Execute(ctx, "TestTool", json.RawMessage(`{}`))
	want := map[string]string{lcotel.AttrTenant: "acme", "gen_ai.tool.call.id": "toolu_7", "gen_ai.tool.name": "TestTool"}
	for _, kv := range exp.GetSpans()[0].Attributes {
		if w, ok := want[string(kv.Key)]; ok && kv.Value.AsString() == w {
			delete(want, string(kv.Key))
		}
	}
	if len(want) > 0 {
		t.Errorf("tool span lacks %v: %+v", want, exp.GetSpans()[0].Attributes)
	}
}

// isErrorAttr reports whether a span carries loomcycle.tool_is_error=true.
func isErrorAttr(s tracetest.SpanStub) bool {
	for _, kv := range s.Attributes {
		if string(kv.Key) == lcotel.AttrToolIsError && kv.Value.AsBool() {
			return true
		}
	}
	return false
}

// An error result (a refusal, a denial, a tool answering "no") is the model's
// to read: an attribute, not the span's error status, so a collector keeping
// error traces does not keep every run that met a refusal.
func TestDispatcherExecute_IsErrorIsAnAttributeNotASpanError(t *testing.T) {
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
	if spans[0].Status.Code.String() == "Error" {
		t.Errorf("span status = Error %q for an error result", spans[0].Status.Description)
	}
	if !isErrorAttr(spans[0]) {
		t.Errorf("%s missing: %+v", lcotel.AttrToolIsError, spans[0].Attributes)
	}
}

func TestDispatcherExecute_UnknownToolIsAnAttributeNotASpanError(t *testing.T) {
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
	if spans[0].Status.Code.String() == "Error" || !isErrorAttr(spans[0]) {
		t.Errorf("unknown-tool span: status %v, %s %v; want an attribute, not an error",
			spans[0].Status.Code, lcotel.AttrToolIsError, isErrorAttr(spans[0]))
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
