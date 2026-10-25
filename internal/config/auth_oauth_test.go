package config

import (
	"strings"
	"testing"
)

func oauthGateConfig(t *testing.T, g AuthGateConfig) *Config {
	t.Helper()
	t.Setenv("LOOMCYCLE_GATE_GH_SECRET", "s3cret-value")
	g.Kind = AuthGateKindOAuth
	if g.ClientID == "" {
		g.ClientID = "cid"
	}
	if g.ClientSecretEnv == "" && g.ClientSecret == "" {
		g.ClientSecretEnv = "LOOMCYCLE_GATE_GH_SECRET"
	}
	if g.IdentityMap == nil {
		g.IdentityMap = &AuthIdentityMapConfig{Tenant: "acme"}
	}
	// Tests that aren't about who may sign in opt into open sign-in on the
	// public instances, which the loader otherwise refuses.
	if g.BaseURL == "" && len(g.AllowedOrgs) == 0 && !g.RequireOrg {
		g.AllowAnyAccount = true
	}
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"gh": g}}}
	c.Env.PublicURL = "https://lc.example"
	return c
}

func TestResolveAuth_OAuthGateIsAnInteractiveIdentityGate(t *testing.T) {
	c := oauthGateConfig(t, AuthGateConfig{Preset: "github", AuthGateOAuthConfig: AuthGateOAuthConfig{AllowedOrgs: []string{"acme"}, RequireOrg: true}})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	g := c.ResolvedAuth.Gates[0]
	if g.OAuth == nil || g.Identity == nil || g.OAuth.ClientSecret != "s3cret-value" ||
		g.OAuth.RedirectURI != "https://lc.example/v1/_auth/complete/gh" || !g.OAuth.RequireOrg {
		t.Fatalf("gate = %+v / %+v", g, g.OAuth)
	}
	if !g.OAuth.TrustEmail {
		t.Fatal("github.com: trust_email should default on")
	}
	// A self-managed instance does not trust e-mail unless told to.
	c = oauthGateConfig(t, AuthGateConfig{Preset: "gitlab", AuthGateOAuthConfig: AuthGateOAuthConfig{BaseURL: "https://gitlab.acme.example"}})
	if err := resolveAuth(c); err != nil || c.ResolvedAuth.Gates[0].OAuth.TrustEmail {
		t.Fatalf("self-managed gitlab: trust_email = %v, %v", c.ResolvedAuth.Gates[0].OAuth, err)
	}
	// Strict mode accepts it as the one interactive gate.
	c = oauthGateConfig(t, AuthGateConfig{Preset: "gitlab"})
	c.Auth.Mode = AuthModeStrict
	if err := resolveAuth(c); err != nil {
		t.Fatalf("strict with only an OAuth gate: %v", err)
	}
}

func TestResolveAuth_OAuthGateRefusesWhatItCannotHonour(t *testing.T) {
	for name, tc := range map[string]struct {
		g    AuthGateConfig
		want string
	}{
		"literal secret":         {AuthGateConfig{Preset: "github", ClientSecret: "x", ClientSecretEnv: ""}, "literal"},
		"no identity map":        {AuthGateConfig{Preset: "github", IdentityMap: nil}, ""},
		"unknown preset":         {AuthGateConfig{Preset: "okta"}, "preset"},
		"gitea without base":     {AuthGateConfig{Preset: "gitea"}, "base_url"},
		"plain http base":        {AuthGateConfig{Preset: "gitlab", AuthGateOAuthConfig: AuthGateOAuthConfig{BaseURL: "http://gitlab.example"}}, "https"},
		"generic, no urls":       {AuthGateConfig{Preset: "generic"}, "auth_url"},
		"require_org alone":      {AuthGateConfig{Preset: "github", AuthGateOAuthConfig: AuthGateOAuthConfig{RequireOrg: true}}, "allowed_orgs"},
		"oidc-only key":          {AuthGateConfig{Preset: "github", Issuer: "https://x.example"}, "kind: oidc"},
		"generic groups":         {AuthGateConfig{Preset: "generic", AuthGateOAuthConfig: AuthGateOAuthConfig{AuthURL: "https://s/a", TokenURL: "https://s/t", UserinfoURL: "https://s/u", AllowedOrgs: []string{"x"}}}, "claims.groups"},
		"generic keys on github": {AuthGateConfig{Preset: "github", AuthGateOAuthConfig: AuthGateOAuthConfig{UserinfoURL: "https://s/u"}}, "preset generic"},
	} {
		c := oauthGateConfig(t, tc.g)
		if name == "no identity map" {
			gg := c.Auth.Gates["gh"]
			gg.IdentityMap = nil
			c.Auth.Gates["gh"] = gg
			tc.want = "identity_map"
		}
		err := resolveAuth(c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
	// OAuth-only keys on another kind are refused.
	c := passwordGateConfig("", AuthGateConfig{AuthGateOAuthConfig: AuthGateOAuthConfig{AllowedOrgs: []string{"acme"}}})
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "only to kind: oauth") {
		t.Fatalf("allowed_orgs on a password gate: %v", err)
	}
}

// On public github.com or gitlab.com anyone has an account, so a gate there
// must confine sign-in to allowed_orgs with require_org, or opt in to open
// sign-in explicitly. A self-managed instance controls its own accounts.
func TestResolveAuth_PublicOAuthPresetsRefuseOpenSignInUnlessChosen(t *testing.T) {
	for _, preset := range []string{"github", "gitlab"} {
		for name, oc := range map[string]AuthGateOAuthConfig{
			"no orgs":                 {},
			"orgs without require":    {AllowedOrgs: []string{"acme"}},
			"orgs, require, explicit": {AllowedOrgs: []string{"acme"}, RequireOrg: true},
		} {
			c := oauthGateConfig(t, AuthGateConfig{Preset: preset, AuthGateOAuthConfig: oc})
			gg := c.Auth.Gates["gh"]
			gg.AllowAnyAccount = false
			c.Auth.Gates["gh"] = gg
			err := resolveAuth(c)
			wantOK := name == "orgs, require, explicit"
			if wantOK != (err == nil) {
				t.Fatalf("%s %s: err = %v, want ok=%v", preset, name, err, wantOK)
			}
			if !wantOK && !strings.Contains(err.Error(), "allow_any_account") {
				t.Fatalf("%s %s: err = %v, want it to name allow_any_account", preset, name, err)
			}
		}
		c := oauthGateConfig(t, AuthGateConfig{Preset: preset, AuthGateOAuthConfig: AuthGateOAuthConfig{AllowAnyAccount: true}})
		if err := resolveAuth(c); err != nil {
			t.Fatalf("%s with allow_any_account: %v", preset, err)
		}
		c = oauthGateConfig(t, AuthGateConfig{Preset: preset, AuthGateOAuthConfig: AuthGateOAuthConfig{BaseURL: "https://git.acme.example"}})
		gg := c.Auth.Gates["gh"]
		gg.AllowAnyAccount = false
		c.Auth.Gates["gh"] = gg
		if err := resolveAuth(c); err != nil {
			t.Fatalf("%s self-managed: %v", preset, err)
		}
	}
}
