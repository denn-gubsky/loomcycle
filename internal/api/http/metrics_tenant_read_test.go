package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedScrapeToken stores a metrics:read token for (tenant, subject) whose
// restrictions record is restrictionsJSON, stored as given.
func seedScrapeToken(t *testing.T, st store.Store, plaintext, tenant, subject, restrictionsJSON string) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_" + subject, Name: subject, TenantID: tenant, Subject: subject,
		TokenHash:        auth.HashToken(testTokenPepper, plaintext),
		AllowedScopes:    []string{auth.ScopeMetricsRead},
		RestrictionsJSON: restrictionsJSON,
	}); err != nil {
		t.Fatalf("seed scrape token: %v", err)
	}
}

// scrape serves GET /metrics through the real authMiddleware with bearer.
func scrape(t *testing.T, s *Server, bearer string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleMetricsProm)).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// A metrics:read token holding tenant_metrics reads its own tenant's
// instrument series and nothing else: no other tenant's, and none of the
// whole runtime's substrate series.
func TestMetricsProm_ATenantMetricsTokenSeesOnlyItsOwnTenant(t *testing.T) {
	installMetrics(t)
	recordTwoTenantsRuns()
	s, st := tokenAuthServer(t, "legacy-secret")
	seedScrapeToken(t, st, "acme-scrape", "acme", "acme-prom", `{"tenant_metrics":true}`)

	code, body := scrape(t, s, "acme-scrape")
	if code != http.StatusOK {
		t.Fatalf("scrape: %d %s", code, body)
	}
	if !strings.Contains(body, `loomcycle_runs_total{status="completed",tenant="acme",trigger="http"} 1`) {
		t.Errorf("its own tenant's series is missing:\n%s", body)
	}
	for _, absent := range []string{`tenant="beta"`, "loomcycle_process_", "loomcycle_concurrency_",
		"loomcycle_build_info", "loomcycle_otel_spans", "loomcycle_acl_", "loomcycle_tool_panics"} {
		if strings.Contains(body, absent) {
			t.Errorf("tenant scrape contains %q:\n%s", absent, body)
		}
	}
}

// A metrics:read token without the marker keeps the whole view, whatever
// tenant it was minted in: the observability recipes mint theirs in "ops".
func TestMetricsProm_AnUnmarkedMetricsTokenKeepsTheOperatorView(t *testing.T) {
	installMetrics(t)
	recordTwoTenantsRuns()
	s, st := tokenAuthServer(t, "legacy-secret")
	seedScrapeToken(t, st, "ops-scrape", "ops", "prometheus", "")

	code, body := scrape(t, s, "ops-scrape")
	if code != http.StatusOK {
		t.Fatalf("scrape: %d %s", code, body)
	}
	for _, present := range []string{"loomcycle_process_goroutines", "loomcycle_build_info", `tenant="acme"`, `tenant="beta"`} {
		if !strings.Contains(body, present) {
			t.Errorf("operator scrape lacks %q", present)
		}
	}
}

// A marked token whose restrictions record cannot be read never gets the
// operator view: the token is refused outright.
func TestMetricsProm_AMarkedTokenWithAnUnreadableRecordGetsNoOperatorView(t *testing.T) {
	installMetrics(t)
	recordTwoTenantsRuns()
	s, st := tokenAuthServer(t, "legacy-secret")
	seedScrapeToken(t, st, "broken-scrape", "acme", "acme-prom", `{"tenant_metrics":true,"from_a_newer_build":1}`)

	code, body := scrape(t, s, "broken-scrape")
	if code == http.StatusOK || strings.Contains(body, "loomcycle_") {
		t.Errorf("scrape with an unreadable record: %d\n%s", code, body)
	}
}

// The view each kind of caller gets, decided from the principal alone.
func TestPromViewForPrincipal_ConfinesOnlyAMarkedMetricsReader(t *testing.T) {
	marked := auth.Principal{TenantID: "acme", Subject: "p", Scopes: []string{auth.ScopeMetricsRead}, Restrictions: auth.Restrictions{TenantMetrics: true}}
	if v := promViewForPrincipal(marked, true); !v.isConfined() || v.tenant != "acme" {
		t.Errorf("marked reader: %+v, want confined to acme", v)
	}
	shared := marked
	shared.TenantID = ""
	if v := promViewForPrincipal(shared, true); !v.isConfined() {
		t.Errorf("marked reader of the shared tenant: %+v, want confined (not the operator view)", v)
	}
	for name, p := range map[string]auth.Principal{
		"unmarked reader": {TenantID: "ops", Subject: "p", Scopes: []string{auth.ScopeMetricsRead}},
		"admin":           {TenantID: "ops", Subject: "a", Scopes: []string{auth.ScopeAdmin}},
		// The marker means nothing on a token that is not a scrape token.
		"marked admin": {TenantID: "ops", Subject: "a", Scopes: []string{auth.ScopeAdmin}, Restrictions: auth.Restrictions{TenantMetrics: true}},
	} {
		if v := promViewForPrincipal(p, true); v.isConfined() {
			t.Errorf("%s: %+v, want the operator view", name, v)
		}
	}
	if v := promViewForPrincipal(auth.Principal{}, false); v.isConfined() {
		t.Errorf("no principal (open mode): %+v, want the operator view", v)
	}
}
