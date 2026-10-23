package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

func compiled(t *testing.T, tenant, subject string, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	a := authz.ACL{Name: "t", Rules: rules}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: tenant, ID: subject}, Bindings: []authz.Binding{{ACL: "t", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"t": {ACL: a}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// RFC EY: after the scope gate, the RPC's type-level ACL question. In compat
// mode an explicit deny holds; in enforce mode an unmatched RPC is refused
// and a missing set is Unavailable, never silently admitted.
func TestGrpcEnforceACL_PerMode(t *testing.T) {
	run := grpcMethodPrefix + "Run"
	deny := compiled(t, "acme", "alice", authz.Rule{Effect: authz.Deny, Actions: []string{"run"}, Resources: []string{"agent:*"}})
	allowSteer := compiled(t, "acme", "alice", authz.Rule{Effect: authz.Allow, Actions: []string{"steer"}, Resources: []string{"run:*"}})
	principal := func(eff *authz.Effective) auth.Principal {
		return auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}, Effective: eff}
	}
	ctxFor := func(mode authz.Mode, p auth.Principal) context.Context {
		return auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), mode), "grpc"), p)
	}
	if err := enforceScope(ctxFor(authz.ModeCompat, principal(deny)), run); status.Code(err) != codes.PermissionDenied {
		t.Errorf("compat + deny: %v, want PermissionDenied", err)
	}
	if err := enforceScope(ctxFor(authz.ModeCompat, principal(nil)), run); err != nil {
		t.Errorf("compat + no set: %v, want nil", err)
	}
	if err := enforceScope(ctxFor(authz.ModeShadow, principal(deny)), run); err != nil {
		t.Errorf("shadow + deny: %v, want nil (legacy decides)", err)
	}
	if err := enforceScope(ctxFor(authz.ModeEnforce, principal(allowSteer)), run); status.Code(err) != codes.PermissionDenied {
		t.Errorf("enforce + no matching rule: %v, want PermissionDenied", err)
	}
	if err := enforceScope(ctxFor(authz.ModeEnforce, principal(allowSteer)), grpcMethodPrefix+"RunInput"); err != nil {
		t.Errorf("enforce + allow: %v, want nil", err)
	}
	if err := enforceScope(ctxFor(authz.ModeEnforce, principal(nil)), run); status.Code(err) != codes.Unavailable {
		t.Errorf("enforce + no set: %v, want Unavailable", err)
	}
	// An ungoverned RPC is untouched by any mode.
	if err := enforceScope(ctxFor(authz.ModeEnforce, principal(nil)), grpcMethodPrefix+"Config"); err != nil {
		t.Errorf("enforce + Config: %v, want nil", err)
	}
	// No principal (open mode) is untouched.
	if err := enforceScope(authz.WithMode(context.Background(), authz.ModeEnforce), run); err != nil {
		t.Errorf("open mode: %v", err)
	}
}
