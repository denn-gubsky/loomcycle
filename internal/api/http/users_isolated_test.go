package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC BX P2b: the run-start derivation used at every principal-bearing run entry
// (RunOnce / handleRun / handleMessages / admitTeamRun). isolatedForCtx stamps
// RunIdentity.Isolated=true for a substrate:user principal and false for tenant /
// admin / open mode. isolatedOrCaptured additionally honours a bit CAPTURED on a
// trigger def when no principal is present (scheduler/webhook/A2A fire path).
func TestIsolatedForCtx_Matrix(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	ctxWith := func(scopes ...string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "x", Scopes: scopes})
	}
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"substrate:user", ctxWith(auth.ScopeUser), true},
		{"substrate:tenant", ctxWith(auth.ScopeTenant), false},
		{"substrate:admin", ctxWith(auth.ScopeAdmin), false},
		{"user+tenant not isolated", ctxWith(auth.ScopeUser, auth.ScopeTenant), false},
		{"open mode (no principal)", context.Background(), false},
	}
	for _, c := range cases {
		if got := s.isolatedForCtx(c.ctx); got != c.want {
			t.Errorf("%s: isolatedForCtx = %v, want %v", c.name, got, c.want)
		}
	}

	// isolatedOrCaptured: a live principal wins; with NO principal the captured
	// trigger-def bit is authority (anti-bypass for scheduler/webhook/A2A).
	userCtx := ctxWith(auth.ScopeUser)
	if !s.isolatedOrCaptured(userCtx, false) {
		t.Error("isolatedOrCaptured: live substrate:user principal must yield true even when captured=false")
	}
	if s.isolatedOrCaptured(ctxWith(auth.ScopeTenant), true) {
		t.Error("isolatedOrCaptured: live tenant principal must yield false even when captured=true (live principal wins)")
	}
	if !s.isolatedOrCaptured(context.Background(), true) {
		t.Error("isolatedOrCaptured: no principal must fall back to captured=true")
	}
	if s.isolatedOrCaptured(context.Background(), false) {
		t.Error("isolatedOrCaptured: no principal + captured=false must yield false")
	}
}

// RFC BX P2b: a substrate:user (isolated member) principal does NOT satisfy the
// ScopeTenant gate on the operator/tenant-plane routes, so the authMiddleware
// 403s it before any handler runs; it DOES satisfy the run routes (userImplied).
// This is the route-level half of the confinement (the data-tool half is in
// internal/tools/builtin).
func TestRequiredScope_IsolatedUserDeniedTenantRoutes(t *testing.T) {
	user := []string{auth.ScopeUser}

	// Tenant/operator-plane routes → the isolated member is refused.
	denied := []struct{ method, path string }{
		{"GET", "/v1/_memory/scopes"},
		{"GET", "/v1/_channels"},
		{"POST", "/v1/_agentdef"},
		{"GET", "/v1/_usage"},
		{"GET", "/v1/_tenants"}, // ScopeAdmin — also refused
	}
	for _, rt := range denied {
		req := requiredScopeFor(rt.method, rt.path)
		if req == "" {
			t.Fatalf("%s %s has no required scope — test premise is stale", rt.method, rt.path)
		}
		if auth.HasScope(user, req) {
			t.Errorf("substrate:user must be DENIED %s %s (requires %q)", rt.method, rt.path, req)
		}
	}

	// Run routes → the isolated member is allowed (userImplied).
	allowed := []struct{ method, path string }{
		{"POST", "/v1/runs"},
		{"GET", "/v1/runs/abc123"},
	}
	for _, rt := range allowed {
		req := requiredScopeFor(rt.method, rt.path)
		if !auth.HasScope(user, req) {
			t.Errorf("substrate:user must be ALLOWED %s %s (requires %q)", rt.method, rt.path, req)
		}
	}
}

// RFC BX P2b: GET /v1/_users returns ONLY the caller's own record for an isolated
// member — never the tenant roster. A substrate:tenant operator still sees the
// whole tenant (regression).
func TestHandleListUsers_IsolatedSeesOnlySelf(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	// Three registered users in one tenant.
	for _, subj := range []string{"alice", "bob", "carol"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: subj, AccessMode: "tenant", Status: "active"}); err != nil {
			t.Fatalf("seed acme/%s: %v", subj, err)
		}
	}

	listFor := func(p auth.Principal) []wireUserSummary {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/_users", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		s.handleListUsers(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list status %d: %s", rec.Code, rec.Body.String())
		}
		var resp listUsersResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Users
	}

	// Isolated alice → sees ONLY alice.
	isolated := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeUser}}
	got := listFor(isolated)
	if len(got) != 1 || got[0].UserID != "alice" {
		t.Errorf("isolated alice list = %+v, want exactly [alice]", got)
	}

	// Tenant operator → sees the whole tenant roster (regression: not narrowed).
	op := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	if n := len(listFor(op)); n != 3 {
		t.Errorf("tenant operator list = %d users, want 3 (whole tenant)", n)
	}
}
