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

// TestHasScope_ProvidersOperatorKey locks RFC AX's scope semantics: the new
// providers:operator-key scope is tenant-implied, so substrate:admin AND
// substrate:tenant satisfy it, an explicit grant satisfies it, and a narrow
// runs-only token does NOT. It is also in the closed catalog (grantable).
func TestHasScope_ProvidersOperatorKey(t *testing.T) {
	if !ValidScope(ScopeProvidersOperatorKey) {
		t.Fatal("providers:operator-key must be a valid catalog scope (grantable)")
	}
	if !HasScope([]string{ScopeAdmin}, ScopeProvidersOperatorKey) {
		t.Error("substrate:admin should satisfy providers:operator-key (superuser)")
	}
	if !HasScope([]string{ScopeTenant}, ScopeProvidersOperatorKey) {
		t.Error("substrate:tenant should satisfy providers:operator-key (tenant-implied)")
	}
	if !HasScope([]string{ScopeProvidersOperatorKey}, ScopeProvidersOperatorKey) {
		t.Error("an explicit providers:operator-key grant should satisfy itself")
	}
	if HasScope([]string{ScopeRunsCreate}, ScopeProvidersOperatorKey) {
		t.Error("runs:create must NOT satisfy providers:operator-key")
	}
}

// TestOperatorKeyRestricted_Matrix pins the RFC AX helper: restricted only when
// the gate is on AND a real (non-legacy, non-admin, un-scoped) principal is
// present AND lacks the scope. Every other combination is fail-OPEN (false =
// operator key allowed) — the deliberate backward-safety posture.
func TestOperatorKeyRestricted_Matrix(t *testing.T) {
	granular := Principal{TenantID: "acme", Subject: "bot", Scopes: []string{ScopeRunsCreate, ScopeRunsRead}}
	tenantOp := Principal{TenantID: "acme", Subject: "op", Scopes: []string{ScopeTenant}}
	adminP := Principal{TenantID: "", Subject: "root", Scopes: []string{ScopeAdmin}}
	legacyP := Principal{TenantID: "default", Subject: "default", Legacy: true}
	explicitP := Principal{TenantID: "acme", Subject: "bot", Scopes: []string{ScopeRunsCreate, ScopeProvidersOperatorKey}}

	cases := []struct {
		name   string
		p      Principal
		ok     bool
		gateOn bool
		want   bool
	}{
		// The ONLY restricted case: gate on + present + granular w/o the scope.
		{"gate_on_granular_restricted", granular, true, true, true},
		// Gate off ⇒ byte-identical old behavior for every token.
		{"gate_off_granular", granular, true, false, false},
		// Open mode (no principal) never restricts.
		{"open_mode", Principal{}, false, true, false},
		// Legacy LOOMCYCLE_AUTH_TOKEN is never restricted.
		{"legacy", legacyP, true, true, false},
		// tenant-implied ⇒ substrate:tenant passes (RFC AX documented trade-off).
		{"tenant_operator", tenantOp, true, true, false},
		// Admin is covered because HasScope treats substrate:admin as universal.
		{"admin", adminP, true, true, false},
		// An explicit grant passes even when the gate is on.
		{"explicit_grant", explicitP, true, true, false},
	}
	for _, c := range cases {
		if got := OperatorKeyRestricted(c.p, c.ok, c.gateOn); got != c.want {
			t.Errorf("%s: OperatorKeyRestricted = %v, want %v", c.name, got, c.want)
		}
	}
}
