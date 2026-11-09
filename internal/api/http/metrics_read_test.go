package http

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

const metricsBearer = "scrape-bearer"

// The route census: through the real authMiddleware, a metrics:read token is
// admitted to GET and HEAD /metrics and refused (403) on every other route
// the mux registers — the routes any authenticated principal may call
// (whoami, the MCP transport, the gateway), the DB-backed /v1/_metrics/*
// dashboards and self-revocation included.
func TestMetricsRead_ReachesOnlyTheScrape(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	seedToken(t, st, metricsBearer, "acme", "prometheus", []string{auth.ScopeMetricsRead}, time.Time{})

	var got []string
	for _, r := range eqRoutes(t) {
		key := r.method + " " + r.pattern
		reached, code := runMW(t, s, r.method, r.path, metricsBearer)
		if reached && key != "GET /metrics" {
			got = append(got, key)
		}
		if !reached && code != http.StatusForbidden {
			t.Errorf("%s: refused with %d, want 403", key, code)
		}
	}
	sort.Strings(got)
	if len(got) > 0 {
		t.Errorf("metrics:read reaches routes beyond the scrape: %v", got)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if reached, code := runMW(t, s, method, "/metrics", metricsBearer); !reached {
			t.Errorf("%s /metrics: refused with %d, want admitted", method, code)
		}
	}
	// Named explicitly so a change to the route enumeration cannot drop them.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/_metrics/summary"}, {"GET", "/v1/_me"}, {"GET", "/v1/runs"}, {"POST", "/v1/runs"},
		{"POST", "/v1/_mcp"}, {"POST", "/v1/chat/completions"}, {"GET", "/v1/_channels"},
		{"POST", "/v1/_auth/token/revoke"}, {"POST", "/v1/_operatortokendef"},
	} {
		if reached, code := runMW(t, s, c.method, c.path, metricsBearer); reached || code != http.StatusForbidden {
			t.Errorf("%s %s: reached=%v code=%d, want refused 403", c.method, c.path, reached, code)
		}
	}
}

// metrics:read is an addition, not a replacement: an admin token keeps the
// scrape, and a tenant operator's token is refused it as before.
func TestMetricsRead_AdminKeepsTheScrapeAndATenantTokenIsRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	seedToken(t, st, "admin-bearer", "ops", "ops", []string{auth.ScopeAdmin}, time.Time{})
	seedToken(t, st, "tenant-bearer", "acme", "acme-ops", []string{auth.ScopeTenant}, time.Time{})
	if reached, code := runMW(t, s, "GET", "/metrics", "admin-bearer"); !reached {
		t.Errorf("admin: refused with %d", code)
	}
	if reached, code := runMW(t, s, "GET", "/metrics", "tenant-bearer"); reached || code != http.StatusForbidden {
		t.Errorf("tenant: reached=%v code=%d, want refused 403", reached, code)
	}
}

// Under LOOMCYCLE_ACL=enforce the metrics-reader baseline answers the
// scrape's question, so no hand-written rule is needed; an explicit deny on
// runtime:metrics bound to the token's subject still refuses it.
func TestMetricsRead_EnforceAdmitsByBaselineAndAnExplicitDenyRefuses(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	seedToken(t, st, metricsBearer, "acme", "prometheus", []string{auth.ScopeMetricsRead}, time.Time{})
	s.SetACLResolver(aclres.New(st, authz.ModeEnforce, 0))

	if reached, code := runMW(t, s, "GET", "/metrics", metricsBearer); !reached {
		t.Fatalf("enforce, no rules: refused with %d, want admitted by the baseline", code)
	}
	putACL(t, st, "no-scrape", `[{"effect":"deny","actions":["read"],"resources":["runtime:metrics"]}]`)
	bind(t, st, "no-scrape", "user", "prometheus")
	if reached, code := runMW(t, s, "GET", "/metrics", metricsBearer); reached || code != http.StatusForbidden {
		t.Errorf("enforce, deny bound: reached=%v code=%d, want refused 403", reached, code)
	}
}

// Minting a token is the operator's: a tenant operator's token cannot reach
// the mint route at all, so it cannot hand out a scrape credential either.
func TestMetricsRead_ATenantOperatorCannotMintIt(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	seedToken(t, st, "tenant-bearer", "acme", "acme-ops", []string{auth.ScopeTenant}, time.Time{})
	if reached, code := runMW(t, s, "POST", "/v1/_operatortokendef", "tenant-bearer"); reached || code != http.StatusForbidden {
		t.Errorf("tenant mint: reached=%v code=%d, want refused 403", reached, code)
	}
	if reached, code := runMW(t, s, "POST", "/v1/_operatortokendef", "legacy-secret"); !reached {
		t.Errorf("operator mint: refused with %d", code)
	}
}

// The own-identity carve-out admits a principal to its OWN identity routes
// past the scope gate; a scrape credential is no person and gets no carve-out.
func TestOwnIdentityRequest_RefusesAMetricsReader(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/_users/prometheus/identities", nil)
	r.Pattern = "GET /v1/_users/{subject}/identities"
	r.SetPathValue("subject", "prometheus")
	member := auth.Principal{TenantID: "acme", Subject: "prometheus", Scopes: []string{auth.ScopeRunsRead}}
	if !ownIdentityRequest(member, r) {
		t.Fatal("the fixture is wrong: a member's own identity request is not admitted")
	}
	scraper := auth.Principal{TenantID: "acme", Subject: "prometheus", Scopes: []string{auth.ScopeMetricsRead}}
	if ownIdentityRequest(scraper, r) {
		t.Error("a metrics:read credential was admitted to an identity route")
	}
}

// A scrape credential does not sign in to the Web UI: the login is refused
// with a reason, audited, and leaves no session behind.
func TestUILogin_RefusesAMetricsReadToken(t *testing.T) {
	s, st, sink := sessionTestServer(t, "legacy-secret")
	seedToken(t, st, metricsBearer, "acme", "prometheus", []string{auth.ScopeMetricsRead}, time.Time{})
	rec := doLogin(t, s, loginReq(metricsBearer))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "metrics_credential_refused") {
		t.Fatalf("status = %d body = %s, want 403 metrics_credential_refused", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, sink, "auth_login_failed"); ev.AuthReason != "metrics_credential" || ev.ActorSubject != "prometheus" {
		t.Errorf("auth_login_failed = %+v", ev)
	}
	if n := sessionRows(t, st); n != 0 {
		t.Errorf("%d session rows after a refused login, want 0", n)
	}
}
