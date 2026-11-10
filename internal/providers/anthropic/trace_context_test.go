package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// TestCall_SendsNoTraceContextToTheProvider: a model provider never receives
// the run's trace id, even inside a sampled span with the propagator set —
// it goes to operator MCP servers only.
func TestCall_SendsNoTraceContextToTheProvider(t *testing.T) {
	defer lcotel.SetInboundForTest(lcotel.InboundParent, propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))()
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()

	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headers <- r.Header.Clone():
		default:
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	ctx, span := tp.Tracer("t").Start(context.Background(), "loomcycle.iteration")
	defer span.End()
	ch, err := New("test-key", srv.URL, streamhttp.Options{}, nil).Call(ctx, providers.Request{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	h := <-headers
	for _, k := range []string{"traceparent", "tracestate", "baggage"} {
		if v := h.Get(k); v != "" {
			t.Errorf("provider request carried %s: %q", k, v)
		}
	}
}
