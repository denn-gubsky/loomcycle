package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The three tenant-resolution helpers disagree on what an admin gets when it
// names no tenant, and that disagreement is deliberate: a LIST read can mean
// "every tenant" while a single-tuple read must name exactly one. Nothing
// asserted it, so the difference read as an accident — and on the memory browse
// routes it WAS one, leaving a super-admin unable to see data its own tenant
// operator could.
//
// This pins every cell. A change to one helper now has to state whether it meant
// to change the family.
func TestTenantResolution_AdminDefaultsAreDeliberate(t *testing.T) {
	s := &Server{}
	adminCtx := auth.WithPrincipal(t.Context(), auth.Principal{
		TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin},
	})
	opCtx := auth.WithPrincipal(t.Context(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})

	t.Run("list read: admin naming none sees every tenant", func(t *testing.T) {
		tenant, all := s.principalTenantScope(adminCtx, "")
		if !all || tenant != "" {
			t.Errorf("principalTenantScope(admin, \"\") = (%q, %v), want (\"\", true) — a list read is where \"all tenants\" is expressible", tenant, all)
		}
	})
	t.Run("single-tuple read: admin naming none keeps its own", func(t *testing.T) {
		if got := s.adminFocusedTenant(adminCtx, ""); got != "acme" {
			t.Errorf("adminFocusedTenant(admin, \"\") = %q, want acme — widening the default here would move behaviour for every existing caller instead of adding a capability", got)
		}
	})
	t.Run("both honour an explicit admin focus", func(t *testing.T) {
		if tenant, all := s.principalTenantScope(adminCtx, "other"); tenant != "other" || all {
			t.Errorf("principalTenantScope(admin, other) = (%q, %v), want (other, false)", tenant, all)
		}
		if got := s.adminFocusedTenant(adminCtx, "other"); got != "other" {
			t.Errorf("adminFocusedTenant(admin, other) = %q, want other", got)
		}
	})
	t.Run("neither lets a tenant operator widen", func(t *testing.T) {
		if tenant, all := s.principalTenantScope(opCtx, "other"); tenant != "acme" || all {
			t.Errorf("principalTenantScope(operator, other) = (%q, %v), want (acme, false) — a tenant must not widen its own scope", tenant, all)
		}
		if got := s.adminFocusedTenant(opCtx, "other"); got != "acme" {
			t.Errorf("adminFocusedTenant(operator, other) = %q, want acme — a tenant must not widen its own scope", got)
		}
	})
	t.Run("browse ctx: operator keeps its tenant, admin takes the focus", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x?tenant=other&scope_id=carol", nil)
		if got := tenantOfBrowseCtx(s, opCtx, req); got != "acme" {
			t.Errorf("substrateBrowseCtxFn(operator) tenant = %q, want acme", got)
		}
		if got := tenantOfBrowseCtx(s, adminCtx, req); got != "other" {
			t.Errorf("substrateBrowseCtxFn(admin) tenant = %q, want other", got)
		}
	})
}

// tenantOfBrowseCtx runs substrateBrowseCtxFn and reads back the tenant it
// stamped, so the browse path can be asserted next to its two siblings.
func tenantOfBrowseCtx(s *Server, ctx context.Context, r *http.Request) string {
	return tools.RunIdentity(s.substrateBrowseCtxFn(r)(ctx)).TenantID
}
