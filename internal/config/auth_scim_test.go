package config

import (
	"strings"
	"testing"
)

func scimConfig(gates map[string]AuthGateConfig) *Config {
	return &Config{Auth: AuthConfig{Gates: gates}}
}

func TestResolveAuth_ASCIMGateTakesItsTenantFromTheToken(t *testing.T) {
	c := scimConfig(map[string]AuthGateConfig{"okta": {Kind: AuthGateKindSCIM, Trusted: true,
		IdentityMap: &AuthIdentityMapConfig{Groups: &AuthIdentityGroupsConfig{Provision: true}, AuthoritativeForUsers: true},
		External:    AuthExternalGateConfig{MaxSyncRemovals: "3"}}})
	if err := resolveAuth(c); err != nil {
		t.Fatalf("a SCIM gate: %v", err)
	}
	g, ok := c.ResolvedAuth.SCIMGate()
	if !ok || g.Identity == nil || g.SCIM == nil {
		t.Fatalf("SCIMGate = %+v %v", g, ok)
	}
	if g.Identity.Tenant != "" || g.Identity.Provision != ProvisionAuto || !g.Identity.SyncGroups || !g.Identity.AuthoritativeForUsers {
		t.Fatalf("identity map = %+v", g.Identity)
	}
	if g.SCIM.RemovalCeiling(1000) != 3 {
		t.Fatalf("ceiling = %d", g.SCIM.RemovalCeiling(1000))
	}
	for _, lg := range c.ResolvedAuth.LoginGates() {
		if lg.Kind == AuthGateKindSCIM {
			t.Fatal("a SCIM gate is offered as a sign-in")
		}
	}
	fixed := scimConfig(map[string]AuthGateConfig{"okta": {Kind: AuthGateKindSCIM, IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"}}})
	if err := resolveAuth(fixed); err != nil {
		t.Fatal(err)
	}
	if g, _ := fixed.ResolvedAuth.SCIMGate(); g.Identity.Tenant != "acme" {
		t.Fatalf("fixed tenant = %q", g.Identity.Tenant)
	}
}

func TestResolveAuth_ASCIMGateRefusesWhatDoesNotApply(t *testing.T) {
	for name, g := range map[string]AuthGateConfig{
		"tenant_claim":     {Kind: AuthGateKindSCIM, IdentityMap: &AuthIdentityMapConfig{TenantClaim: "org", AllowedTenants: map[string]string{"a": "a"}}},
		"subject claim":    {Kind: AuthGateKindSCIM, IdentityMap: &AuthIdentityMapConfig{Subject: "email"}},
		"provision known":  {Kind: AuthGateKindSCIM, IdentityMap: &AuthIdentityMapConfig{Provision: ProvisionKnown}},
		"untrusted groups": {Kind: AuthGateKindSCIM, IdentityMap: &AuthIdentityMapConfig{Groups: &AuthIdentityGroupsConfig{}}},
		"untrusted tenant floor": {Kind: AuthGateKindSCIM,
			IdentityMap: &AuthIdentityMapConfig{NewUser: &AuthNewUserConfig{AccessMode: "tenant"}}},
		"public key":   {Kind: AuthGateKindSCIM, External: AuthExternalGateConfig{CallbackURL: "https://idp.example"}},
		"password key": {Kind: AuthGateKindSCIM, RequireMFA: new(bool)},
		"bad ceiling":  {Kind: AuthGateKindSCIM, External: AuthExternalGateConfig{MaxSyncRemovals: "0"}},
	} {
		if err := resolveAuth(scimConfig(map[string]AuthGateConfig{"okta": g})); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	two := scimConfig(map[string]AuthGateConfig{"a": {Kind: AuthGateKindSCIM}, "b": {Kind: AuthGateKindSCIM}})
	if err := resolveAuth(two); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("two SCIM gates: %v", err)
	}
	// A tenant's sign-in policy cannot name a gate that signs nobody in.
	pol := scimConfig(map[string]AuthGateConfig{"okta": {Kind: AuthGateKindSCIM}, "paste": {Kind: AuthGateKindToken}})
	pol.Auth.TenantPolicies = map[string]AuthTenantPolicyConfig{"acme": {Gates: []string{"okta"}}}
	if err := resolveAuth(pol); err == nil {
		t.Error("a tenant policy named the SCIM gate")
	}
}
