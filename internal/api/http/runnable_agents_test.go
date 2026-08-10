package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestRunnableIncludesTenant pins the RFC BY tiering decision — the
// security-bearing part of discovery. An ISOLATED token never sees tenant
// agents (a hard floor from the scopes, so a stale access_mode column can't
// widen it); a tenant-mode user is governed by its authoritative
// users.access_mode row; a non-user principal (tenant operator / admin / open
// mode) has no users row and defaults to tenant-visible.
func TestRunnableIncludesTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedUser(t, st, "acme", "bob", "tenant", "active")
	isolated, _ := auth.GrantableUserScopes("isolated")
	tenantMode, _ := auth.GrantableUserScopes("tenant")

	ctxWith := func(p *auth.Principal) context.Context {
		if p == nil {
			return context.Background()
		}
		return auth.WithPrincipal(context.Background(), *p)
	}
	for _, tc := range []struct {
		name string
		p    *auth.Principal
		want bool
	}{
		{"isolated user", &auth.Principal{TenantID: "acme", Subject: "alice", Scopes: isolated}, false},
		{"tenant-mode user", &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: tenantMode}, true},
		{"tenant operator (no users row)", &auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}, true},
		{"admin (no users row)", &auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, true},
		{"open mode (no principal)", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.runnableIncludesTenant(ctxWith(tc.p)); got != tc.want {
				t.Errorf("runnableIncludesTenant = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHandleRunnableAgents_TieredCatalog is the end-to-end discovery contract:
// bundled agents show for everyone; the tenant's substrate agents show for a
// tenant-mode user but NOT an isolated one; and a DIFFERENT tenant's agent is
// never leaked to either. Reverting the isolation floor (showing tenant agents
// to an isolated user) must break the isolated case.
func TestHandleRunnableAgents_TieredCatalog(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.cfg.Agents = map[string]config.AgentDef{"chat": {Model: "stub"}} // bundled floor

	// A tenant substrate agent for acme, and one for a DIFFERENT tenant.
	mustSeedAgentDef(t, st, "acme", "tenant-bot")
	mustSeedAgentDef(t, st, "beta", "other-tenant-bot")

	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedUser(t, st, "acme", "bob", "tenant", "active")
	isolated, _ := auth.GrantableUserScopes("isolated")
	tenantMode, _ := auth.GrantableUserScopes("tenant")

	call := func(p auth.Principal) map[string]string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/_runnable-agents", nil)
		req = req.WithContext(auth.WithPrincipal(context.Background(), p))
		s.handleRunnableAgents(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp runnableAgentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		bySource := map[string]string{}
		for _, a := range resp.Agents {
			bySource[a.Name] = a.Source
		}
		return bySource
	}

	// Isolated user: bundled ONLY — no tenant agents, and never the other tenant's.
	iso := call(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: isolated})
	if iso["chat"] != "bundled" {
		t.Errorf("isolated user missing bundled 'chat': %v", iso)
	}
	if _, ok := iso["tenant-bot"]; ok {
		t.Errorf("isolated user must NOT see the tenant agent: %v", iso)
	}
	if _, ok := iso["other-tenant-bot"]; ok {
		t.Errorf("isolated user leaked ANOTHER tenant's agent: %v", iso)
	}

	// Tenant-mode user: bundled + its OWN tenant's agent, never the other tenant's.
	ten := call(auth.Principal{TenantID: "acme", Subject: "bob", Scopes: tenantMode})
	if ten["chat"] != "bundled" {
		t.Errorf("tenant-mode user missing bundled 'chat': %v", ten)
	}
	if ten["tenant-bot"] != "tenant" {
		t.Errorf("tenant-mode user must see its tenant agent as source=tenant: %v", ten)
	}
	if _, ok := ten["other-tenant-bot"]; ok {
		t.Errorf("tenant-mode user leaked ANOTHER tenant's agent: %v", ten)
	}
}

func mustSeedAgentDef(t *testing.T, st store.Store, tenant, name string) {
	t.Helper()
	if _, err := st.AgentDefCreate(context.Background(), store.AgentDefRow{
		DefID:      "def_" + tenant + "_" + name,
		Name:       name,
		Version:    1,
		Definition: []byte(`{"system_prompt":"x"}`),
		CreatedAt:  time.Now(),
		TenantID:   tenant,
	}); err != nil {
		t.Fatalf("seed agentdef %s/%s: %v", tenant, name, err)
	}
}
