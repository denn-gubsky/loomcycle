package a2a

import (
	"context"
	"net/http"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// TestPrincipalInterceptor_ReadsTraceContextOnlyForAnAuthenticatedPeer: the
// ctx an authenticated peer's call carries to the executor joins the peer's
// trace; an anonymous (open-mode) call's does not.
func TestPrincipalInterceptor_ReadsTraceContextOnlyForAnAuthenticatedPeer(t *testing.T) {
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer lcotel.SetTracerProviderForTest(tp)()
	defer lcotel.SetInboundForTest(lcotel.InboundParent, propagation.TraceContext{})()

	runSpanUnder := func(ic principalInterceptor) tracetest.SpanStub {
		t.Helper()
		exp.Reset()
		h := http.Header{}
		h.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
		_, cc := a2asrv.NewCallContext(context.Background(), a2asrv.NewServiceParams(h))
		ctx, _, err := ic.Before(context.Background(), cc, &a2asrv.Request{})
		if err != nil {
			t.Fatal(err)
		}
		_, span := lcotel.RecordRunStart(ctx, lcotel.RunStartAttrs{})
		span.End()
		return exp.GetSpans()[0]
	}

	authed := principalInterceptor{auth: func(context.Context, http.Header) (Peer, bool) { return Peer{Name: "peer"}, true }}
	if s := runSpanUnder(authed); s.SpanContext.TraceID().String() != traceID || s.Parent.SpanID().String() != spanID {
		t.Errorf("authenticated peer: trace=%s parent=%s, want %s/%s", s.SpanContext.TraceID(), s.Parent.SpanID(), traceID, spanID)
	}
	if s := runSpanUnder(principalInterceptor{auth: nil}); s.Parent.IsValid() {
		t.Error("open mode: the run joined the anonymous caller's trace")
	}
}
