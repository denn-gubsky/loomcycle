package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func gatewayProvider() *scriptedProvider {
	return &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
	}}
}

func tracedWith(t *testing.T, sampler sdktrace.Sampler) (*tracetest.InMemoryExporter, *tracetest.SpanRecorder) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sampler))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	t.Cleanup(func() { cleanup(); _ = tp.Shutdown(context.Background()) })
	return exp, rec
}

// A gateway call is a model call of its caller's tenant: it has a model-call
// span naming the tenant and the gateway purpose, ended with the stream.
func TestGatewayCall_ModelSpanNamesTheCallersTenantAndPurpose(t *testing.T) {
	exp, rec := tracedWith(t, sdktrace.AlwaysSample())
	srv, _ := makeServer(t, gatewayProvider(), makeBaseConfig())
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice"})
	ch, _, err := srv.callGateway(ctx, gatewayProvider(), providers.Request{Model: "scripted-model"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if started, ended := len(rec.Started()), len(rec.Ended()); started != ended {
		t.Fatalf("%d spans started, %d ended", started, ended)
	}
	model := oneSpan(t, exp.GetSpans(), lcotel.SpanModelCall)
	got := map[string]string{}
	for _, kv := range model.Attributes {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for k, want := range map[string]string{
		lcotel.AttrTenant: "acme", lcotel.AttrModelPurpose: "gateway",
		"gen_ai.usage.input_tokens": "10", "gen_ai.request.model": "scripted-model",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// The tenant sampler decides a gateway call like a run: it is a root span
// carrying its tenant.
func TestGatewayCall_TenantSamplerApplies(t *testing.T) {
	exp, _ := tracedWith(t, lcotel.NewTenantSampler(sdktrace.AlwaysSample(), map[string]float64{"acme": 0}))
	srv, _ := makeServer(t, gatewayProvider(), makeBaseConfig())
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice"})
	ch, _, err := srv.callGateway(ctx, gatewayProvider(), providers.Request{Model: "scripted-model"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if n := len(exp.GetSpans()); n != 0 {
		t.Errorf("a tenant sampled at 0 exported %d gateway spans", n)
	}
}

// Every gateway route, streamed or not, goes through the traced call.
func TestGatewayRoutes_EachTracesItsModelCall(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/_llm/chat", `{"messages":[{"role":"user","content":"hi"}],"provider":"scripted","model":"scripted-model"}`},
		{"/v1/_llm/chat", `{"messages":[{"role":"user","content":"hi"}],"provider":"scripted","model":"scripted-model","stream":true}`},
		{"/v1/chat/completions", `{"model":"scripted-model","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted"}`},
		{"/v1/chat/completions", `{"model":"scripted-model","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted","stream":true}`},
	} {
		exp, _ := tracedWith(t, sdktrace.AlwaysSample())
		srv, _ := makeServer(t, gatewayProvider(), makeBaseConfig())
		ts := httptest.NewServer(srv.Mux())
		resp, err := http.Post(ts.URL+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		ts.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d", tc.path, tc.body, resp.StatusCode)
		}
		model := oneSpan(t, exp.GetSpans(), lcotel.SpanModelCall)
		purpose := ""
		for _, kv := range model.Attributes {
			if string(kv.Key) == lcotel.AttrModelPurpose {
				purpose = kv.Value.AsString()
			}
		}
		if purpose != "gateway" {
			t.Errorf("%s: purpose %q, want gateway", tc.path, purpose)
		}
	}
}
