package otel_test

import (
	"context"
	"errors"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// withInMemoryExporter installs an in-memory exporter as the global
// tracer for the duration of t. Returns the exporter so the test can
// assert what spans landed. Mirrors the canonical harness from
// TestSetTracerProviderForTest_CapturesSpansToInMemoryExporter in
// tracer_test.go.
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

func TestRecordRunStart_AttributeSet(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{
		RunID:     "run_abc",
		AgentID:   "ag_xyz",
		AgentName: "researcher",
		UserID:    "user_42",
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Name != lcotel.SpanRun {
		t.Errorf("span name = %q, want %q", got.Name, lcotel.SpanRun)
	}
	want := map[string]string{
		lcotel.AttrRunID:     "run_abc",
		lcotel.AttrAgentID:   "ag_xyz",
		lcotel.AttrAgentName: "researcher",
		lcotel.AttrUserID:    "user_42",
	}
	for _, kv := range got.Attributes {
		k := string(kv.Key)
		if w, ok := want[k]; ok {
			if kv.Value.AsString() != w {
				t.Errorf("attr %q = %q, want %q", k, kv.Value.AsString(), w)
			}
			delete(want, k)
		}
	}
	for k := range want {
		t.Errorf("attr %q missing from span", k)
	}
}

func TestRecordRunStart_OmitsEmptyAttrs(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{
		RunID:     "run_abc",
		AgentName: "researcher",
		// AgentID, UserID, ParentAgentID intentionally empty
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	for _, kv := range spans[0].Attributes {
		switch string(kv.Key) {
		case lcotel.AttrAgentID, lcotel.AttrUserID, lcotel.AttrParentAgentID:
			t.Errorf("attr %q should be omitted when empty, got value %q",
				kv.Key, kv.Value.AsString())
		}
	}
}

func TestRecordRunStart_SubRunHasParentAgentID(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{
		RunID:         "sub_run",
		AgentID:       "child",
		ParentAgentID: "parent",
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	found := false
	for _, kv := range spans[0].Attributes {
		if string(kv.Key) == lcotel.AttrParentAgentID && kv.Value.AsString() == "parent" {
			found = true
		}
	}
	if !found {
		t.Errorf("ParentAgentID attribute missing from sub-run span")
	}
}

// TestRecordIteration_NestsUnderRun pins the inheritance contract: the
// iteration span MUST be a child of the run span via ctx propagation.
// Operators rely on this to see the run → iteration tree in Jaeger.
func TestRecordIteration_NestsUnderRun(t *testing.T) {
	exp := withInMemoryExporter(t)
	runCtx, runSpan := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1"})
	_, iterSpan := lcotel.RecordIteration(runCtx, 0)
	iterSpan.End()
	runSpan.End()

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	// Spans are emitted in End() order. iter ended first, then run.
	iter := spans[0]
	run := spans[1]
	if iter.Name != lcotel.SpanIteration {
		t.Fatalf("first span = %q, want %q", iter.Name, lcotel.SpanIteration)
	}
	if run.Name != lcotel.SpanRun {
		t.Fatalf("second span = %q, want %q", run.Name, lcotel.SpanRun)
	}
	if iter.Parent.SpanID() != run.SpanContext.SpanID() {
		t.Errorf("iteration span parent = %s, want run span ID = %s",
			iter.Parent.SpanID(), run.SpanContext.SpanID())
	}
}

func TestRecordProviderCall_StampsProviderAndModel(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordProviderCall(context.Background(), lcotel.ProviderCallAttrs{
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
		Tier:     "smart",
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != lcotel.SpanProviderCall {
		t.Fatalf("unexpected spans: %+v", spans)
	}
	got := map[string]string{}
	for _, kv := range spans[0].Attributes {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got[lcotel.AttrProvider] != "anthropic" {
		t.Errorf("provider attr = %q", got[lcotel.AttrProvider])
	}
	if got[lcotel.AttrModel] != "claude-sonnet-4-6" {
		t.Errorf("model attr = %q", got[lcotel.AttrModel])
	}
	if got[lcotel.AttrTier] != "smart" {
		t.Errorf("tier attr = %q", got[lcotel.AttrTier])
	}
}

func TestRecordToolCall_StampsToolName(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordToolCall(context.Background(), "Bash")
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != lcotel.SpanToolCall {
		t.Fatalf("unexpected spans: %+v", spans)
	}
	for _, kv := range spans[0].Attributes {
		if string(kv.Key) == lcotel.AttrTool && kv.Value.AsString() == "Bash" {
			return
		}
	}
	t.Errorf("tool attribute missing or wrong: %+v", spans[0].Attributes)
}

func TestRecordMCPCall_NestsInsideToolCall(t *testing.T) {
	exp := withInMemoryExporter(t)
	toolCtx, toolSpan := lcotel.RecordToolCall(context.Background(), "mcp__jobs__getAgentContext")
	_, mcpSpan := lcotel.RecordMCPCall(toolCtx, "jobs", "getAgentContext")
	mcpSpan.End()
	toolSpan.End()

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	mcp := spans[0]
	tool := spans[1]
	if mcp.Name != lcotel.SpanMCPCall {
		t.Fatalf("first = %q, want %q", mcp.Name, lcotel.SpanMCPCall)
	}
	if mcp.Parent.SpanID() != tool.SpanContext.SpanID() {
		t.Error("mcp.call span is not a child of tool.call")
	}
}

func TestSetRunDone_RecordsUsageAndStopReason(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1"})
	lcotel.SetRunDone(span, lcotel.RunDoneAttrs{
		InputTokens:     1234,
		OutputTokens:    567,
		CacheReadTokens: 100,
		StopReason:      "end_turn",
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	want := map[string]attribute.Value{
		lcotel.AttrInputTokens:     attribute.IntValue(1234),
		lcotel.AttrOutputTokens:    attribute.IntValue(567),
		lcotel.AttrCacheReadTokens: attribute.IntValue(100),
		lcotel.AttrStopReason:      attribute.StringValue("end_turn"),
	}
	for _, kv := range spans[0].Attributes {
		k := string(kv.Key)
		if w, ok := want[k]; ok {
			if kv.Value != w {
				t.Errorf("attr %q = %v, want %v", k, kv.Value, w)
			}
			delete(want, k)
		}
	}
	for k := range want {
		t.Errorf("attr %q missing from done-state", k)
	}
}

func TestSetRunDone_ErrorMarksSpan(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1"})
	lcotel.SetRunDone(span, lcotel.RunDoneAttrs{
		StopReason: "failed",
		Err:        errors.New("provider 503"),
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Status.Code != codes.Error {
		t.Errorf("span status code = %v, want %v", got.Status.Code, codes.Error)
	}
	if got.Status.Description == "" {
		t.Errorf("span status description empty; want error message")
	}
	// Error event should also be recorded.
	if len(got.Events) == 0 {
		t.Errorf("no error event recorded on the span")
	}
}

func TestSetRunDone_NilSpanIsSafe(t *testing.T) {
	// finishRun* paths call SetRunDone(meta.otelSpan, ...) — meta from
	// tests / early-failure paths may have a nil span. Must not panic.
	lcotel.SetRunDone(nil, lcotel.RunDoneAttrs{StopReason: "x"})
}

func TestParseMCPToolName_RoundTrip(t *testing.T) {
	cases := []struct {
		in           string
		wantServer   string
		wantToolName string
	}{
		{"mcp__jobs__getAgentContext", "jobs", "getAgentContext"},
		{"mcp__brave-search__brave_web_search", "brave-search", "brave_web_search"},
		{"Read", "", ""}, // not an MCP tool
		{"mcp__missing-sep", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		gs, gt := lcotel.ParseMCPToolName(c.in)
		if gs != c.wantServer || gt != c.wantToolName {
			t.Errorf("ParseMCPToolName(%q) = (%q, %q), want (%q, %q)",
				c.in, gs, gt, c.wantServer, c.wantToolName)
		}
	}
}
