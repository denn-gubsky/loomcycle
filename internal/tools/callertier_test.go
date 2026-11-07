package tools

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Whose runs a caller reads (auth.OwnRowsOnly) and who decides for the tenant
// (TierForPrincipal) are one split of the same principals: the operator plane
// reads every run of its tenant, a member or an isolated member reads only its
// own. A principal that confined one way and not the other would read
// colleagues' conversations it may not decide about, or the reverse.
func TestOwnRowsOnly_MatchesTheMemberTiers(t *testing.T) {
	member, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		p    auth.Principal
		ok   bool
	}{
		{"open mode", auth.Principal{}, false},
		{"legacy", auth.Principal{Legacy: true, TenantID: "default", Subject: "op"}, true},
		{"admin", auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, true},
		{"tenant operator", auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}, true},
		{"tenant operator holding substrate:user too", auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant, auth.ScopeUser}}, true},
		{"member", auth.Principal{TenantID: "acme", Subject: "bob", Scopes: member}, true},
		{"isolated member", auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser}}, true},
		{"read-only token", auth.Principal{TenantID: "acme", Subject: "reader", Scopes: []string{auth.ScopeRunsRead}}, true},
	} {
		tier := TierForPrincipal(c.p, c.ok, c.p.TenantID)
		memberTier := tier == TierMember || tier == TierIsolated
		if got := auth.OwnRowsOnly(c.p, c.ok); got != memberTier {
			t.Errorf("%s: OwnRowsOnly = %v, but its tier in its own tenant is %s", c.name, got, tier)
		}
	}
}
