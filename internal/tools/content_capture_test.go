package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// echoTool answers with its input; marked, it flags its result as a no-trace
// run's content, as the Agent tool does for a no-trace child's answer.
type echoTool struct{ notRetained bool }

func (echoTool) Name() string                 { return "Echo" }
func (echoTool) Description() string          { return "echoes its input" }
func (echoTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e echoTool) Execute(ctx context.Context, in json.RawMessage) (Result, error) {
	if e.notRetained {
		MarkContentNotRetained(ctx)
	}
	return Result{Text: "you said " + string(in)}, nil
}

// capturedToolSpan runs one Echo call with content capture on and returns
// the tool span's arguments and result.
func capturedToolSpan(t *testing.T, tool echoTool, capture bool) (args, result string, ok bool) {
	t.Helper()
	// A tenant router must be installed for capture to apply; the spans are
	// read from a provider of the test's own.
	restoreRouter := lcotel.SetTenantRouterForTest(lcotel.NewTenantRouter(lcotel.TenantRouterConfig{}))
	defer restoreRouter()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := lcotel.SetTracerProviderForTest(tp)
	defer func() { restore(); _ = tp.Shutdown(context.Background()) }()

	ctx := WithRunIdentity(context.Background(), RunIdentityValue{TenantID: "acme"})
	if capture {
		mask := func(s string) string { return strings.ReplaceAll(s, "hunter22", "[redacted]") }
		ctx = lcotel.WithContentCapture(ctx, &lcotel.ContentCapture{MaxChars: 4096, Redact: mask,
			RedactArgs: func(_ string, b []byte) []byte { return []byte(mask(string(b))) }})
	}
	ctx, _ = WithContentMark(ctx)
	_, _ = NewDispatcher([]Tool{tool}).Call(ctx, "Echo", json.RawMessage(`{"password":"hunter22"}`))
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			switch string(kv.Key) {
			case "gen_ai.tool.call.arguments":
				args, ok = kv.Value.AsString(), true
			case "gen_ai.tool.call.result":
				result = kv.Value.AsString()
			}
		}
	}
	return args, result, ok
}

func TestToolSpan_CapturedContentIsRedactedAndAbsentWithoutCapture(t *testing.T) {
	if _, _, ok := capturedToolSpan(t, echoTool{}, false); ok {
		t.Fatal("a run without content capture recorded the tool's arguments")
	}
	args, result, ok := capturedToolSpan(t, echoTool{}, true)
	if !ok || args != `{"password":"[redacted]"}` || result != `you said {"password":"[redacted]"}` {
		t.Errorf("args = %q, result = %q (recorded %v), want both redacted", args, result, ok)
	}
}

func TestToolSpan_ANoTraceRunsContentIsAMarker(t *testing.T) {
	_, result, ok := capturedToolSpan(t, echoTool{notRetained: true}, true)
	if !ok || result != "[content not retained]" {
		t.Errorf("result = %q (recorded %v), want the not-retained marker", result, ok)
	}
}
