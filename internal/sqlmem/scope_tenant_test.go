package sqlmem

import "testing"

// ScopeTenant is the ONE mapping from a runtime tenant to the tenant a SQL
// Memory scope key uses. It had four implementations across three packages, each
// comment naming a different one as authoritative and nothing asserting they
// agreed — they did, byte for byte, which is the only reason it was a latent risk
// rather than a live bug.
//
// The consequence of divergence is documented at the erasure call site and is not
// cosmetic: a DropScope built from a raw "" tenant matches nothing, so a
// single-tenant deployment's subject erasure leaves the whole SQL Memory database
// in place while reporting success. So the rule is pinned here, including the two
// cases that matter — the empty tenant becoming the default key, and a tenant
// LITERALLY NAMED "default" being indistinguishable from it.
func TestScopeTenant_PinsTheMapping(t *testing.T) {
	for _, tc := range []struct{ in, want, why string }{
		{"", "default", "an empty runtime tenant is what pgScopeNames refuses, so it MUST become the default key"},
		{"acme", "acme", "a named tenant passes through untouched"},
		{"default", "default", "a tenant literally named default is indistinguishable from the empty one — collapsing them is the accepted cost of using a magic name"},
		{" ", " ", "NOT trimmed: a whitespace tenant is a distinct (if pathological) tenant, and silently folding it into default would merge two keyspaces"},
	} {
		if got := ScopeTenant(tc.in); got != tc.want {
			t.Errorf("ScopeTenant(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
		}
	}
}

// The mapping's whole purpose is producing a tenant pgScopeNames accepts. If it
// ever returned "" the derivation would refuse, and every SQL Memory op in a
// single-tenant deployment would fail — so assert the two are consistent rather
// than trusting that they stay so.
func TestScopeTenant_ProducesAKeyTheDerivationAccepts(t *testing.T) {
	for _, in := range []string{"", "acme", "default"} {
		key := ScopeKey{Tenant: ScopeTenant(in), Scope: "user", ScopeID: "alice"}
		schema, role, err := pgScopeNames(key)
		if err != nil {
			t.Errorf("pgScopeNames rejected the key ScopeTenant(%q) built: %v", in, err)
			continue
		}
		if !pgIdentRe.MatchString(schema) || !pgIdentRe.MatchString(role) {
			t.Errorf("ScopeTenant(%q) produced non-conforming identifiers %q/%q", in, schema, role)
		}
	}
	// And distinct tenants must still derive distinct schemas — the mapping must
	// not collapse anything except the empty case it exists for.
	a, _, _ := pgScopeNames(ScopeKey{Tenant: ScopeTenant(""), Scope: "user", ScopeID: "alice"})
	b, _, _ := pgScopeNames(ScopeKey{Tenant: ScopeTenant("acme"), Scope: "user", ScopeID: "alice"})
	if a == b {
		t.Errorf("the empty tenant and %q derived the SAME schema %q — that is a cross-tenant leak", "acme", a)
	}
}
