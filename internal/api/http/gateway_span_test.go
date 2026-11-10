package http

import (
	"context"
	"errors"
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
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
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

const gatewaySpanCanary = "GW-SPAN-CANARY-71c3-no-trace"

// failingGatewayProvider fails the way a provider's 400 can: its error text
// quotes the request, from Call or in the stream.
type failingGatewayProvider struct {
	scriptedProvider
	inStream bool
}

func (p *failingGatewayProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	msg := "invalid request, prompt was: " + gatewaySpanCanary
	if !p.inStream {
		return nil, errors.New(msg)
	}
	ch := make(chan providers.Event, 1)
	ch <- providers.Event{Type: providers.EventError, Error: msg}
	close(ch)
	return ch, nil
}

// spanText is everything a span says in words: attribute values, status and
// event attributes.
func spanText(spans tracetest.SpanStubs) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Status.Description)
		for _, kv := range s.Attributes {
			b.WriteString(" " + kv.Value.Emit())
		}
		for _, ev := range s.Events {
			for _, kv := range ev.Attributes {
				b.WriteString(" " + kv.Value.Emit())
			}
		}
	}
	return b.String()
}

// A no-trace caller's gateway call is traced by class: the provider's error
// text, which can quote the request, reaches no span, and neither do the
// request's parameters. An ordinary caller's span keeps the text, so the
// check can fail.
func TestGatewayCall_ANoTraceCallersSpanCarriesNoErrorText(t *testing.T) {
	temp := 0.3
	for _, inStream := range []bool{false, true} {
		for _, retention := range []string{retentionmode.None, ""} {
			exp, _ := tracedWith(t, sdktrace.AlwaysSample())
			srv, _ := makeServer(t, gatewayProvider(), makeBaseConfig())
			p := auth.Principal{TenantID: "acme", Subject: "alice"}
			p.Restrictions.Retention = retention
			ctx := auth.WithPrincipal(context.Background(), p)
			ch, _, err := srv.callGateway(ctx, &failingGatewayProvider{inStream: inStream}, providers.Request{Model: "m", Temperature: &temp, MaxTokens: 77})
			if err == nil {
				for range ch {
				}
			}
			got := spanText(exp.GetSpans())
			has := strings.Contains(got, gatewaySpanCanary)
			if retention == retentionmode.None {
				if has {
					t.Errorf("inStream=%v: a no-trace caller's span carries the error text: %q", inStream, got)
				}
				for _, s := range exp.GetSpans() {
					for _, kv := range s.Attributes {
						if strings.HasPrefix(string(kv.Key), "gen_ai.request.") && string(kv.Key) != "gen_ai.request.model" {
							t.Errorf("inStream=%v: a no-trace caller's span carries %s", inStream, kv.Key)
						}
					}
				}
			} else if !has {
				t.Fatalf("inStream=%v: an ordinary caller's span carries %q: this test cannot see the text", inStream, got)
			}
		}
	}
}

// A summary made outside a run's loop carries no-trace when the stored row
// is no-trace or the caller's token is, so its model-call span names a
// failure by class.
func TestSummaryRetention_NoTraceFromTheRowOrTheCaller(t *testing.T) {
	plain := context.Background()
	p := auth.Principal{TenantID: "acme", Subject: "alice"}
	p.Restrictions.Retention = retentionmode.None
	noTraceCaller := auth.WithPrincipal(plain, p)
	for _, tc := range []struct {
		ctx  context.Context
		row  string
		want string
	}{
		{plain, "", ""},
		{plain, retentionmode.None, retentionmode.None},
		{noTraceCaller, "", retentionmode.None},
	} {
		if got := summaryRetention(tc.ctx, tc.row); got != tc.want {
			t.Errorf("summaryRetention(row %q) = %q, want %q", tc.row, got, tc.want)
		}
	}
}
