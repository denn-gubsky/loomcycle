package otel

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
	traceparent = "00-" + callerTrace + "-" + callerSpan + "-01"
)

func inMemoryTracing(t *testing.T, mode InboundMode) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restoreTP := SetTracerProviderForTest(tp)
	restoreIn := SetInboundForTest(mode, newPropagator([]string{"tracecontext", "baggage"}))
	t.Cleanup(func() {
		restoreIn()
		restoreTP()
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

func callerHeaders() http.Header {
	h := http.Header{}
	h.Set("traceparent", traceparent)
	h.Set("baggage", "user=alice")
	return h
}

func runSpan(t *testing.T, exp *tracetest.InMemoryExporter, ctx context.Context) tracetest.SpanStub {
	t.Helper()
	_, span := RecordRunStart(ctx, RunStartAttrs{RunID: "r1"})
	span.End()
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	return spans[0]
}

// TestRecordRunStart_ParentModeJoinsTheCallersTrace: the run span is a child
// of the caller's span, and the caller's baggage never becomes an attribute.
func TestRecordRunStart_ParentModeJoinsTheCallersTrace(t *testing.T) {
	exp := inMemoryTracing(t, InboundParent)
	ctx := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	s := runSpan(t, exp, ctx)
	if s.SpanContext.TraceID().String() != callerTrace {
		t.Errorf("trace = %s, want the caller's %s", s.SpanContext.TraceID(), callerTrace)
	}
	if s.Parent.SpanID().String() != callerSpan || !s.Parent.IsRemote() {
		t.Errorf("parent = %s (remote %v), want the caller's span %s", s.Parent.SpanID(), s.Parent.IsRemote(), callerSpan)
	}
	for _, kv := range s.Attributes {
		if kv.Value.Emit() == "alice" {
			t.Errorf("baggage copied onto the span as %s", kv.Key)
		}
	}
}

// TestRecordRunStart_LinkModeStartsARootThatLinks: a new trace whose root
// links to the caller's span.
func TestRecordRunStart_LinkModeStartsARootThatLinks(t *testing.T) {
	exp := inMemoryTracing(t, InboundLink)
	ctx := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	s := runSpan(t, exp, ctx)
	if s.Parent.IsValid() {
		t.Errorf("link mode run has parent %s, want a root", s.Parent.SpanID())
	}
	if s.SpanContext.TraceID().String() == callerTrace {
		t.Error("link mode joined the caller's trace")
	}
	if len(s.Links) != 1 || s.Links[0].SpanContext.SpanID().String() != callerSpan {
		t.Errorf("links = %+v, want one to the caller's span", s.Links)
	}
}

// TestRecordRunStart_IgnoreModeReadsNothing: a fresh root, no link.
func TestRecordRunStart_IgnoreModeReadsNothing(t *testing.T) {
	exp := inMemoryTracing(t, InboundIgnore)
	ctx := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	s := runSpan(t, exp, ctx)
	if s.Parent.IsValid() || len(s.Links) != 0 || s.SpanContext.TraceID().String() == callerTrace {
		t.Errorf("ignore mode used the caller's context: parent=%v links=%d trace=%s", s.Parent.IsValid(), len(s.Links), s.SpanContext.TraceID())
	}
}

// TestRecordRunStart_SubRunKeepsItsLocalParent: under a span already in ctx,
// the caller's context does not re-parent the run.
func TestRecordRunStart_SubRunKeepsItsLocalParent(t *testing.T) {
	exp := inMemoryTracing(t, InboundLink)
	ctx := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	ctx, parent := Tracer().Start(ctx, "loomcycle.iteration")
	_, sub := RecordRunStart(ctx, RunStartAttrs{RunID: "child"})
	sub.End()
	parent.End()
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d", len(spans))
	}
	if spans[0].Parent.SpanID() != parent.SpanContext().SpanID() || len(spans[0].Links) != 0 {
		t.Errorf("sub-run parent = %s links = %d, want the iteration span and no link", spans[0].Parent.SpanID(), len(spans[0].Links))
	}
}

// TestCarryInbound_DetachedContextKeepsTheCallersTrace: a run started on a
// context built from Background still joins the caller's trace.
func TestCarryInbound_DetachedContextKeepsTheCallersTrace(t *testing.T) {
	exp := inMemoryTracing(t, InboundParent)
	req := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	s := runSpan(t, exp, CarryInbound(context.Background(), req))
	if s.SpanContext.TraceID().String() != callerTrace {
		t.Errorf("trace = %s, want the caller's", s.SpanContext.TraceID())
	}
}

// TestInjectTraceContext_SendsTraceparentNotBaggage.
func TestInjectTraceContext_SendsTraceparentNotBaggage(t *testing.T) {
	inMemoryTracing(t, InboundParent)
	ctx := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	ctx, span := RecordRunStart(ctx, RunStartAttrs{})
	defer span.End()
	h := http.Header{}
	InjectTraceContext(ctx, h)
	tp := h.Get("traceparent")
	if want := "00-" + callerTrace + "-" + span.SpanContext().SpanID().String() + "-01"; tp != want {
		t.Errorf("traceparent = %q, want %q", tp, want)
	}
	if h.Get("baggage") != "" {
		t.Errorf("baggage forwarded: %q", h.Get("baggage"))
	}

	none := http.Header{}
	restore := SetInboundForTest(InboundParent, newPropagator(nil)) // OTEL_PROPAGATORS=none
	InjectTraceContext(ctx, none)
	restore()
	if len(none) != 0 {
		t.Errorf("propagators=none still injected %v", none)
	}
}

// TestSampledTraceID_EmptyUnlessRecordedAndSampled.
func TestSampledTraceID_EmptyUnlessRecordedAndSampled(t *testing.T) {
	// Tracing off: the no-op span carries the caller's context, which this
	// process never exported.
	restore := SetTracerProviderForTest(noop.NewTracerProvider())
	defer restore()
	restoreIn := SetInboundForTest(InboundParent, newPropagator([]string{"tracecontext"}))
	defer restoreIn()
	ctx := ExtractInbound(context.Background(), propagation.HeaderCarrier(callerHeaders()))
	ctx, span := RecordRunStart(ctx, RunStartAttrs{})
	if got := SampledTraceID(ctx); got != "" {
		t.Errorf("tracing off: SampledTraceID = %q, want empty", got)
	}
	span.End()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	ctx, span = tp.Tracer("t").Start(context.Background(), "s")
	if got := SampledTraceID(ctx); got != "" {
		t.Errorf("unsampled: SampledTraceID = %q, want empty", got)
	}
	span.End()

	tp = sdktrace.NewTracerProvider()
	ctx, span = tp.Tracer("t").Start(context.Background(), "s")
	if got := SampledTraceID(ctx); got != span.SpanContext().TraceID().String() {
		t.Errorf("sampled: SampledTraceID = %q, want %s", got, span.SpanContext().TraceID())
	}
	span.End()
}
