package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// principalCtx stamps a principal onto a request context (what the
// middleware does in production).
func principalReq(method, target string, p auth.Principal) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	return req.WithContext(auth.WithPrincipal(req.Context(), p))
}

func TestWhoami_AdminTenantOpen(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")

	// Admin principal.
	rec := httptest.NewRecorder()
	s.handleWhoami(rec, principalReq("GET", "/v1/_me", auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}}))
	var admin map[string]any
	json.Unmarshal(rec.Body.Bytes(), &admin)
	if admin["is_admin"] != true || admin["tenant_id"] != "acme" {
		t.Errorf("admin whoami = %v", admin)
	}

	// Tenant principal.
	rec = httptest.NewRecorder()
	s.handleWhoami(rec, principalReq("GET", "/v1/_me", auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}}))
	var tenant map[string]any
	json.Unmarshal(rec.Body.Bytes(), &tenant)
	if tenant["is_admin"] != false || tenant["tenant_id"] != "acme" || tenant["subject"] != "alice" {
		t.Errorf("tenant whoami = %v", tenant)
	}

	// Open mode (no principal) → synthetic admin so the dev UI works.
	rec = httptest.NewRecorder()
	s.handleWhoami(rec, httptest.NewRequest("GET", "/v1/_me", nil))
	var open map[string]any
	json.Unmarshal(rec.Body.Bytes(), &open)
	if open["is_admin"] != true || open["open_mode"] != true {
		t.Errorf("open-mode whoami = %v", open)
	}
}

func TestPrincipalTenantScopeAndVisible(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy")
	adminCtx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Scopes: []string{auth.ScopeAdmin}})
	tenantCtx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Scopes: []string{auth.ScopeRunsRead}})

	// Admin: wire tenant honored; unset = all.
	if tid, all := s.principalTenantScope(adminCtx, "focus"); tid != "focus" || all {
		t.Errorf("admin focus = (%q,%v)", tid, all)
	}
	if _, all := s.principalTenantScope(adminCtx, ""); !all {
		t.Error("admin no-tenant should be all=true")
	}
	// Tenant: forced to own tenant, wire ignored, never all.
	if tid, all := s.principalTenantScope(tenantCtx, "other"); tid != "acme" || all {
		t.Errorf("tenant scope = (%q,%v), want (acme,false) — wire 'other' must be ignored", tid, all)
	}
	// tenantVisible.
	if !s.tenantVisible(adminCtx, "anything") {
		t.Error("admin sees any tenant")
	}
	if !s.tenantVisible(tenantCtx, "acme") || s.tenantVisible(tenantCtx, "other") {
		t.Error("tenant sees only its own tenant")
	}
}

// The security substance: a tenant principal listing agents sees only
// runs in its own tenant, even when the requested user_id has runs in
// another tenant.
func TestHandleListUserAgents_TenantFiltersCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	// Seed user "alice" with one run in tenant acme and one in tenant other.
	for _, tt := range []struct{ tenant, agentID string }{{"acme", "a_acme"}, {"other", "a_other"}} {
		sess, err := st.CreateSession(ctx, tt.tenant, "echo", "alice")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: tt.agentID, UserID: "alice", TenantID: tt.tenant}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}

	call := func(p auth.Principal, target string) []map[string]any {
		req := principalReq("GET", target, p)
		req.SetPathValue("user_id", "alice")
		rec := httptest.NewRecorder()
		s.handleListUserAgents(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Agents []map[string]any `json:"agents"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Agents
	}

	base := "/v1/users/alice/agents?status=all"
	// Tenant=acme principal sees ONLY the acme run.
	tenantAgents := call(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}}, base)
	if len(tenantAgents) != 1 || tenantAgents[0]["agent_id"] != "a_acme" {
		t.Errorf("tenant saw %d agents (want 1 = a_acme): %v", len(tenantAgents), tenantAgents)
	}
	// Admin sees both.
	if adminAgents := call(auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}, base); len(adminAgents) != 2 {
		t.Errorf("admin saw %d agents, want 2", len(adminAgents))
	}
	// Admin focusing ?tenant=other sees ONLY the other run (the UI's
	// tenant-focus switcher).
	if focused := call(auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}, base+"&tenant=other"); len(focused) != 1 || focused[0]["agent_id"] != "a_other" {
		t.Errorf("admin ?tenant=other saw %d agents (want 1 = a_other): %v", len(focused), focused)
	}
	// A tenant principal cannot widen via ?tenant= — still only its own.
	if escaped := call(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}}, base+"&tenant=other"); len(escaped) != 1 || escaped[0]["agent_id"] != "a_acme" {
		t.Errorf("tenant ?tenant=other leaked %d agents (want 1 = a_acme): %v", len(escaped), escaped)
	}
}

// Another tenant's newer runs for a colliding user id must not crowd a
// tenant's own runs out of the store's row bound: all 5 acme runs are listed
// even though tenant other has 150 newer ones for the same "alice".
func TestHandleListUserAgents_TenantSeesAllOwnRunsPastOtherTenantsNewer(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	seed := func(tenant string, n int) {
		sess, err := st.CreateSession(ctx, tenant, "echo", "alice")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		for i := 0; i < n; i++ {
			if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: fmt.Sprintf("a_%s_%d", tenant, i), UserID: "alice", TenantID: tenant}); err != nil {
				t.Fatalf("create run: %v", err)
			}
		}
	}
	seed("acme", 5)
	seed("other", 150)

	req := principalReq("GET", "/v1/users/alice/agents?status=all", auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}})
	req.SetPathValue("user_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListUserAgents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Agents) != 5 {
		t.Errorf("acme saw %d agents, want all 5 of its own", len(resp.Agents))
	}
}

// The store reads tenant "" as "all tenants", but a non-admin principal whose
// tenant is "" (a config-declared principal may omit it) is confined to tenant
// "". The handler's post-filter is what keeps it there.
func TestHandleListUserAgents_EmptyTenantPrincipalSeesOnlyEmptyTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	for _, tt := range []struct{ tenant, agentID string }{{"", "a_default"}, {"other", "a_other"}} {
		sess, err := st.CreateSession(ctx, tt.tenant, "echo", "alice")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: tt.agentID, UserID: "alice", TenantID: tt.tenant}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}

	req := principalReq("GET", "/v1/users/alice/agents?status=all", auth.Principal{Subject: "alice", Scopes: []string{auth.ScopeRunsRead}})
	req.SetPathValue("user_id", "alice")
	rec := httptest.NewRecorder()
	s.handleListUserAgents(rec, req)
	var resp struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Agents) != 1 || resp.Agents[0]["agent_id"] != "a_default" {
		t.Errorf("empty-tenant principal saw %v, want only a_default", resp.Agents)
	}
}

// /v1/_users is reachable by any authenticated principal and tenant-scoped:
// a tenant sees only its own tenant's users; admin sees all and can focus
// one via ?tenant=. Drives the Web UI's per-tenant user picker.
func TestHandleListUsers_TenantScope(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	for _, tt := range []struct{ tenant, user, agentID string }{
		{"acme", "alice", "a_alice"},
		{"acme", "bob", "a_bob"},
		{"other", "carol", "a_carol"},
	} {
		sess, err := st.CreateSession(ctx, tt.tenant, "echo", tt.user)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: tt.agentID, UserID: tt.user, TenantID: tt.tenant}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}

	call := func(p auth.Principal, target string) map[string]bool {
		rec := httptest.NewRecorder()
		s.handleListUsers(rec, principalReq("GET", target, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Users []struct {
				UserID string `json:"user_id"`
			} `json:"users"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		ids := map[string]bool{}
		for _, u := range resp.Users {
			ids[u.UserID] = true
		}
		return ids
	}

	// Tenant=acme sees alice + bob, never carol.
	tenant := call(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}}, "/v1/_users")
	if !tenant["alice"] || !tenant["bob"] || tenant["carol"] {
		t.Errorf("tenant acme saw %v, want {alice,bob} without carol", tenant)
	}
	// Admin (no focus) sees all three.
	admin := call(auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}, "/v1/_users")
	if !admin["alice"] || !admin["bob"] || !admin["carol"] {
		t.Errorf("admin saw %v, want all three", admin)
	}
	// Admin focusing ?tenant=other sees only carol.
	focused := call(auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}, "/v1/_users?tenant=other")
	if focused["alice"] || focused["bob"] || !focused["carol"] {
		t.Errorf("admin ?tenant=other saw %v, want only carol", focused)
	}
	// Tenant can't widen via ?tenant= — still only acme users.
	escaped := call(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}}, "/v1/_users?tenant=other")
	if escaped["carol"] || !escaped["alice"] {
		t.Errorf("tenant ?tenant=other leaked %v, want only acme users", escaped)
	}
}

// /v1/_users must be exempt from the admin scope gate so a tenant token
// can reach it (the handler does the tenant scoping, not the route).
func TestRequiredScopeFor_UsersExempt(t *testing.T) {
	if got := requiredScopeFor("GET", "/v1/_users"); got != "" {
		t.Errorf("requiredScopeFor(/v1/_users) = %q, want \"\" (any authenticated)", got)
	}
}
