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
// F2). Both the guard and a config reload must see the CURRENT principals:
// a reload can drop the admin principal, and a token retire can drop the last
// admin token, in either order, and neither may leave the server with no admin
// credential — or, with no principal left, in open mode.

var (
	raOps = config.PrincipalDef{Subject: "ops", Scopes: []string{auth.ScopeAdmin}, TokenEnv: "LOOMCYCLE_OPS_TOKEN"}
	raAli = config.PrincipalDef{Tenant: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_ALICE_TOKEN"}
)

// retireReloadServer boots a server declaring an admin and a tenant principal
// (no legacy secret), wires the token tool as boot does, and mints one admin
// token. It returns the server and an admin-tool executor.
func retireReloadServer(t *testing.T) (*Server, func(in string) tools.Result) {
	t.Helper()
	s, st := tokenAuthServer(t, "")
	cfg := s.cfg()
	cfg.Principals = map[string]config.PrincipalDef{"ops": raOps, "alice": raAli}
	cfg.ResolvedPrincipals = []auth.DeclaredPrincipal{
		{Secret: "lct_ra_ops", Principal: auth.Principal{Subject: "ops", Scopes: raOps.Scopes, TokenDefID: "cfg:ops"}},
		{Secret: "lct_ra_alice", Principal: auth.Principal{TenantID: "acme", Subject: "alice", Scopes: raAli.Scopes, TokenDefID: "cfg:alice"}},
	}
	hasher, err := cfg.Env.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	tool := &builtin.OperatorTokenDef{
		Store:                  st,
		Hasher:                 hasher,
		Audit:                  audit.NopSink{},
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
	// As a real reload loads it: each kept principal with its resolved secret.
	candidate := &config.Config{Principals: principals}
	for _, dp := range s.cfg().ResolvedPrincipals {
		if _, kept := principals[dp.Principal.TokenDefID[len("cfg:"):]]; kept {
			candidate.ResolvedPrincipals = append(candidate.ResolvedPrincipals, dp)
		}
	}
	candidate.Env = s.cfg().Env
	s.SetConfigReloader(func() (*config.Config, error) { return candidate, nil })
	_, err := s.ReloadConfig(context.Background(), false)
	return err
}

// Reload first: the admin token keeps an admin credential, so dropping the admin
// principal (keeping a tenant one) is allowed; the retire after it must then be
// refused, or nobody could administer.
func TestRetireLastAdminToken_RefusedAfterReloadDropsAdminPrincipal(t *testing.T) {
	s, exec := retireReloadServer(t)
	if err := reloadTo(t, s, map[string]config.PrincipalDef{"alice": raAli}); err != nil {
		t.Fatalf("reload dropping the admin principal while an admin token exists: %v", err)
	}
	if res := exec(`{"op":"retire","name":"root"}`); !res.IsError {
		t.Fatal("retired the last admin token after a reload dropped the admin principal; want refused")
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
