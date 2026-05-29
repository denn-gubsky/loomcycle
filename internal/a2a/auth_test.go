package a2a

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// TestPrincipalFromContext_AuthenticatedUserThreadsUserAndTenant
// verifies an authenticated CallContext User maps to UserID, and the
// explicit request tenant wins over the CallContext tenant.
func TestPrincipalFromContext_AuthenticatedUserThreadsUserAndTenant(t *testing.T) {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = a2asrv.NewAuthenticatedUser("alice", nil)

	p := principalFromContext(ctx, "tenant-explicit")
	if !p.Authenticated {
		t.Fatal("expected authenticated principal")
	}
	if p.UserID != "alice" {
		t.Errorf("user = %q, want alice", p.UserID)
	}
	if p.TenantID != "tenant-explicit" {
		t.Errorf("tenant = %q, want the explicit request tenant", p.TenantID)
	}
}

// TestPrincipalFromContext_AnonymousIsUnauthenticated verifies the
// default-deny shape: no CallContext → unauthenticated, no user.
func TestPrincipalFromContext_AnonymousIsUnauthenticated(t *testing.T) {
	p := principalFromContext(context.Background(), "")
	if p.Authenticated {
		t.Error("expected unauthenticated principal with no CallContext")
	}
	if p.UserID != "" {
		t.Errorf("user = %q, want empty for anonymous", p.UserID)
	}
}

// TestPrincipalFromContext_UnauthenticatedUserDropsName ensures a User
// flagged Authenticated=false does not leak its Name into UserID —
// run attribution must reflect only authenticated identity.
func TestPrincipalFromContext_UnauthenticatedUserDropsName(t *testing.T) {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	callCtx.User = &a2asrv.User{Name: "spoofed", Authenticated: false}

	p := principalFromContext(ctx, "")
	if p.UserID != "" {
		t.Errorf("user = %q, want empty (unauthenticated)", p.UserID)
	}
}
