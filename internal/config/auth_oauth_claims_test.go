package config

import (
	"strings"
	"testing"
)

// An OAuth provider's login, name and username are the user's to rename: an
// identity map keyed on them would let a rename collide with someone else's
// account or tenant. Only the immutable id may be the subject, only a
// verified e-mail domain may pick the tenant, and groups come only from the
// gate's own org-filtered list.
func TestResolveAuth_OAuthIdentityMapRefusesMutableClaims(t *testing.T) {
	for name, m := range map[string]*AuthIdentityMapConfig{
		"subject login":    {Tenant: "acme", Subject: "login"},
		"subject name":     {Tenant: "acme", Subject: "name"},
		"subject username": {Tenant: "acme", Subject: "username"},
		"subject email":    {Tenant: "acme", Subject: "email"},
		"tenant login":     {TenantClaim: "login", AllowedTenants: map[string]string{"alice": "acme"}},
		"groups name":      {Tenant: "acme", Groups: &AuthIdentityGroupsConfig{Claim: "name"}},
	} {
		c := oauthGateConfig(t, AuthGateConfig{Preset: "github", Trusted: true, IdentityMap: m})
		if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "identity_map") {
			t.Fatalf("%s: err = %v, want an identity_map refusal", name, err)
		}
	}
	for name, m := range map[string]*AuthIdentityMapConfig{
		"default subject":     {Tenant: "acme"},
		"sub subject":         {Tenant: "acme", Subject: "sub"},
		"email domain tenant": {TenantClaim: EmailDomainClaim, AllowedTenants: map[string]string{"acme.example": "acme"}},
		"gate's own groups":   {Tenant: "acme", Groups: &AuthIdentityGroupsConfig{}},
	} {
		c := oauthGateConfig(t, AuthGateConfig{Preset: "github", Trusted: true, IdentityMap: m})
		if err := resolveAuth(c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
