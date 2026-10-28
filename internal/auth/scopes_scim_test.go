package auth

import "testing"

// scim:provision (RFC FA P6) implies nothing, is implied by nothing but the
// superuser's scope, and is minted alone.
func TestScopeSCIMProvision_ImpliesNothingAndIsMintedAlone(t *testing.T) {
	if !ValidScope(ScopeSCIMProvision) {
		t.Fatal("scim:provision is not in the catalog")
	}
	for _, want := range []string{ScopeTenant, ScopeUser, ScopeRunsCreate, ScopeRunsRead, ScopeAuthManage, ScopeLLMGateway} {
		if HasScope([]string{ScopeSCIMProvision}, want) {
			t.Errorf("scim:provision satisfies %q", want)
		}
	}
	if HasScope([]string{ScopeTenant}, ScopeSCIMProvision) {
		t.Error("a tenant scope satisfies scim:provision")
	}
	if err := ValidateScopeSet([]string{ScopeSCIMProvision}); err != nil {
		t.Fatalf("alone: %v", err)
	}
	for _, other := range []string{ScopeAdmin, ScopeTenant, ScopeRunsCreate, ScopeAuthManage} {
		if err := ValidateScopeSet([]string{ScopeSCIMProvision, other}); err == nil {
			t.Errorf("scim:provision + %s accepted", other)
		}
	}
	if !IsSCIMProvisioner(Principal{Scopes: []string{ScopeAdmin, ScopeSCIMProvision}}) || IsSCIMProvisioner(Principal{Legacy: true, Scopes: []string{ScopeAdmin}}) {
		t.Error("IsSCIMProvisioner must hold for any set containing the scope, never for the legacy secret")
	}
}
