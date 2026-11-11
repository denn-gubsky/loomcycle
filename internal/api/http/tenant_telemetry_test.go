package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tenanttelemetry"
)

// telemetryCollector is an OTLP/HTTP collector counting the spans it is sent.
func telemetryCollector(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var spans atomic.Int64
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		_ = proto.Unmarshal(body, &req)
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				spans.Add(int64(len(ss.GetSpans())))
			}
		}
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &spans
}

// withTenantTelemetry turns the feature on for s, exporting over plain HTTP
// to loopback collectors (the lab override and an unguarded dial: these tests
// are about who may, not where to).
func withTenantTelemetry(t *testing.T, s *Server, st store.Store) *tenanttelemetry.Manager {
	t.Helper()
	router := lcotel.NewTenantRouter(lcotel.TenantRouterConfig{ScheduleDelay: 20 * time.Millisecond, ExportTimeout: 2 * time.Second})
	t.Cleanup(func() { _ = router.Shutdown(context.Background()) })
	m, err := tenanttelemetry.New(tenanttelemetry.Deps{
		Config: tenanttelemetry.Config{Enabled: true, AllowHTTP: true, MaxRatio: 1},
		Store:  st, Router: router,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetTenantTelemetry(m)
	return m
}

func telemetryBody(endpoint, extra string) string {
	return `{"endpoint":"` + endpoint + `"` + extra + `}`
}

func decodeTelemetry(t *testing.T, body string) connector.TenantTelemetryResult {
	t.Helper()
	var res connector.TenantTelemetryResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return res
}

func TestTenantTelemetry_GateOffAnswersFeatureDisabledOnEveryRoute(t *testing.T) {
	_, ts, _ := overrideFixture(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/_telemetry", ""},
		{"PUT", "/v1/_telemetry", telemetryBody("https://otel.example", "")},
		{"DELETE", "/v1/_telemetry", ""},
		{"POST", "/v1/_telemetry:test", ""},
	} {
		code, body := bearerCall(t, ts, opsBearer, c.method, c.path, c.body)
		if code != 404 || !strings.Contains(body, `"feature_disabled"`) {
			t.Errorf("%s %s: %d %s, want 404 feature_disabled", c.method, c.path, code, body)
		}
	}
}

func TestTenantTelemetry_OperatorSetsReadsTestsAndDeletesItsOwnPolicy(t *testing.T) {
	s, ts, st := overrideFixture(t)
	withTenantTelemetry(t, s, st)
	sink := &recordingSink{}
	s.SetTokenAudit(sink)
	col, spans := telemetryCollector(t)

	code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_telemetry", "")
	if res := decodeTelemetry(t, body); code != 200 || res.Tenant != "acme" || res.Policy != nil {
		t.Fatalf("get before any policy: %d %s", code, body)
	}
	code, body = bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody(col.URL, `,"sampler_ratio":0.5,"content_capture":"off"`))
	res := decodeTelemetry(t, body)
	if code != 200 || res.Policy == nil || res.Policy.Endpoint != col.URL || res.Policy.Protocol != "http/protobuf" ||
		res.Policy.SamplerRatio != 0.5 || res.Policy.UpdatedBy != "ops" || res.Status == nil || !res.Status.Exporting {
		t.Fatalf("set: %d %s", code, body)
	}
	code, body = bearerCall(t, ts, opsBearer, "POST", "/v1/_telemetry:test", "")
	res = decodeTelemetry(t, body)
	if code != 200 || res.Test == nil || !res.Test.OK || spans.Load() != 1 {
		t.Fatalf("test: %d %s (collector got %d spans)", code, body, spans.Load())
	}
	code, body = bearerCall(t, ts, opsBearer, "DELETE", "/v1/_telemetry", "")
	if res := decodeTelemetry(t, body); code != 200 || !res.Deleted || res.Status.Exporting {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_telemetry", ""); code != 200 || decodeTelemetry(t, body).Policy != nil {
		t.Fatalf("get after delete: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, opsBearer, "POST", "/v1/_telemetry:test", ""); code != 400 {
		t.Errorf("test with no policy: %d %s, want 400", code, body)
	}
	if len(sink.events) != 2 || sink.events[0].Action != "tenant_telemetry_set" || sink.events[0].TargetTenant != "acme" ||
		sink.events[0].TargetName != col.URL || sink.events[1].Action != "tenant_telemetry_delete" {
		t.Errorf("audit = %+v", sink.events)
	}

	// A policy that cannot be audited is not set.
	s.SetTokenAudit(failingSink{err: errors.New("disk full")})
	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody(col.URL, "")); code != 503 || !strings.Contains(body, "audit_unavailable") {
		t.Errorf("set with a failing audit log: %d %s", code, body)
	}
	if rows, _ := st.TenantTelemetryPolicyList(context.Background()); len(rows) != 0 {
		t.Errorf("a policy was stored although its audit record could not be written: %+v", rows)
	}
}

func TestTenantTelemetry_TenantOperatorReachesOnlyItsOwnTenant(t *testing.T) {
	s, ts, st := overrideFixture(t)
	withTenantTelemetry(t, s, st)
	col, _ := telemetryCollector(t)

	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody(col.URL, `,"tenant":"globex"`)); code != 403 {
		t.Errorf("tenant operator writing another tenant: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_telemetry?tenant=globex", ""); code != 403 {
		t.Errorf("tenant operator reading another tenant: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, opsBearer, "DELETE", "/v1/_telemetry?tenant=globex", ""); code != 403 {
		t.Errorf("tenant operator deleting another tenant's: %d %s", code, body)
	}
	if rows, _ := st.TenantTelemetryPolicyList(context.Background()); len(rows) != 0 {
		t.Fatalf("refused writes stored %+v", rows)
	}
	// Naming its own tenant is the same as naming none.
	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry?tenant=acme", telemetryBody(col.URL, "")); code != 200 {
		t.Errorf("tenant operator naming its own tenant: %d %s", code, body)
	}
	// An admin focuses any tenant.
	if code, body := bearerCall(t, ts, adminBearer, "PUT", "/v1/_telemetry", telemetryBody(col.URL, `,"tenant":"globex"`)); code != 200 {
		t.Fatalf("admin writing globex: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, globexBearer, "GET", "/v1/_telemetry", ""); code != 200 || decodeTelemetry(t, body).Policy == nil {
		t.Errorf("globex reading its own: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, adminBearer, "GET", "/v1/_telemetry?tenant=acme", ""); code != 200 || decodeTelemetry(t, body).Policy == nil {
		t.Errorf("admin reading acme: %d %s", code, body)
	}
}

func TestTenantTelemetry_MembersAndRunPlaneTokensAreRefused(t *testing.T) {
	s, ts, st := overrideFixture(t)
	withTenantTelemetry(t, s, st)
	for _, bearer := range []string{freeBearer, restrictedBearer} {
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/v1/_telemetry", ""},
			{"PUT", "/v1/_telemetry", telemetryBody("http://127.0.0.1:1", "")},
			{"DELETE", "/v1/_telemetry", ""},
			{"POST", "/v1/_telemetry:test", ""},
		} {
			if code, body := bearerCall(t, ts, bearer, c.method, c.path, c.body); code != 403 {
				t.Errorf("%s %s %s: %d %s, want 403", bearer, c.method, c.path, code, body)
			}
		}
	}
}

func TestTenantTelemetry_PolicyRefusalsAreBadRequests(t *testing.T) {
	s, ts, st := overrideFixture(t)
	withTenantTelemetry(t, s, st)
	col, _ := telemetryCollector(t)
	for name, body := range map[string]string{
		"metrics":         telemetryBody(col.URL, `,"signals":["traces","metrics"]`),
		"content capture": telemetryBody(col.URL, `,"content_capture":"on"`),
		"ratio":           telemetryBody(col.URL, `,"sampler_ratio":2`),
		"protocol":        telemetryBody(col.URL, `,"protocol":"http/json"`),
		"credential":      telemetryBody(col.URL, `,"headers_credential":"otel"`),
		"unknown field":   telemetryBody(col.URL, `,"headers":{"authorization":"x"}`),
		"no endpoint":     `{}`,
	} {
		if code, resp := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", body); code != 400 {
			t.Errorf("%s: %d %s, want 400", name, code, resp)
		}
	}
}

func TestTenantTelemetry_EnforceModeBaselineAdmitsTheOperatorAndADenyRefuses(t *testing.T) {
	s, ts, st := overrideFixture(t)
	s.SetACLResolver(aclres.New(st, authz.ModeEnforce, 0))
	withTenantTelemetry(t, s, st)
	col, _ := telemetryCollector(t)
	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_telemetry", telemetryBody(col.URL, "")); code != 200 {
		t.Fatalf("enforce: the tenant-operator baseline should admit its own write: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_telemetry", ""); code != 200 {
		t.Fatalf("enforce: read: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, freeBearer, "GET", "/v1/_telemetry", ""); code != 403 {
		t.Errorf("enforce: member: %d %s", code, body)
	}
	putACL(t, st, "no-telemetry", `[{"effect":"deny","actions":["manage"],"resources":["runtime:telemetry"]}]`)
	bind(t, st, "no-telemetry", "user", "ops")
	for _, c := range []struct{ method, path string }{{"PUT", "/v1/_telemetry"}, {"DELETE", "/v1/_telemetry"}, {"POST", "/v1/_telemetry:test"}} {
		if code, body := bearerCall(t, ts, opsBearer, c.method, c.path, telemetryBody(col.URL, "")); code != 403 || !strings.Contains(body, "acl_denied") {
			t.Errorf("enforce with a deny: %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	if code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_telemetry", ""); code != 200 {
		t.Errorf("a deny on manage leaves read: %d %s", code, body)
	}
}
