package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ P5: a gRPC call is evaluated under its principal's tenant's mode,
// the one the HTTP server answers for that tenant (Config.ACLModeFor), not
// the deployment's alone. Without the lookup every tenant gets ACLMode.
func TestGrpcAuthenticate_StampsThePrincipalsTenantMode(t *testing.T) {
	tenants := map[string]string{"acme-token": "acme", "beta-token": "beta"}
	s := &Server{
		authConfigured: func(context.Context) bool { return true },
		principalResolver: func(_ context.Context, bearer string) (auth.Principal, bool) {
			tenant, ok := tenants[bearer]
			return auth.Principal{TenantID: tenant, Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}, ok
		},
		aclMode: authz.ModeCompat,
		aclModeFor: func(tenant string) authz.Mode {
			return authz.TenantMode(authz.ModeCompat, map[string]authz.Mode{"acme": authz.ModeEnforce}, tenant)
		},
	}
	for bearer, want := range map[string]authz.Mode{"acme-token": authz.ModeEnforce, "beta-token": authz.ModeCompat} {
		ctx, err := s.authenticate(mdCtx(bearer))
		if err != nil {
			t.Fatalf("%s: %v", bearer, err)
		}
		if got := authz.ModeFrom(ctx); got != want {
			t.Errorf("%s: mode %s, want %s", bearer, got, want)
		}
	}
	s.aclModeFor = nil
	ctx, err := s.authenticate(mdCtx("acme-token"))
	if err != nil {
		t.Fatal(err)
	}
	if got := authz.ModeFrom(ctx); got != authz.ModeCompat {
		t.Errorf("no lookup wired: mode %s, want the deployment's compat", got)
	}
}
