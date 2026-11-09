package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// TestClient_TraceparentOnlyWhenPropagating: a request to an operator's
// server carries the calling span as traceparent; one to a server without
// the flag (a tenant registered it) carries none.
func TestClient_TraceparentOnlyWhenPropagating(t *testing.T) {
	defer lcotel.SetInboundForTest(lcotel.InboundParent, propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))()
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("traceparent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("t").Start(context.Background(), "loomcycle.mcp.call")
	defer span.End()
	want := "00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"

	for _, propagate := range []bool{true, false} {
		c, err := New(Config{URL: srv.URL, PropagateTraceContext: propagate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Call(ctx, "tools/list", nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("requests = %d", len(got))
	}
	if got[0] != want {
		t.Errorf("operator server traceparent = %q, want %q", got[0], want)
	}
	if got[1] != "" {
		t.Errorf("tenant server got traceparent %q", got[1])
	}
}
