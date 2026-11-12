package otel

import (
	"context"
	"net/http"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// inboundMode is LOOMCYCLE_OTEL_INBOUND_CONTEXT, set once by Init. It lives
// beside the global propagator it qualifies, which the OTEL API makes
// process-wide; the four extraction sites (HTTP, gRPC, webhooks, A2A) read it
// without each needing a handle threaded through. Before Init it is parent,
// the documented default, and extraction is inert anyway: the global
// propagator is a no-op until Init sets one.
var inboundMode atomic.Value // InboundMode

func setInboundMode(m InboundMode) {
	if m == "" {
		m = InboundParent
	}
	inboundMode.Store(m)
}

func currentInboundMode() InboundMode {
	if m, ok := inboundMode.Load().(InboundMode); ok {
		return m
	}
	return InboundParent
}

// SetInboundForTest installs mode and propagator for the duration of a test
// and returns the restore function.
func SetInboundForTest(mode InboundMode, p propagation.TextMapPropagator) func() {
	prevMode, prevProp := currentInboundMode(), otel.GetTextMapPropagator()
	setInboundMode(mode)
	otel.SetTextMapPropagator(p)
	return func() {
		setInboundMode(prevMode)
		otel.SetTextMapPropagator(prevProp)
	}
}

type inboundKey struct{}

// inbound is a caller's span context, with the mode in force when it was read.
type inbound struct {
	sc   trace.SpanContext
	mode InboundMode
}

// ExtractInbound reads a caller's trace context from carrier into ctx. Call
// it only once the request is authenticated or its signature verified: the
// context decides whether a run is sampled and which trace it joins, and an
// anonymous caller must not choose either.
//
// The span context is kept aside rather than made ctx's current span, so the
// spans a handler opens before the run (a webhook's receive span, say) keep
// their own parents; RecordRunStart applies it. Baggage is carried in ctx and
// never copied onto a span.
func ExtractInbound(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	mode := currentInboundMode()
	if mode == InboundIgnore {
		return ctx
	}
	ext := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	if b := baggage.FromContext(ext); b.Len() > 0 {
		ctx = baggage.ContextWithBaggage(ctx, b)
	}
	sc := trace.SpanContextFromContext(ext)
	if !sc.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, inboundKey{}, inbound{sc: sc, mode: mode})
}

// CarryInbound returns dst carrying the caller's trace context src holds, for
// a run started on a context detached from its request.
func CarryInbound(dst, src context.Context) context.Context {
	if in, ok := src.Value(inboundKey{}).(inbound); ok {
		dst = context.WithValue(dst, inboundKey{}, in)
	}
	if b := baggage.FromContext(src); b.Len() > 0 {
		dst = baggage.ContextWithBaggage(dst, b)
	}
	return dst
}

// runStartContext applies the caller's trace context to a run span about to
// start on ctx. A run that already has a parent span in ctx (a sub-run) keeps
// it: the caller's context describes the request, not each run under it.
func runStartContext(ctx context.Context) (context.Context, []trace.SpanStartOption) {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, nil
	}
	in, ok := ctx.Value(inboundKey{}).(inbound)
	if !ok {
		return ctx, nil
	}
	switch in.mode {
	case InboundParent:
		return trace.ContextWithRemoteSpanContext(ctx, in.sc), nil
	case InboundLink:
		return ctx, []trace.SpanStartOption{trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: in.sc})}
	}
	return ctx, nil
}

// InjectTraceContext writes ctx's span as traceparent (and tracestate) into
// h, through the configured propagator so OTEL_PROPAGATORS=none sends
// nothing. Baggage is never forwarded: it came from a caller and is not
// loomcycle's to pass on. For operator-configured MCP servers only; model
// providers and model-chosen hosts never receive it.
//
// Only a span this process records is passed on: with tracing off the no-op
// span still carries a caller's context, and forwarding it would hand the
// caller's trace id on although loomcycle took no part in the trace.
func InjectTraceContext(ctx context.Context, h http.Header) {
	span := trace.SpanFromContext(ctx)
	// Spans recorded only for tenants' own collectors are no trace of the
	// operator's for a server it calls to join.
	// A trace sampled only for a tenant's export is likewise none of the
	// operator's.
	if routingOnly.Load() || !span.IsRecording() || !span.SpanContext().IsValid() || tenantOnly(span.SpanContext()) {
		return
	}
	mc := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, mc)
	for _, k := range []string{"traceparent", "tracestate"} {
		if v := mc.Get(k); v != "" {
			h.Set(k, v)
		}
	}
}

// SampledTraceID is the trace id of ctx's span when this process records and
// samples it, else "": an unsampled trace leaves nothing in the backend to
// link to. Recording is checked too because with tracing off the no-op span
// still carries a caller's span context, whose trace this process never
// exported.
func SampledTraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	// Recorded only for tenants' own collectors: the operator's backend holds
	// nothing to link a run to.
	if routingOnly.Load() || !span.IsRecording() || !sc.IsValid() || !sc.IsSampled() || tenantOnly(sc) {
		return ""
	}
	return sc.TraceID().String()
}
