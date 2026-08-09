package auth

import (
	"reflect"
	"testing"
)

// The grant derivation is the security core of RFC BX P2c delegated minting:
// a member's scopes come from its access_mode, and NEVER include an operator/
// admin scope. This locks the mapping and the anti-escalation property.
func TestGrantableUserScopes(t *testing.T) {
	tenant, err := GrantableUserScopes("tenant")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	wantTenant := []string{ScopeRunsCreate, ScopeRunsRead, ScopeChannelPublish, ScopeChannelRead}
	if !reflect.DeepEqual(tenant, wantTenant) {
		t.Errorf("tenant grant = %v, want %v", tenant, wantTenant)
	}

	iso, err := GrantableUserScopes("isolated")
	if err != nil {
		t.Fatalf("isolated: %v", err)
	}
	if !reflect.DeepEqual(iso, []string{ScopeUser}) {
		t.Errorf("isolated grant = %v, want [%s]", iso, ScopeUser)
	}

	// Unknown access_mode → (nil, error): a store invariant break must NOT mint a
	// default (unbounded) grant.
	got, err := GrantableUserScopes("wat")
	if err == nil || got != nil {
		t.Errorf("unknown mode → (%v, %v), want (nil, error)", got, err)
	}

	// Anti-escalation: no derived grant may carry an operator/admin/tenant scope,
	// and every derived scope must be in the closed catalog (an unenforceable
	// name would be a false grant).
	for _, mode := range []string{"tenant", "isolated"} {
		scopes, _ := GrantableUserScopes(mode)
		for _, s := range scopes {
			if s == ScopeAdmin || s == ScopeTenant || s == ScopeProvidersOperatorKey {
				t.Errorf("%s grant leaked privileged scope %q", mode, s)
			}
		}
		if bad := UnknownScopes(scopes); bad != nil {
			t.Errorf("%s grant has non-catalog scope(s) %v", mode, bad)
		}
	}
}
