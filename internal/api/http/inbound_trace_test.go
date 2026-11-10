package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

const (
	inboundTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	inboundSpanID  = "00f067aa0ba902b7"
	inboundParent  = "00-" + inboundTraceID + "-" + inboundSpanID + "-01"
)

// inboundTracing installs an in-memory tracer and the inbound mode for one
// test.
func inboundTracing(t *testing.T, mode lcotel.InboundMode) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restoreTP := lcotel.SetTracerProviderForTest(tp)
	restoreIn := lcotel.SetInboundForTest(mode, propagation.TraceContext{})
	t.Cleanup(func() {
		restoreIn()
		restoreTP()
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

func runSpans(exp *tracetest.InMemoryExporter) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == lcotel.SpanRun {
			out = append(out, s)
		}
	}
	return out
}

// TestRunStart_AuthenticatedTraceparentParentsTheRunSpan: a run started over
// HTTP by an authenticated caller joins the caller's trace as a child of the
// caller's span.
func TestRunStart_AuthenticatedTraceparentParentsTheRunSpan(t *testing.T) {
	exp := inboundTracing(t, lcotel.InboundParent)
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "secret-bearer"
	srv, _ := makeServer(t, completingProvider(), cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/runs", strings.NewReader(runBody("default")))
	req.Header.Set("Authorization", "Bearer secret-bearer")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", inboundParent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	spans := runSpans(exp)
	if len(spans) != 1 {
		t.Fatalf("run spans = %d, want 1", len(spans))
	}
	if got := spans[0].SpanContext.TraceID().String(); got != inboundTraceID {
		t.Errorf("run trace = %s, want the caller's %s", got, inboundTraceID)
	}
	if got := spans[0].Parent.SpanID().String(); got != inboundSpanID {
		t.Errorf("run parent = %s, want the caller's span %s", got, inboundSpanID)
	}
}

// TestAuthMiddleware_ReadsTraceContextOnlyAfterAuthentication: the same
// traceparent is applied to a run under an authenticated request, refused with
// the request when the bearer is wrong, and ignored in open mode, which
// authenticates nobody. Each mode shapes the run span as documented.
func TestAuthMiddleware_ReadsTraceContextOnlyAfterAuthentication(t *testing.T) {
	type outcome struct {
		served          bool
		joined, linked  bool
		freshRootNoLink bool
	}
	call := func(t *testing.T, mode lcotel.InboundMode, authToken, bearer string) outcome {
		t.Helper()
		exp := inboundTracing(t, mode)
		cfg := makeBaseConfig()
		cfg.Env.AuthToken = authToken
		srv, _ := makeServer(t, completingProvider(), cfg)
		var o outcome
		h := srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			o.served = true
			_, span := lcotel.RecordRunStart(r.Context(), lcotel.RunStartAttrs{RunID: "r"})
			span.End()
		}))
		req := httptest.NewRequest("POST", "/v1/runs", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("traceparent", inboundParent)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !o.served {
			return o
		}
		s := runSpans(exp)[0]
		o.joined = s.SpanContext.TraceID().String() == inboundTraceID && s.Parent.SpanID().String() == inboundSpanID
		o.linked = len(s.Links) == 1 && s.Links[0].SpanContext.SpanID().String() == inboundSpanID && !s.Parent.IsValid()
		o.freshRootNoLink = !s.Parent.IsValid() && len(s.Links) == 0 && s.SpanContext.TraceID().String() != inboundTraceID
		return o
	}

	if o := call(t, lcotel.InboundParent, "tok", "tok"); !o.served || !o.joined {
		t.Errorf("authenticated, parent mode: %+v, want the run to join the caller's trace", o)
	}
	if o := call(t, lcotel.InboundLink, "tok", "tok"); !o.served || !o.linked {
		t.Errorf("authenticated, link mode: %+v, want a root linking to the caller", o)
	}
	if o := call(t, lcotel.InboundIgnore, "tok", "tok"); !o.served || !o.freshRootNoLink {
		t.Errorf("authenticated, ignore mode: %+v, want a fresh root", o)
	}
	if o := call(t, lcotel.InboundParent, "tok", "wrong"); o.served {
		t.Error("a wrong bearer reached the handler")
	}
	if o := call(t, lcotel.InboundParent, "", ""); !o.served || !o.freshRootNoLink {
		t.Errorf("open mode (unauthenticated): %+v, want the caller's context ignored", o)
	}
}
