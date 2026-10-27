package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func edJWK(t *testing.T) map[string]any {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub)}
}

func externalCfg(g AuthGateConfig) *Config {
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"tg": g}}}
	c.Env.PublicURL = "https://lc.example"
	return c
}

func TestResolveExternalGate_LoadsAKeyAnIdentityMapAndTheTokenCeiling(t *testing.T) {
	g := AuthGateConfig{Kind: "external", IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", Provision: "auto"}}
	g.External.PublicKey = edJWK(t)
	g.External.IssuedToken = &AuthIssuedTokenConfig{TTL: "1h", Agents: []string{"chat"}, MaxActive: 3}
	g.External.OnRemoved = &AuthOnRemovedConfig{UnbindAfter: "3d"}
	c := externalCfg(g)
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	got := c.ResolvedAuth.Gates[0]
	if got.Kind != AuthGateKindExternal || got.Identity == nil || got.Identity.Tenant != "acme" || got.External == nil {
		t.Fatalf("gate = %+v", got)
	}
	x := got.External
	if x.Key.Algorithm() != "EdDSA" || x.TokenTTL != time.Hour || x.MaxActive != 3 || len(x.TokenRestrictions.Agents) != 1 ||
		x.Removal.UnbindAfter != 72*time.Hour || x.Removal.EraseAfter != 0 {
		t.Fatalf("external = %+v", x)
	}
	// No callback_url: an assert-only gate, not on the login page.
	if x.Interactive() || len(c.ResolvedAuth.LoginGates()) != 1 /* the implicit token gate */ {
		t.Fatalf("login gates = %+v", c.ResolvedAuth.LoginGates())
	}
}

// Each external-gate key refuses what it cannot honour, at load.
func TestResolveExternalGate_RefusesWhatItCannotHonour(t *testing.T) {
	good := func() AuthGateConfig {
		g := AuthGateConfig{Kind: "external", IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"}}
		g.External.PublicKey = edJWK(t)
		return g
	}
	for name, tc := range map[string]struct {
		mut  func(*AuthGateConfig, *Config)
		want string
	}{
		"no public url": {func(g *AuthGateConfig, c *Config) { c.Env.PublicURL = "" }, "LOOMCYCLE_PUBLIC_URL"},
		"no key":        {func(g *AuthGateConfig, c *Config) { g.External.PublicKey = nil }, "required"},
		"private key": {func(g *AuthGateConfig, c *Config) {
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			g.External.PublicKey["d"] = base64.RawURLEncoding.EncodeToString(priv.Seed())
		}, "private"},
		"literal hmac secret": {func(g *AuthGateConfig, c *Config) {
			g.External.PublicKey = nil
			g.External.HMACSecret = strings.Repeat("s", 40)
		}, "hmac_secret_env"},
		// A verification secret in the credential store would be one a
		// tenant could set: refused (coordinator rule after the P3 review).
		"cred hmac secret": {func(g *AuthGateConfig, c *Config) {
			g.External.PublicKey = nil
			g.External.HMACSecret = "$cred:gate-secret"
		}, "not accepted"},
		"both keys": {func(g *AuthGateConfig, c *Config) { g.External.HMACSecretEnv = "LOOMCYCLE_TEST_GATE_SECRET" }, "not both"},
		"empty secret env": {func(g *AuthGateConfig, c *Config) {
			g.External.PublicKey = nil
			g.External.HMACSecretEnv = "LOOMCYCLE_TEST_GATE_UNSET"
		}, "is empty"},
		"non-loomcycle env": {func(g *AuthGateConfig, c *Config) {
			g.External.PublicKey = nil
			g.External.HMACSecretEnv = "HOME"
		}, "LOOMCYCLE_"},
		"no identity map":    {func(g *AuthGateConfig, c *Config) { g.IdentityMap = nil }, "identity_map"},
		"http callback":      {func(g *AuthGateConfig, c *Config) { g.External.CallbackURL = "http://gate.example/login" }, "https"},
		"return_to no login": {func(g *AuthGateConfig, c *Config) { g.AllowedReturnTo = []string{"https://app.example/"} }, "callback_url"},
		"password keys":      {func(g *AuthGateConfig, c *Config) { g.Invites.MaxTTL = "1h" }, "do not apply"},
		"token ttl too long": {func(g *AuthGateConfig, c *Config) { g.External.IssuedToken = &AuthIssuedTokenConfig{TTL: "90d"} }, "issued_token.ttl"},
		"reactivate auto":    {func(g *AuthGateConfig, c *Config) { g.External.OnRemoved = &AuthOnRemovedConfig{Reactivate: "auto"} }, "not supported yet"},
		"untrusted promotes": {func(g *AuthGateConfig, c *Config) {
			g.IdentityMap.NewUser = &AuthNewUserConfig{AccessMode: "tenant"}
		}, "trusted"},
	} {
		t.Run(name, func(t *testing.T) {
			g := good()
			c := externalCfg(g)
			tc.mut(&g, c)
			c.Auth.Gates["tg"] = g
			err := resolveAuth(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	// The external keys belong to kind external only.
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token"}}}}
	g := c.Auth.Gates["paste"]
	g.External.CallbackURL = "https://gate.example/"
	c.Auth.Gates["paste"] = g
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "apply only to kind: external") {
		t.Fatalf("external keys on a token gate: %v", err)
	}
}

// A co-located gate's HMAC secret comes from the operator's env, and the
// name is denied from ${...} expansion like a principal's bearer.
func TestResolveExternalGate_HMACSecretComesFromTheOperatorsEnv(t *testing.T) {
	t.Setenv("LOOMCYCLE_TEST_GATE_HMAC", strings.Repeat("k", 40))
	g := AuthGateConfig{Kind: "external", IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"}}
	g.External.HMACSecretEnv = "LOOMCYCLE_TEST_GATE_HMAC"
	c := externalCfg(g)
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	if x := c.ResolvedAuth.Gates[0].External; x.Key.Algorithm() != "HS256" || x.HMACSecretEnv != "LOOMCYCLE_TEST_GATE_HMAC" {
		t.Fatalf("external = %+v", x)
	}
	if !envNameDenied("LOOMCYCLE_TEST_GATE_HMAC") {
		t.Fatal("the gate secret's env var stays interpolatable")
	}
	t.Setenv("LOOMCYCLE_TEST_GATE_HMAC", "short")
	if err := resolveAuth(externalCfg(g)); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("a short secret: %v", err)
	}
}

// The keys read at the gate's own level, as the RFC writes them.
func TestResolveExternalGate_DecodesFromYAML(t *testing.T) {
	var a AuthConfig
	src := `
gates:
  tg:
    kind: external
    public_key: { kty: OKP, crv: Ed25519, x: "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo" }
    callback_url: https://gate.example/login
    allowed_return_to: [https://app.example/welcome]
    issued_token: { ttl: 2h, agents: [chat], overrides: [interactive], rate: { per_minute: 6, burst: 10 }, max_active: 2 }
    on_removed: { unbind_after: 72h }
    identity_map: { tenant: acme, provision: auto }
`
	if err := yaml.Unmarshal([]byte(src), &a); err != nil {
		t.Fatal(err)
	}
	c := &Config{Auth: a}
	c.Env.PublicURL = "https://lc.example"
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	x := c.ResolvedAuth.Gates[0].External
	if x.CallbackURL != "https://gate.example/login" || x.TokenTTL != 2*time.Hour || x.TokenRestrictions.Rate.PerMinute != 6 ||
		len(x.AllowedReturnTo) != 1 || x.Removal.UnbindAfter != 72*time.Hour {
		t.Fatalf("external = %+v", x)
	}
}

// Strict mode counts an external gate as a way in only when a browser can
// sign in through it (it has a callback_url).
func TestResolveExternalGate_StrictCountsOnlyABrowserGate(t *testing.T) {
	g := AuthGateConfig{Kind: "external", IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"}}
	g.External.PublicKey = edJWK(t)
	c := externalCfg(g)
	c.Auth.Mode = "strict"
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("strict with only an assert-only gate: %v", err)
	}
	g.External.CallbackURL = "https://gate.example/login"
	c = externalCfg(g)
	c.Auth.Mode = "strict"
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	if lg := c.ResolvedAuth.LoginGates(); len(lg) != 1 || lg[0].Name != "tg" {
		t.Fatalf("login gates = %+v", lg)
	}
	if d := c.ResolvedAuth.Gates[0].External.DiscoveryURL; d != "https://gate.example" {
		t.Fatalf("discovery default = %q", d)
	}
}
