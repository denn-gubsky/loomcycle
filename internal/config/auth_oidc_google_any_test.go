package config

import (
	"strings"
	"testing"
)

// allow_any_account opens the google preset to every Google account, personal
// ones included (no hd claim). A stranger's first sign-in may then make at
// most an account that waits for approval, or none (provision: known).
func TestResolveAuth_GoogleAllowAnyAccount(t *testing.T) {
	pending := &AuthIdentityMapConfig{Tenant: "acme", Provision: ProvisionAuto, NewUser: &AuthNewUserConfig{Status: UserStatusPending}}
	c := oidcConfig(t, "", AuthGateConfig{Preset: "google", AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}, IdentityMap: pending})
	if err := resolveAuth(c); err != nil {
		t.Fatalf("any account, pending: %v", err)
	}
	o := c.ResolvedAuth.Gates[0].OIDC
	if !o.AnyAccount || len(o.AllowedDomains) != 0 || o.Issuer != GoogleIssuer {
		t.Fatalf("oidc = %+v", o)
	}

	known := &AuthIdentityMapConfig{Tenant: "acme", Provision: ProvisionKnown}
	if err := resolveAuth(oidcConfig(t, "", AuthGateConfig{Preset: "google", AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}, IdentityMap: known})); err != nil {
		t.Errorf("any account, provision known: %v", err)
	}

	// The deployment floor may be the one holding new users.
	c = oidcConfig(t, "", AuthGateConfig{Preset: "google", AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true},
		IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", Provision: ProvisionAuto}})
	c.Auth.Lifecycle.NewUser.Status = UserStatusPending
	if err := resolveAuth(c); err != nil {
		t.Errorf("any account, pending floor: %v", err)
	}

	// Refused: auto-provisioned active users from any Google account.
	for name, idm := range map[string]*AuthIdentityMapConfig{
		"auto, default status": {Tenant: "acme", Provision: ProvisionAuto},
		"auto, active":         {Tenant: "acme", Provision: ProvisionAuto, NewUser: &AuthNewUserConfig{Status: UserStatusActive}},
	} {
		err := resolveAuth(oidcConfig(t, "", AuthGateConfig{Preset: "google", AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}, IdentityMap: idm}))
		if err == nil || !strings.Contains(err.Error(), "allow_any_account") {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Refused: both, or on another preset or kind.
	both := AuthGateConfig{Preset: "google", AllowedDomains: []string{"acme.com"},
		AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}, IdentityMap: known}
	if err := resolveAuth(oidcConfig(t, "", both)); err == nil || !strings.Contains(err.Error(), "exclude each other") {
		t.Errorf("allow_any_account with allowed_domains: %v", err)
	}
	generic := AuthGateConfig{Preset: "generic", Issuer: "https://idp.example",
		AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}, IdentityMap: known}
	if err := resolveAuth(oidcConfig(t, "", generic)); err == nil || !strings.Contains(err.Error(), "allow_any_account") {
		t.Errorf("allow_any_account on preset generic: %v", err)
	}
	c = &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token",
		AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "allow_any_account") {
		t.Errorf("allow_any_account on a token gate: %v", err)
	}

	// Without either, the google preset still refuses to load.
	err := resolveAuth(oidcConfig(t, "", AuthGateConfig{Preset: "google", IdentityMap: known}))
	if err == nil || !strings.Contains(err.Error(), "allowed_domains") {
		t.Errorf("google with neither: %v", err)
	}
}

// Google reached through another preset would skip the google preset's rule
// and admit every Google account: refused.
func TestResolveAuth_GoogleOnlyThroughItsPreset(t *testing.T) {
	for name, g := range map[string]AuthGateConfig{
		"generic issuer":       {Preset: "generic", Issuer: "https://accounts.google.com"},
		"generic, upper case":  {Preset: "generic", Issuer: "https://ACCOUNTS.google.com"},
		"okta issuer":          {Preset: "okta", Issuer: "https://accounts.google.com"},
		"keycloak issuer":      {Preset: "keycloak", Issuer: "https://accounts.google.com/"},
		"generic discovery":    {Preset: "generic", Issuer: "https://idp.example", DiscoveryURL: "https://accounts.google.com/.well-known/openid-configuration"},
		"preset omitted (gen)": {Issuer: "https://accounts.google.com"},
	} {
		err := resolveAuth(oidcConfig(t, "", g))
		if err == nil || !strings.Contains(err.Error(), "preset: google") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Another provider on a similar name is not Google.
	if err := resolveAuth(oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://accounts.google.com.evil.example"})); err != nil {
		t.Errorf("a look-alike host was refused as Google: %v", err)
	}
}
