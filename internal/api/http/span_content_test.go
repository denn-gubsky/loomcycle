package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// runTraced starts one run over HTTP against prov and returns the spans it
// ended.
func runTraced(t *testing.T, prov providers.Provider, body string) tracetest.SpanStubs {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()
	srv, _ := makeServer(t, prov, makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	return exp.GetSpans()
}

func oneSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d %s spans, want 1", len(found), name)
	}
	return found[0]
}

func TestRunSpan_AFailedRunIsAnError(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]providers.Event{{
		{Type: providers.EventError, Error: "scripted 400: bad request"},
	}}}
	run := oneSpan(t, runTraced(t, prov, runBody("default")), lcotel.SpanRun)
	if run.Status.Code != codes.Error {
		t.Fatalf("a failed run's span status = %v %q, want Error", run.Status.Code, run.Status.Description)
	}
}

func TestRunSpan_ACompletedRunIsNotAnError(t *testing.T) {
	run := oneSpan(t, runTraced(t, completingProvider(), runBody("default")), lcotel.SpanRun)
	if run.Status.Code == codes.Error {
		t.Fatalf("a completed run's span is an error: %q", run.Status.Description)
	}
}

// The run's spans all name its tenant, the run span its chat, and the model
// call sits between the iteration and the provider: the wiring the recorder
// unit tests cannot see.
func TestRunSpans_CarryTheTenantAndTheModelCall(t *testing.T) {
	defer lcotel.SetUserIDPolicyForTest([]byte("0123456789abcdef0123456789abcdef"), false)()
	spans := runTraced(t, completingProvider(),
		`{"agent":"default","tenant_id":"acme","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)
	run := oneSpan(t, spans, lcotel.SpanRun)
	iter := oneSpan(t, spans, lcotel.SpanIteration)
	model := oneSpan(t, spans, lcotel.SpanModelCall)
	if model.Parent.SpanID() != iter.SpanContext.SpanID() {
		t.Error("the model-call span is not the iteration's child")
	}
	attrs := func(s tracetest.SpanStub) map[string]string {
		m := map[string]string{}
		for _, kv := range s.Attributes {
			m[string(kv.Key)] = kv.Value.Emit()
		}
		return m
	}
	ra := attrs(run)
	if ra["gen_ai.conversation.id"] == "" || ra["gen_ai.operation.name"] != "invoke_agent" {
		t.Errorf("run span gen_ai attributes: %v", ra)
	}
	if _, raw := ra[lcotel.AttrUserID]; raw || ra[lcotel.AttrUserHash] == "" {
		t.Errorf("run span user attributes: %v", ra)
	}
	for _, s := range []tracetest.SpanStub{run, iter, model} {
		if got := attrs(s)[lcotel.AttrTenant]; got != "acme" {
			t.Errorf("%s: %s = %q, want acme", s.Name, lcotel.AttrTenant, got)
		}
	}
}
