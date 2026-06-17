package auth

import "testing"

// TestHasScope_TenantImplication locks RFC AF's scope semantics:
// substrate:tenant satisfies the tenant-confined scopes (runs / channels / the
// def-hook-MCP gate `substrate:tenant`) but NOT substrate:admin — so a tenant
// operator passes the def/hook/MCP route gate yet is refused the operator-plane
// routes (minting, runtime admin). substrate:admin stays the superuser.
func TestHasScope_TenantImplication(t *testing.T) {
	tenant := []string{ScopeTenant}
	// A tenant operator satisfies every WITHIN-tenant scope.
	for _, want := range []string{ScopeTenant, ScopeRunsCreate, ScopeRunsRead, ScopeChannelPublish, ScopeChannelRead} {
		if !HasScope(tenant, want) {
			t.Errorf("substrate:tenant should satisfy %q", want)
		}
	}
	// But NOT the operator plane — this is the whole point of RFC AF.
	if HasScope(tenant, ScopeAdmin) {
		t.Fatal("substrate:tenant must NOT satisfy substrate:admin (would re-grant superuser)")
	}

	// substrate:admin remains the superuser — satisfies everything incl. tenant.
	admin := []string{ScopeAdmin}
	for _, want := range []string{ScopeAdmin, ScopeTenant, ScopeRunsCreate, ScopeChannelRead} {
		if !HasScope(admin, want) {
			t.Errorf("substrate:admin should satisfy %q", want)
		}
	}

	// A narrow runs-only token satisfies neither the tenant gate nor admin.
	narrow := []string{ScopeRunsCreate}
	if HasScope(narrow, ScopeTenant) || HasScope(narrow, ScopeAdmin) {
		t.Error("runs:create must not satisfy substrate:tenant or substrate:admin")
	}
}

// TestValidScope_Tenant: substrate:tenant is in the closed catalog (so
// operator-token create/rotate accept it).
func TestValidScope_Tenant(t *testing.T) {
	if !ValidScope(ScopeTenant) {
		t.Error("substrate:tenant must be a valid catalog scope")
	}
	if bad := UnknownScopes([]string{ScopeTenant, ScopeRunsCreate}); bad != nil {
		t.Errorf("UnknownScopes flagged valid scopes: %v", bad)
	}
}
