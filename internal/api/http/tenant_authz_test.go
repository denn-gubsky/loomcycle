package http

import (
	"context"
	"encoding/json"
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

	call := func(p auth.Principal) []map[string]any {
		req := principalReq("GET", "/v1/users/alice/agents?status=all", p)
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

	// Tenant=acme principal sees ONLY the acme run.
	tenantAgents := call(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}})
	if len(tenantAgents) != 1 || tenantAgents[0]["agent_id"] != "a_acme" {
		t.Errorf("tenant saw %d agents (want 1 = a_acme): %v", len(tenantAgents), tenantAgents)
	}
	// Admin sees both.
	if adminAgents := call(auth.Principal{TenantID: "x", Scopes: []string{auth.ScopeAdmin}}); len(adminAgents) != 2 {
		t.Errorf("admin saw %d agents, want 2", len(adminAgents))
	}
}
