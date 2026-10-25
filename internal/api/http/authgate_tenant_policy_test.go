package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P5: per-tenant strict policy. A tenant whose people must come in
// through named gates refuses a session or an issued token from any other.

func policyServer(t *testing.T, cfgPolicies map[string][]string) (*Server, store.Store, *recordingSink) {
	t.Helper()
	s, st, sink := sessionTestServer(t, "legacy")
	s.cfg().ResolvedAuth = config.AuthPolicy{
		Gates: []config.AuthGate{
			{Name: "local", Kind: config.AuthGateKindPassword, Label: "Sign in"},
			{Name: "paste", Kind: config.AuthGateKindToken, Label: "Access token"},
		},
		TenantPolicies: cfgPolicies,
	}
	return s, st, sink
}

func acmeMemberToken(t *testing.T, s *Server, st store.Store, subject string) string {
	t.Helper()
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", subject, "isolated", "active")
	return mintFor(t, s, ops, subject, "").Token
}

func TestTenantPolicy_ConfigRefusesASessionFromAnUnlistedGate(t *testing.T) {
	s, st, sink := policyServer(t, map[string][]string{"acme": {"local"}})
	tok := acmeMemberToken(t, s, st, "alice")
	rec := doLogin(t, s, loginReq(tok))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "gate_not_allowed") {
		t.Fatalf("paste login of an acme token = %d %s, want 403 gate_not_allowed", rec.Code, rec.Body.String())
	}
	if n := sessionRows(t, st); n != 0 {
		t.Fatalf("a refused login left %d session rows", n)
	}
	if ev := lastEvent(t, sink, "auth_login_failed"); ev.AuthReason != "tenant_policy" {
		t.Fatalf("audit reason = %q", ev.AuthReason)
	}
	// Another tenant has no policy: its token signs in through paste.
	ops := auth.Principal{TenantID: "beta", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "beta", "bob", "isolated", "active")
	mustSession(t, s, mintFor(t, s, ops, "bob", "").Token)
}

func TestTenantPolicy_IssuedTokenFromAnUnlistedGateIsRefused(t *testing.T) {
	s, st, _ := policyServer(t, map[string][]string{"acme": {"local"}})
	ctx := context.Background()
	seedUser(t, st, "acme", "alice", "isolated", "active")
	if _, _, err := s.issueGateToken(ctx, "acme", "alice", "paste", 0, auth.Restrictions{}); !errors.Is(err, errTenantGatePolicy) {
		t.Fatalf("issued through an unlisted gate: err = %v", err)
	}
	if _, _, err := s.issueGateToken(ctx, "acme", "alice", "local", 0, auth.Restrictions{}); err != nil {
		t.Fatalf("issued through the listed gate: %v", err)
	}
}

// A stored row narrows the config and can never widen it.
func TestTenantPolicy_StoredRowOnlyNarrowsTheConfig(t *testing.T) {
	s, st, _ := policyServer(t, map[string][]string{"acme": {"local", "paste"}})
	ctx := context.Background()
	tok := acmeMemberToken(t, s, st, "alice")
	mustSession(t, s, tok) // config allows paste

	if err := st.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "acme", Gates: []string{"local"}}); err != nil {
		t.Fatal(err)
	}
	// The API flushes every replica's policy memo; a direct store write must too.
	s.tenantPolicyCache.flush()
	if rec := doLogin(t, s, loginReq(tok)); rec.Code != http.StatusForbidden {
		t.Fatalf("after the row narrowed acme to local: %d", rec.Code)
	}
	// Config narrows a row the other way: config local only, row paste only
	// → nothing in common → nobody signs in (fail closed), never paste.
	s.cfg().ResolvedAuth.TenantPolicies = map[string][]string{"acme": {"local"}}
	if err := st.TenantAuthPolicyPut(ctx, store.TenantAuthPolicyRow{TenantID: "acme", Gates: []string{"paste"}}); err != nil {
		t.Fatal(err)
	}
	// The API flushes every replica's policy memo; a direct store write must too.
	s.tenantPolicyCache.flush()
	if err := s.tenantGateAllowed(ctx, "acme", "paste"); !errors.Is(err, errTenantGatePolicy) {
		t.Fatalf("a row widened the config to paste: %v", err)
	}
}

type failingPolicyStore struct{ store.Store }

func (failingPolicyStore) TenantAuthPolicyGet(context.Context, string) (store.TenantAuthPolicyRow, error) {
	return store.TenantAuthPolicyRow{}, errDiskFull
}

func TestTenantPolicy_UnreadablePolicyRefusesTheSignIn(t *testing.T) {
	s, st, _ := policyServer(t, nil)
	tok := acmeMemberToken(t, s, st, "alice")
	s.store = failingPolicyStore{Store: st}
	if rec := doLogin(t, s, loginReq(tok)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error: login = %d, want 503 (an unreadable policy is not an absent one)", rec.Code)
	}
}

func adminReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	return r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}))
}

// PUT takes effect on what was already issued: the tenant's sessions and
// gate-issued tokens from a gate it no longer lists end at once; hand-minted
// tokens are left to the operator.
func TestTenantPolicy_PutEndsSessionsAndIssuedTokensOutsideIt(t *testing.T) {
	s, st, _ := policyServer(t, nil)
	ctx := context.Background()
	tok := acmeMemberToken(t, s, st, "alice")
	c := mustSession(t, s, tok)
	issued, _, err := s.issueGateToken(ctx, "acme", "alice", "paste", 0, auth.Restrictions{})
	if err != nil {
		t.Fatal(err)
	}
	keep, _, err := s.issueGateToken(ctx, "acme", "alice", "local", 0, auth.Restrictions{})
	if err != nil {
		t.Fatal(err)
	}

	req := adminReq(http.MethodPut, "/v1/_auth/tenant-policies/acme", `{"gates":["local"]}`)
	req.SetPathValue("tenant", "acme")
	rec := httptest.NewRecorder()
	s.handlePutTenantPolicy(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessions_ended":1`) || !strings.Contains(rec.Body.String(), `"tokens_retired":1`) {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
		t.Fatalf("the paste session still authenticates: %d", code)
	}
	if _, ok := s.resolvePrincipal(ctx, issued); ok {
		t.Fatal("a token issued through paste still authenticates")
	}
	if _, ok := s.resolvePrincipal(ctx, keep); !ok {
		t.Fatal("the token issued through the listed gate was retired")
	}
	if _, ok := s.resolvePrincipal(ctx, tok); !ok {
		t.Fatal("a hand-minted token was retired by the policy")
	}
}

func TestTenantPolicy_PutRefusesUnknownGatesAndAnEmptyResult(t *testing.T) {
	s, _, _ := policyServer(t, map[string][]string{"acme": {"local"}})
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"gates":["nope"]}`, http.StatusBadRequest},
		{`{"gates":[]}`, http.StatusBadRequest},
		{`{"gates":["paste"]}`, http.StatusConflict}, // nothing in common with the config floor
		{`{"gates":["local"],"extra":1}`, http.StatusBadRequest},
	} {
		req := adminReq(http.MethodPut, "/v1/_auth/tenant-policies/acme", tc.body)
		req.SetPathValue("tenant", "acme")
		rec := httptest.NewRecorder()
		s.handlePutTenantPolicy(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("PUT %s = %d %s, want %d", tc.body, rec.Code, rec.Body.String(), tc.code)
		}
	}
}

// Only an admin manages tenant policies: a tenant operator cannot loosen (or
// tighten) its own tenant's.
func TestTenantPolicy_RoutesNeedAdmin(t *testing.T) {
	s, st, _ := policyServer(t, nil)
	seedToken(t, st, "lct_tenant_operator_value_1", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		path := "/v1/_auth/tenant-policies/acme"
		if m == http.MethodGet {
			path = "/v1/_auth/tenant-policies"
		}
		r := httptest.NewRequest(m, path, strings.NewReader(`{"gates":["local"]}`))
		r.Header.Set("Authorization", "Bearer lct_tenant_operator_value_1")
		if code, reached := through(s, r); code != http.StatusForbidden || reached {
			t.Fatalf("%s %s as a tenant operator = %d reached=%v", m, path, code, reached)
		}
	}
}
