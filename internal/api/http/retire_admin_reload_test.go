package http

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// An admin config principal lifts the last-admin-token retire guard (RFC ES
// F2). A reload and a token retire, in either order and on either replica,
// must not leave the server with no admin credential — or, with no principal
// left, in open mode. The reload decides both on its config alone (RFC ES F2
// for open mode, C1 for the admin credential): a store token can be retired
// right after the check, by a replica whose own config lifts its guard.

var (
	raOps = config.PrincipalDef{Subject: "ops", Scopes: []string{auth.ScopeAdmin}, TokenEnv: "LOOMCYCLE_OPS_TOKEN"}
	raAli = config.PrincipalDef{Tenant: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_ALICE_TOKEN"}
)

// raResolved is each test principal as a real load resolves it: with its secret.
var raResolved = map[string]auth.DeclaredPrincipal{
	"ops":   {Secret: "lct_ra_ops", Principal: auth.Principal{Subject: "ops", Scopes: raOps.Scopes, TokenDefID: "cfg:ops"}},
	"alice": {Secret: "lct_ra_alice", Principal: auth.Principal{TenantID: "acme", Subject: "alice", Scopes: raAli.Scopes, TokenDefID: "cfg:alice"}},
}

// raConfig fills c with the named test principals (sorted, so the resolved
// list is stable) and returns it.
func raConfig(c *config.Config, principals map[string]config.PrincipalDef) *config.Config {
	c.Principals = principals
	c.ResolvedPrincipals = nil
	for _, name := range []string{"alice", "ops"} {
		if _, ok := principals[name]; ok {
			c.ResolvedPrincipals = append(c.ResolvedPrincipals, raResolved[name])
		}
	}
	return c
}

// retireReloadServer boots a server declaring an admin and a tenant principal
// (no legacy secret), wires the token tool as boot does, and mints one admin
// token. It returns the server and an admin-tool executor.
func retireReloadServer(t *testing.T) (*Server, func(in string) tools.Result) {
	t.Helper()
	return retireReloadServerWith(t, map[string]config.PrincipalDef{"ops": raOps, "alice": raAli})
}

// retireReloadServerWith is retireReloadServer declaring only principals.
func retireReloadServerWith(t *testing.T, principals map[string]config.PrincipalDef) (*Server, func(in string) tools.Result) {
	t.Helper()
	s, st := tokenAuthServer(t, "")
	cfg := raConfig(s.cfg(), principals)
	hasher, err := cfg.Env.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	tool := &builtin.OperatorTokenDef{
		Store:                  st,
		Hasher:                 hasher,
		Audit:                  audit.NopSink{},
		LegacyTokenSet:         cfg.Env.AuthToken != "",
		AdminPrincipalDeclared: s.AdminPrincipalResolved,
	}
	ctx := tools.WithOperatorTokenDefPolicy(context.Background(), tools.OperatorTokenDefPolicyValue{Admin: true})
	exec := func(in string) tools.Result {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatalf("Execute(%s): %v", in, err)
		}
		return res
	}
	if res := exec(`{"op":"create","name":"root","tenant_id":"default","subject":"root","scopes":["substrate:admin"]}`); res.IsError {
		t.Fatalf("mint admin token: %s", res.Text)
	}
	return s, exec
}

func reloadTo(t *testing.T, s *Server, principals map[string]config.PrincipalDef) error {
	t.Helper()
	return reloadToDry(t, s, principals, false)
}

func reloadToDry(t *testing.T, s *Server, principals map[string]config.PrincipalDef, dry bool) error {
	t.Helper()
	candidate := raConfig(&config.Config{}, principals)
	candidate.Env = s.cfg().Env
	return reloadCandidate(s, candidate, dry)
}

func reloadCandidate(s *Server, candidate *config.Config, dry bool) error {
	s.SetConfigReloader(func() (*config.Config, error) { return candidate, nil })
	_, err := s.ReloadConfig(context.Background(), dry)
	return err
}

// Whether a config holds an admin credential is decided by the config alone
// (RFC ES C1). An admin token in the store does not let a reload drop the admin
// principal: another replica, whose config still has its admin principal, may
// retire that token a moment later, leaving nobody able to administer.
func TestReloadDroppingAdminPrincipal_RefusedEvenWithAnAdminToken(t *testing.T) {
	s, _ := retireReloadServer(t)
	for _, dry := range []bool{true, false} {
		err := reloadToDry(t, s, map[string]config.PrincipalDef{"alice": raAli}, dry)
		if !errors.Is(err, config.ErrReloadInvalid) || !strings.Contains(err.Error(), "LOOMCYCLE_AUTH_TOKEN") ||
			!strings.Contains(err.Error(), "restart") {
			t.Fatalf("reload (dry=%v) dropping the admin principal with an admin token present: err = %v, "+
				"want ErrReloadInvalid naming a restart and LOOMCYCLE_AUTH_TOKEN", dry, err)
		}
	}
	if !s.AdminPrincipalResolved() {
		t.Error("the refused reload still dropped the admin principal")
	}
}

// A reload that keeps the admin principal may drop other principals.
func TestReloadKeepingAdminPrincipal_Allowed(t *testing.T) {
	s, _ := retireReloadServer(t)
	if err := reloadTo(t, s, map[string]config.PrincipalDef{"ops": raOps}); err != nil {
		t.Fatalf("reload keeping the admin principal: %v", err)
	}
	if !s.AdminPrincipalResolved() {
		t.Error("the admin principal did not survive a reload that kept it")
	}
}

// LOOMCYCLE_AUTH_TOKEN is an admin credential in config, so with it set a
// reload may drop the admin principal.
func TestReloadDroppingAdminPrincipal_AllowedWithLegacySecret(t *testing.T) {
	s, _ := retireReloadServer(t)
	candidate := raConfig(&config.Config{}, map[string]config.PrincipalDef{"alice": raAli})
	candidate.Env = s.cfg().Env
	candidate.Env.AuthToken = "legacy"
	if err := reloadCandidate(s, candidate, false); err != nil {
		t.Fatalf("reload dropping the admin principal while setting LOOMCYCLE_AUTH_TOKEN: %v", err)
	}
}

// Whether a config authenticates is decided by the config alone. An admin token
// in the store does not let a reload drop every principal: the token can be
// retired a moment later — from another replica whose config still has an admin
// principal, or inside this reload's own check-to-swap window — and nothing
// would notice this replica had gone open.
func TestReloadDroppingEveryPrincipal_RefusedEvenWithAnAdminToken(t *testing.T) {
	s, _ := retireReloadServer(t)
	err := reloadTo(t, s, nil)
	if !errors.Is(err, config.ErrReloadInvalid) || !strings.Contains(err.Error(), "LOOMCYCLE_AUTH_TOKEN") {
		t.Fatalf("reload dropping every principal with an admin token present: err = %v, want ErrReloadInvalid naming the way out", err)
	}
}

// A retire landing between the reload's checks and its swap cannot open the
// server: the reload is refused on the config alone, before that window.
func TestReload_RetireInsideTheReloadWindowCannotOpenServer(t *testing.T) {
	s, exec := retireReloadServer(t)
	candidate := &config.Config{}
	candidate.Env = s.cfg().Env
	s.SetConfigReloader(func() (*config.Config, error) { return candidate, nil },
		SectionReloader{Sections: []string{"principals"}, Reload: func(*config.Config) error {
			exec(`{"op":"retire","name":"root"}`)
			return nil
		}})
	_, _ = s.ReloadConfig(context.Background(), false)
	if on, err := s.AuthConfiguredErr(context.Background()); err != nil || !on {
		t.Fatalf("after a retire inside a reload dropping every principal: authConfigured = (%v, %v), want on", on, err)
	}
}

// Two replicas sharing one store, nothing concurrent: A reloads its principals
// away while an admin token exists, then B — whose config keeps its admin
// principal — retires that token. A must not be left open.
func TestReload_CrossReplicaRetireCannotOpenServer(t *testing.T) {
	b, exec := retireReloadServer(t)
	cfgA := &config.Config{Principals: b.cfg().Principals, ResolvedPrincipals: b.cfg().ResolvedPrincipals}
	cfgA.Env = b.cfg().Env
	a := &Server{cfgHolder: config.NewHolder(cfgA), store: b.store}
	_ = reloadTo(t, a, nil)
	if res := exec(`{"op":"retire","name":"root"}`); res.IsError {
		t.Fatalf("replica B retire (its admin principal is live): %s", res.Text)
	}
	if on, err := a.AuthConfiguredErr(context.Background()); err != nil || !on {
		t.Fatalf("replica A after B retired the last admin token: authConfigured = (%v, %v), want on", on, err)
	}
}

// Two replicas sharing one store, nothing concurrent: A reloads its admin
// principal away (keeping a tenant one) while an admin token exists, then B —
// whose config keeps its admin principal — retires that token. A must not be
// left with no admin credential: its reload is refused.
func TestReload_CrossReplicaRetireCannotLeaveNoAdmin(t *testing.T) {
	b, exec := retireReloadServer(t)
	cfgA := &config.Config{Principals: b.cfg().Principals, ResolvedPrincipals: b.cfg().ResolvedPrincipals}
	cfgA.Env = b.cfg().Env
	a := &Server{cfgHolder: config.NewHolder(cfgA), store: b.store}
	if err := reloadTo(t, a, map[string]config.PrincipalDef{"alice": raAli}); !errors.Is(err, config.ErrReloadInvalid) {
		t.Errorf("replica A reload dropping its admin principal while an admin token exists: err = %v, want ErrReloadInvalid", err)
	}
	if res := exec(`{"op":"retire","name":"root"}`); res.IsError {
		t.Fatalf("replica B retire (its admin principal is live): %s", res.Text)
	}
	if !a.AdminPrincipalResolved() {
		t.Fatal("replica A has no admin credential after B retired the last admin token")
	}
}

// Retire first: the admin principal keeps an admin credential, so the retire is
// allowed; the reload dropping that principal must then be refused.
func TestReloadDroppingAdminPrincipal_RefusedAfterLastAdminTokenRetired(t *testing.T) {
	s, exec := retireReloadServer(t)
	if res := exec(`{"op":"retire","name":"root"}`); res.IsError {
		t.Fatalf("retire with an admin config principal: %s", res.Text)
	}
	if err := reloadTo(t, s, map[string]config.PrincipalDef{"alice": raAli}); !errors.Is(err, config.ErrReloadInvalid) {
		t.Fatalf("reload dropping the only admin credential: err = %v, want ErrReloadInvalid", err)
	}
	if !s.AdminPrincipalResolved() {
		t.Error("the refused reload still dropped the admin principal")
	}
}

// The retire guard reads the admin principal from the CURRENT config: once a
// reload adds one, the last admin token may go — the principal is the admin
// credential left behind.
func TestRetireLastAdminToken_AllowedAfterReloadAddsAdminPrincipal(t *testing.T) {
	s, exec := retireReloadServerWith(t, map[string]config.PrincipalDef{"alice": raAli})
	if res := exec(`{"op":"retire","name":"root"}`); !res.IsError {
		t.Fatal("retired the last admin token with no admin credential in config; want refused")
	}
	if err := reloadTo(t, s, map[string]config.PrincipalDef{"ops": raOps, "alice": raAli}); err != nil {
		t.Fatalf("reload adding an admin principal: %v", err)
	}
	if res := exec(`{"op":"retire","name":"root"}`); res.IsError {
		t.Fatalf("retire after a reload added an admin principal: %s", res.Text)
	}
}

