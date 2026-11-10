package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

func spanAttr(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// A 429 is told apart by attribute, not by searching the error text: the
// refused attempt carries its status and error.type, and the retry its number.
func TestCall_RateLimitedAttemptIsFilterableByAttribute(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	ctx := lcotel.WithTenant(context.Background(), "acme")
	ch, err := New("test-key", srv.URL, streamhttp.Options{}, nil).Call(ctx, providers.Request{Model: "claude-x"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	var attempts []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == lcotel.SpanProviderCall {
			attempts = append(attempts, s)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("got %d provider attempt spans, want 2", len(attempts))
	}
	first, retry := attempts[0], attempts[1]
	for key, want := range map[string]string{
		"http.response.status_code": "429", "error.type": lcotel.ErrorTypeRateLimited,
		lcotel.AttrAttempt: "1", lcotel.AttrTenant: "acme",
	} {
		if v, _ := spanAttr(first, key); v.Emit() != want {
			t.Errorf("429 attempt: %s = %q, want %q", key, v.Emit(), want)
		}
	}
	if first.Status.Code.String() != "Error" {
		t.Errorf("429 attempt status = %v, want Error", first.Status.Code)
	}
	for key, want := range map[string]string{"http.response.status_code": "200", lcotel.AttrAttempt: "2"} {
		if v, _ := spanAttr(retry, key); v.Emit() != want {
			t.Errorf("retry: %s = %q, want %q", key, v.Emit(), want)
		}
	}
	if v, ok := spanAttr(retry, "error.type"); ok {
		t.Errorf("the successful retry carries error.type=%q", v.Emit())
	}
}
