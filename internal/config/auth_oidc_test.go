package config

import (
	"strings"
	"testing"
)

const oidcSecretEnv = "LOOMCYCLE_TEST_GATE_OIDC_SECRET"

func oidcConfig(t *testing.T, mode string, g AuthGateConfig) *Config {
	t.Helper()
	t.Setenv(oidcSecretEnv, "s3cret-value")
	g.Kind = AuthGateKindOIDC
	if g.ClientID == "" {
		g.ClientID = "client-1"
	}
	if g.ClientSecretEnv == "" && g.ClientSecret == "" {
		g.ClientSecretEnv = oidcSecretEnv
	}
	if g.IdentityMap == nil {
		g.IdentityMap = &AuthIdentityMapConfig{Tenant: "acme"}
	}
	c := &Config{Auth: AuthConfig{Mode: mode, Gates: map[string]AuthGateConfig{"sso": g}}}
	c.Env.PublicURL = "https://lc.example"
	return c
}

func TestResolveAuth_OIDCGateDefaults(t *testing.T) {
	c := oidcConfig(t, "strict", AuthGateConfig{Issuer: "https://idp.example/realms/acme", Preset: "keycloak"})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	g := c.ResolvedAuth.Gates[0]
	o := g.OIDC
	if o == nil || g.Identity == nil || g.Identity.Tenant != "acme" {
		t.Fatalf("gate = %+v", g)
	}
	if o.ClientSecret != "s3cret-value" || o.RedirectURI != "https://lc.example/v1/_auth/complete/sso" ||
		o.DiscoveryURL != "https://idp.example/realms/acme/.well-known/openid-configuration" ||
		strings.Join(o.Scopes, " ") != "openid email profile" {
		t.Fatalf("oidc = %+v", o)
	}
	// Strict mode accepts an OIDC gate as its only interactive gate.
	if !c.ResolvedAuth.Strict() {
		t.Fatal("not strict")
	}
}

// RFC FA §8 credential at rest: the config refuses a literal secret.
func TestResolveAuth_OIDCRefusesALiteralSecret(t *testing.T) {
	for _, v := range []string{"literal-secret", "$cred:google-oidc"} {
		c := oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://idp.example", ClientSecret: v})
		if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "client_secret") {
			t.Errorf("client_secret %q: %v", v, err)
		}
	}
	c := oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://idp.example", ClientSecretEnv: "HOME"})
	if err := resolveAuth(c); err == nil {
		t.Error("a non-LOOMCYCLE env name was accepted")
	}
	t.Setenv("LOOMCYCLE_TEST_EMPTY_SECRET", "")
	c = oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://idp.example", ClientSecretEnv: "LOOMCYCLE_TEST_EMPTY_SECRET"})
	if err := resolveAuth(c); err == nil {
		t.Error("an empty secret env was accepted")
	}
}

// One refusal per key that would otherwise load without effect, or let in
// more than asked (no fail-open keys).
func TestResolveAuth_OIDCRefusesBadOrMisplacedKeys(t *testing.T) {
	cases := map[string]AuthGateConfig{
		"no client_id":                 {Preset: "generic", Issuer: "https://idp.example", ClientID: " "},
		"unknown preset":               {Preset: "ping", Issuer: "https://idp.example"},
		"generic without issuer":       {Preset: "generic"},
		"http issuer":                  {Preset: "generic", Issuer: "http://idp.example"},
		"google without domains":       {Preset: "google"},
		"google with foreign issuer":   {Preset: "google", Issuer: "https://evil.example", AllowedDomains: []string{"acme.com"}},
		"allowed_domains off google":   {Preset: "generic", Issuer: "https://idp.example", AllowedDomains: []string{"acme.com"}},
		"entra without tids":           {Preset: "entra"},
		"entra bad tid":                {Preset: "entra", AllowedTIDs: []string{"acme"}},
		"allowed_tids off entra":       {Preset: "okta", Issuer: "https://acme.okta.com", AllowedTIDs: []string{"11111111-1111-1111-1111-111111111111"}},
		"cognito bad issuer":           {Preset: "cognito", Issuer: "https://idp.example"},
		"discovery_url off generic":    {Preset: "okta", Issuer: "https://acme.okta.com", DiscoveryURL: "https://x.example/d"},
		"bad token_auth":               {Preset: "generic", Issuer: "https://idp.example", TokenAuth: "jwt"},
		"bad scope":                    {Preset: "generic", Issuer: "https://idp.example", Scopes: []string{"a b"}},
		"password key on oidc":         {Preset: "generic", Issuer: "https://idp.example", NewUser: AuthGateNewUserConfig{AccessMode: "tenant"}},
		"foreign allowed_return_to":    {Preset: "generic", Issuer: "https://idp.example", AllowedReturnTo: []string{"http://evil.example/"}},
		"group sync on untrusted gate": {Preset: "generic", Issuer: "https://idp.example", IdentityMap: &AuthIdentityMapConfig{Tenant: "a", Groups: &AuthIdentityGroupsConfig{Claim: "groups"}}},
	}
	for name, g := range cases {
		if err := resolveAuth(oidcConfig(t, "", g)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	c := oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://idp.example"})
	c.Env.PublicURL = ""
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_PUBLIC_URL") {
		t.Errorf("no public URL: %v", err)
	}
	// An OIDC gate needs an identity map (no implicit tenant).
	c = oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://idp.example"})
	g := c.Auth.Gates["sso"]
	g.IdentityMap = nil
	c.Auth.Gates["sso"] = g
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "identity_map") {
		t.Errorf("no identity map: %v", err)
	}
	// The OIDC keys are refused on other kinds.
	c = &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token", ClientID: "x"}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "only to kind: oidc") {
		t.Errorf("client_id on a token gate: %v", err)
	}
}

func TestResolveAuth_OIDCPresets(t *testing.T) {
	const t1, t2 = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	cases := map[string]struct {
		g              AuthGateConfig
		issuer, disco  string
		issuerTemplate bool
	}{
		"google": {AuthGateConfig{Preset: "google", AllowedDomains: []string{"ACME.com"}}, GoogleIssuer,
			GoogleIssuer + "/.well-known/openid-configuration", false},
		"entra single": {AuthGateConfig{Preset: "entra", AllowedTIDs: []string{t1}}, EntraAuthority + "/" + t1 + "/v2.0",
			EntraAuthority + "/" + t1 + "/v2.0/.well-known/openid-configuration", false},
		"entra multi": {AuthGateConfig{Preset: "entra", AllowedTIDs: []string{t1, t2}}, EntraAuthority + "/{tenantid}/v2.0",
			EntraAuthority + "/organizations/v2.0/.well-known/openid-configuration", true},
		"cognito": {AuthGateConfig{Preset: "cognito", Issuer: "https://cognito-idp.eu-west-1.amazonaws.com/eu-west-1_AbC123"},
			"https://cognito-idp.eu-west-1.amazonaws.com/eu-west-1_AbC123", "https://cognito-idp.eu-west-1.amazonaws.com/eu-west-1_AbC123/.well-known/openid-configuration", false},
		"auth0 trailing slash": {AuthGateConfig{Preset: "auth0", Issuer: "https://acme.eu.auth0.com/"}, "https://acme.eu.auth0.com/",
			"https://acme.eu.auth0.com/.well-known/openid-configuration", false},
		"identity-center": {AuthGateConfig{Preset: "identity-center", Issuer: "https://identitycenter.example/ssoins-1"}, "https://identitycenter.example/ssoins-1",
			"https://identitycenter.example/ssoins-1/.well-known/openid-configuration", false},
		"generic loopback with discovery": {AuthGateConfig{Preset: "generic", Issuer: "http://127.0.0.1:9000", DiscoveryURL: "http://127.0.0.1:9000/disco"},
			"http://127.0.0.1:9000", "http://127.0.0.1:9000/disco", false},
	}
	for name, tc := range cases {
		c := oidcConfig(t, "", tc.g)
		if err := resolveAuth(c); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		o := c.ResolvedAuth.Gates[0].OIDC
		if o.Issuer != tc.issuer || o.DiscoveryURL != tc.disco || o.IssuerTemplate != tc.issuerTemplate {
			t.Errorf("%s: %+v", name, o)
		}
	}
}
