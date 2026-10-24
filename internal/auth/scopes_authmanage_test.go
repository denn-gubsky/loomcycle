package auth

import "testing"

// auth:manage (RFC FA D16) is in the catalog but implies nothing, and no
// other scope implies it except the superuser's.
func TestScopeAuthManage_ImpliesNothingAndOnlyAdminSatisfiesIt(t *testing.T) {
	if !ValidScope(ScopeAuthManage) {
		t.Fatal("auth:manage is not in the catalog")
	}
	for _, want := range []string{ScopeTenant, ScopeUser, ScopeRunsCreate, ScopeRunsRead, ScopeChannelRead, ScopeLLMGateway, ScopeRunsDelete} {
		if HasScope([]string{ScopeAuthManage}, want) {
			t.Errorf("auth:manage satisfies %q", want)
		}
	}
	if HasScope([]string{ScopeTenant}, ScopeAuthManage) || HasScope([]string{ScopeUser}, ScopeAuthManage) {
		t.Error("a tenant or user scope satisfies auth:manage")
	}
}

func TestValidateScopeSet_RefusesAuthManageWithAnyOtherScope(t *testing.T) {
	if err := ValidateScopeSet([]string{ScopeAuthManage}); err != nil {
		t.Fatalf("alone: %v", err)
	}
	for _, other := range []string{ScopeAdmin, ScopeTenant, ScopeRunsCreate} {
		if err := ValidateScopeSet([]string{ScopeAuthManage, other}); err == nil {
			t.Errorf("auth:manage + %s accepted", other)
		}
	}
	if err := ValidateScopeSet([]string{ScopeTenant, ScopeRunsCreate}); err != nil {
		t.Fatalf("a set without auth:manage: %v", err)
	}
}

// A principal holding auth:manage is a manager whatever else it holds (the
// restrictive reading of a set that slipped past ValidateScopeSet); the legacy
// secret never is.
func TestIsAuthManager_HoldsForAnySetContainingTheScope(t *testing.T) {
	cases := []struct {
		p    Principal
		want bool
	}{
		{Principal{Scopes: []string{ScopeAuthManage}}, true},
		{Principal{Scopes: []string{ScopeAdmin, ScopeAuthManage}}, true},
		{Principal{Scopes: []string{ScopeAdmin}}, false},
		{Principal{Scopes: nil}, false},
		{Principal{Legacy: true, Scopes: []string{ScopeAdmin}}, false},
	}
	for _, c := range cases {
		if got := IsAuthManager(c.p); got != c.want {
			t.Errorf("IsAuthManager(%v) = %v, want %v", c.p.Scopes, got, c.want)
		}
	}
}
