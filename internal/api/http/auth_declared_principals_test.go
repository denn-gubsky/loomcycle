package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC ES F2: declaring config `principals:` turns authentication on. Before,
// the open-mode decision looked only at the legacy secret and the admin
// operator tokens, so a deployment that declared only principals ran open —
// every caller the default admin — and its principals never authenticated.

const declaredAliceSecret = "lct_declared_alice"

// declaredOnlyServer is a server whose ONLY credential is one config principal
// (no legacy secret, no operator token in the store).
func declaredOnlyServer(t *testing.T) *Server {
	t.Helper()
	s, _ := tokenAuthServer(t, "")
	cfg := s.cfg()
	cfg.Principals = map[string]config.PrincipalDef{
		"alice": {Tenant: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_ALICE_TOKEN"},
	}
	cfg.ResolvedPrincipals = []auth.DeclaredPrincipal{{
		Secret:    declaredAliceSecret,
		Principal: auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}, TokenDefID: "cfg:alice"},
	}}
	return s
}

func TestAuthMiddleware_DeclaredPrincipalsOnlyRefusesNoBearer(t *testing.T) {
	s := declaredOnlyServer(t)
	var reached bool
	rec := httptest.NewRecorder()
	s.authMiddleware(passthroughHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/_me", nil))
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("no bearer with only config principals: status = %d, reached = %v; want 401 and not reached", rec.Code, reached)
	}
}

func TestAuthMiddleware_DeclaredPrincipalAuthenticatesAsItself(t *testing.T) {
	s := declaredOnlyServer(t)
	req := httptest.NewRequest("GET", "/v1/_me", nil)
	req.Header.Set("Authorization", "Bearer "+declaredAliceSecret)
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleWhoami)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("principal bearer: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me["tenant_id"] != "acme" || me["subject"] != "alice" || me["is_admin"] != false {
		t.Errorf("/v1/_me = %v, want the declared principal acme/alice, not admin", me)
	}
	if _, open := me["open_mode"]; open {
		t.Errorf("/v1/_me reports open_mode with principals declared: %v", me)
	}
}

// A declared principal whose secret is unset at boot authenticates nobody, but
// it still says the operator meant auth to be on. Reading it as "no auth"
// would serve the whole API to anyone because an env var was missing.
func TestAuthConfigured_InertDeclaredPrincipalStillTurnsAuthOn(t *testing.T) {
	s, _ := tokenAuthServer(t, "")
	s.cfg().Principals = map[string]config.PrincipalDef{
		"ops": {Subject: "ops", Scopes: []string{auth.ScopeAdmin}, TokenEnv: "LOOMCYCLE_OPS_TOKEN"},
	}
	on, err := s.AuthConfiguredErr(context.Background())
	if err != nil || !on {
		t.Fatalf("AuthConfiguredErr with an inert declared principal = (%v, %v), want (true, nil)", on, err)
	}
	var reached bool
	rec := httptest.NewRecorder()
	s.authMiddleware(passthroughHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/x", nil))
	if rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("no bearer with an inert principal: status = %d, reached = %v; want 401", rec.Code, reached)
	}
}

// A principals-only server whose principals are all inert, with no token in the
// store, turns auth on but lets nobody in; one whose principals lack admin has
// nobody to administer it. Boot names both, with the fix.
func TestAuthPostureWarnings_NamesNoLoginAndNoAdmin(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.cfg().Principals = map[string]config.PrincipalDef{
		"ops": {Subject: "ops", Scopes: []string{auth.ScopeAdmin}, TokenEnv: "LOOMCYCLE_OPS_TOKEN"},
	}
	got := strings.Join(s.AuthPostureWarnings(context.Background()), "\n")
	if !strings.Contains(got, "401") || !strings.Contains(got, "substrate:admin principal or set LOOMCYCLE_AUTH_TOKEN") {
		t.Fatalf("inert principals, empty store: warnings = %q, want both the 401 and the no-admin warning", got)
	}

	// A token row lets someone in, but it is not an admin: only the admin warning.
	seedToken(t, st, "lct_member", "acme", "member", []string{auth.ScopeRunsRead}, time.Time{})
	got = strings.Join(s.AuthPostureWarnings(context.Background()), "\n")
	if strings.Contains(got, "401") || !strings.Contains(got, "substrate:admin") {
		t.Fatalf("a member token in the store: warnings = %q, want only the no-admin warning", got)
	}

	// A resolved admin principal: nothing to say.
	s.cfg().ResolvedPrincipals = []auth.DeclaredPrincipal{{Secret: "lct_ops", Principal: auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeAdmin}}}}
	if w := s.AuthPostureWarnings(context.Background()); len(w) != 0 {
		t.Fatalf("a live admin principal: warnings = %q, want none", w)
	}
}
