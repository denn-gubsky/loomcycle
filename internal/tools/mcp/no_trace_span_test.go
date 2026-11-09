package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const spanCanary = "SPAN-CANARY-0c7e-no-trace"

// failingCaller answers tools/call with an error that quotes the call's
// arguments, as a server may.
type failingCaller struct{ fakeCaller }

func (c *failingCaller) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "tools/call" {
		return nil, errors.New("server says: bad input " + spanCanary)
	}
	return c.fakeCaller.Call(ctx, method, params)
}

// The MCP call span of a no-trace run carries no error text — a server's
// error can quote the arguments, which are the run's content. An ordinary
// run's span keeps the text, so the check can fail.
func TestMCPCallSpan_ANoTraceRunsErrorCarriesNoText(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	defer func() { cleanup(); _ = tp.Shutdown(context.Background()) }()

	build := func(_, name string) (Caller, error) {
		c := &failingCaller{fakeCaller: fakeCaller{server: name}}
		c.healthyFlag.Store(true)
		return c, nil
	}
	tool := NewTool(NewPool(build, nil, nil), "srv", ToolDescriptor{Name: "do"})
	for _, retention := range []string{"none", ""} {
		exp.Reset()
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{Retention: retention})
		_, _ = tool.Execute(ctx, json.RawMessage(`{"q":"`+spanCanary+`"}`))
		var got strings.Builder
		for _, s := range exp.GetSpans() {
			got.WriteString(s.Status.Description)
			for _, ev := range s.Events {
				for _, kv := range ev.Attributes {
					got.WriteString(" " + kv.Value.Emit())
				}
			}
		}
		has := strings.Contains(got.String(), spanCanary)
		if retention == "none" && has {
			t.Errorf("a no-trace run's MCP span carries %q", got.String())
		}
		if retention == "" && !has {
			t.Fatalf("an ordinary run's MCP span carries %q: this test cannot see the text", got.String())
		}
	}
}

// A server's isError answer is the model's to read, not a failed call: the
// MCP span records it as an attribute and keeps an unset status.
func TestMCPCallSpan_AnIsErrorAnswerIsAnAttributeNotAnError(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	defer func() { cleanup(); _ = tp.Shutdown(context.Background()) }()

	caller := &structuredCaller{structured: `{}`}
	pool := NewPool(func(_, _ string) (Caller, error) { return caller, nil }, nil, nil)
	if _, err := NewTool(pool, "peer", ToolDescriptor{Name: "probe"}).Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	var mcpSpan *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.Name == lcotel.SpanMCPCall {
			mcpSpan = &exp.GetSpans()[i]
		}
	}
	if mcpSpan == nil {
		t.Fatal("no MCP span")
	}
	if mcpSpan.Status.Code == codes.Error {
		t.Errorf("an isError answer failed the span: %q", mcpSpan.Status.Description)
	}
	flagged := false
	for _, kv := range mcpSpan.Attributes {
		flagged = flagged || (string(kv.Key) == lcotel.AttrToolIsError && kv.Value.AsBool())
	}
	if !flagged {
		t.Errorf("%s missing: %+v", lcotel.AttrToolIsError, mcpSpan.Attributes)
	}
}
