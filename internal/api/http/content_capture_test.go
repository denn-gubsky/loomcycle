package http

import (
	"context"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tenanttelemetry"
)

// capturedSpan is one span a content collector received: its name and its
// attributes.
type capturedSpan struct {
	name  string
	attrs map[string]string
}

func contentCollector(t *testing.T) (*httptest.Server, func() []capturedSpan) {
	t.Helper()
	var mu sync.Mutex
	var spans []capturedSpan
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		_ = proto.Unmarshal(body, &req)
		mu.Lock()
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					c := capturedSpan{name: sp.GetName(), attrs: map[string]string{}}
					for _, kv := range sp.GetAttributes() {
						c.attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
					}
					spans = append(spans, c)
				}
			}
		}
		mu.Unlock()
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedSpan { mu.Lock(); defer mu.Unlock(); return append([]capturedSpan(nil), spans...) }
}

const capturedCredential = "cred-s3cr3t-9f8e7d6c"

// contentCaptureFixture is a server whose tenant acme exports its traces with
// content capture on, to a collector of its own, and whose redactor holds a
// resolved credential of acme's.
func contentCaptureFixture(t *testing.T, operatorAllows bool) (*Server, *httptest.Server, store.Store, *lcotel.TenantRouter, func() []capturedSpan, *tenanttelemetry.Manager) {
	t.Helper()
	s, ts, st := overrideFixture(t) // agent "other" is no-trace
	s.redactor = newAtRestRedactor(nil)
	s.redactor.RegisterCredential(redact.Holder{Tenant: "acme"}, capturedCredential)
	router := lcotel.NewTenantRouter(lcotel.TenantRouterConfig{ScheduleDelay: 20 * time.Millisecond, ExportTimeout: 2 * time.Second})
	t.Cleanup(lcotel.SetTenantRouterForTest(router))
	m, err := tenanttelemetry.New(tenanttelemetry.Deps{
		Config: tenanttelemetry.Config{Enabled: true, AllowHTTP: true, MaxRatio: 1, ContentCapture: operatorAllows}, Store: st, Router: router,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetTenantTelemetry(m)
	col, got := contentCollector(t)
	if operatorAllows {
		code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody(col.URL, `,"content_capture":"on","content_max_chars":2048`))
		if code != 200 || !strings.Contains(body, `"content_capture":"on","content_max_chars":2048`) {
			t.Fatalf("set: %d %s", code, body)
		}
	}
	return s, ts, st, router, got, m
}

func runSaying(agent, agentID, text string) string {
	return `{"agent":"` + agent + `","agent_id":"` + agentID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"` + text + `"}]}]}`
}

// waitForSpan flushes until a span of name with the attribute key = value
// arrives, and returns every span received.
func waitForSpan(t *testing.T, router *lcotel.TenantRouter, got func() []capturedSpan, name, key, value string) []capturedSpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = router.ForceFlush(context.Background())
		spans := got()
		for _, sp := range spans {
			if sp.name == name && sp.attrs[key] == value {
				return spans
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s span with %s=%s arrived: %v", name, key, value, spans)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContentCapture_TenantCollectorGetsTheRunsContentWithTheCredentialMasked(t *testing.T) {
	_, ts, st, router, got, _ := contentCaptureFixture(t, true)
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runSaying("allowed", "a_cap", "my key is "+capturedCredential)); code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	spans := waitForSpan(t, router, got, lcotel.SpanRun, lcotel.AttrAgentName, "allowed")
	var in, out string
	for _, sp := range spans {
		if sp.name == lcotel.SpanModelCall {
			in, out = sp.attrs["gen_ai.input.messages"], sp.attrs["gen_ai.output.messages"]
		}
		for k, v := range sp.attrs {
			if strings.Contains(v, capturedCredential) {
				t.Errorf("%s %s carries the resolved credential: %q", sp.name, k, v)
			}
		}
	}
	if !strings.Contains(in, "my key is [redacted:credential]") {
		t.Errorf("input messages = %q, want the prompt with the credential masked", in)
	}
	if !strings.Contains(out, `"content":"done"`) {
		t.Errorf("output messages = %q, want the model's answer", out)
	}
	if _, err := st.TenantTelemetryContentExportGet(context.Background(), "acme"); err != nil {
		t.Errorf("the capture is not on record: %v", err)
	}
}

func TestContentCapture_AnAgentThatSaysNeverAndANoTraceRunCaptureNothing(t *testing.T) {
	s, ts, _, router, got, _ := contentCaptureFixture(t, true)
	def := s.cfg().Agents["allowed"]
	def.Telemetry = &config.AgentTelemetry{ContentCapture: config.AgentContentCaptureNever}
	s.cfg().Agents["allowed"] = def
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("other", "a_quiet", "")); code != 200 {
		t.Fatalf("no-trace run: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runSaying("allowed", "a_never", "private words")); code != 200 {
		t.Fatalf("never run: %d %s", code, body)
	}
	spans := waitForSpan(t, router, got, lcotel.SpanRun, lcotel.AttrAgentName, "allowed")
	for _, sp := range spans {
		for _, k := range lcotel.ContentAttributeKeys {
			if v, ok := sp.attrs[string(k)]; ok {
				t.Errorf("%s carries %s = %q", sp.name, k, v)
			}
		}
		if sp.attrs[lcotel.AttrAgentName] == "other" {
			t.Errorf("a span of the no-trace run reached the tenant's collector: %s", sp.name)
		}
	}
	if n := countRunSpans(spans); n != 1 {
		t.Errorf("%d run spans arrived, want only the opted-out agent's", n)
	}
}

func countRunSpans(spans []capturedSpan) int {
	n := 0
	for _, sp := range spans {
		if sp.name == lcotel.SpanRun {
			n++
		}
	}
	return n
}

func TestContentCapture_OperatorGateOffRefusesTheTenantsOn(t *testing.T) {
	_, ts, _, _, _, _ := contentCaptureFixture(t, false)
	code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody("http://127.0.0.1:9", `,"content_capture":"on"`))
	if code != 400 || !strings.Contains(body, "content capture is not allowed on this deployment") {
		t.Fatalf("gate off: %d %s, want 400 naming the operator's gate", code, body)
	}
	// Without capture the policy is stored, and reads back off at the default
	// length.
	code, body = bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody("http://127.0.0.1:9", ""))
	if code != 200 || !strings.Contains(body, `"content_capture":"off","content_max_chars":4096`) {
		t.Fatalf("no capture: %d %s", code, body)
	}
}
