package http

import (
	"context"
	"encoding/json"
	"errors"
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
// principal is allowed; the retire after it must then be refused.
func TestRetireLastAdminToken_RefusedAfterReloadDropsAdminPrincipal(t *testing.T) {
	for name, keep := range map[string]map[string]config.PrincipalDef{
		"tenant principal kept": {"alice": raAli}, // retire would leave no admin
		"no principal kept":     nil,              // retire would leave open mode
	} {
		t.Run(name, func(t *testing.T) {
			s, exec := retireReloadServer(t)
			if err := reloadTo(t, s, keep); err != nil {
				t.Fatalf("reload dropping the admin principal while an admin token exists: %v", err)
			}
			if res := exec(`{"op":"retire","name":"root"}`); !res.IsError {
				t.Fatal("retired the last admin token after a reload dropped the admin principal; want refused")
			}
		})
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
