package auth

import "testing"

// metrics:read is in the catalog but implies nothing, and only the
// superuser's scope satisfies it.
func TestScopeMetricsRead_ImpliesNothingAndOnlyAdminSatisfiesIt(t *testing.T) {
	if !ValidScope(ScopeMetricsRead) {
		t.Fatal("metrics:read is not in the catalog")
	}
	for _, want := range []string{ScopeTenant, ScopeUser, ScopeRunsCreate, ScopeRunsRead, ScopeChannelRead, ScopeLLMGateway, ScopeRunsDelete, ScopeAuthManage} {
		if HasScope([]string{ScopeMetricsRead}, want) {
			t.Errorf("metrics:read satisfies %q", want)
		}
	}
	if HasScope([]string{ScopeTenant}, ScopeMetricsRead) || HasScope([]string{ScopeUser}, ScopeMetricsRead) {
		t.Error("a tenant or user scope satisfies metrics:read")
	}
}

func TestValidateScopeSet_RefusesMetricsReadWithAnyOtherScope(t *testing.T) {
	if err := ValidateScopeSet([]string{ScopeMetricsRead}); err != nil {
		t.Fatalf("alone: %v", err)
	}
	for _, other := range []string{ScopeAdmin, ScopeTenant, ScopeRunsRead, ScopeAuthManage} {
		if err := ValidateScopeSet([]string{ScopeMetricsRead, other}); err == nil {
			t.Errorf("metrics:read + %s accepted", other)
		}
	}
}

// A principal holding metrics:read is a metrics reader whatever else it holds
// (the restrictive reading of a set that slipped past ValidateScopeSet); the
// legacy secret never is.
func TestIsMetricsReader_HoldsForAnySetContainingTheScope(t *testing.T) {
	cases := []struct {
		p    Principal
		want bool
	}{
		{Principal{Scopes: []string{ScopeMetricsRead}}, true},
		{Principal{Scopes: []string{ScopeAdmin, ScopeMetricsRead}}, true},
		{Principal{Scopes: []string{ScopeAdmin}}, false},
		{Principal{Scopes: nil}, false},
		{Principal{Legacy: true, Scopes: []string{ScopeAdmin}}, false},
	}
	for _, c := range cases {
		if got := IsMetricsReader(c.p); got != c.want {
			t.Errorf("IsMetricsReader(%v) = %v, want %v", c.p.Scopes, got, c.want)
		}
	}
}
