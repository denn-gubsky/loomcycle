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

// TestPrincipalFromContext_RoutedTenantOverridesRequestTenant is the
// trust-boundary test: the host/path-derived routed tenant must win
// over a tenant the peer supplied in the message body, so a body field
// cannot mislabel the run's tenant.
func TestPrincipalFromContext_RoutedTenantOverridesRequestTenant(t *testing.T) {
	ctx := WithRoutedTenant(context.Background(), "tenant-routed")
	p := principalFromContext(ctx, "tenant-from-body")
	if p.TenantID != "tenant-routed" {
		t.Errorf("tenant = %q, want tenant-routed (routed wins over body)", p.TenantID)
	}
}

// An active routing mode that resolves an EMPTY tenant is still
// authoritative: it must suppress the peer-supplied body tenant so a peer
// cannot mislabel its run by stuffing a body field on a non-tenant host /
// un-prefixed route. Only the absence of any routing decision
// (single-tenant mode, context never stamped) permits the body fallback.
func TestWithRoutedTenant_EmptyIsAuthoritative(t *testing.T) {
	ctx := WithRoutedTenant(context.Background(), "")
	if got, ok := RoutedTenantFrom(ctx); !ok || got != "" {
		t.Errorf("RoutedTenantFrom = (%q, %v), want (\"\", true) — empty must be authoritative", got, ok)
	}
	if p := principalFromContext(ctx, "tenant-from-body"); p.TenantID != "" {
		t.Errorf("tenant = %q, want \"\" (body tenant suppressed in a routing mode)", p.TenantID)
	}
}

// With NO routing decision on the context (single-tenant / none mode),
// the body-supplied tenant is the only signal and is used for attribution.
func TestPrincipalFromContext_BodyTenantOnlyWhenNoRoutingDecision(t *testing.T) {
	p := principalFromContext(context.Background(), "tenant-from-body")
	if p.TenantID != "tenant-from-body" {
		t.Errorf("tenant = %q, want tenant-from-body fallback in single-tenant mode", p.TenantID)
	}
}
