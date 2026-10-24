package config

import (
	"net/netip"
	"strings"
	"testing"
)

func edgeConfig(mode string, gates map[string]AuthGateConfig) *Config {
	return &Config{Auth: AuthConfig{Mode: mode, Gates: gates}}
}

func cfGateConfig() AuthGateConfig {
	return AuthGateConfig{
		Kind: "header", Preset: "cloudflare-access", TeamDomain: "acme.cloudflareaccess.com", Audience: "aud-tag",
		TrustedProxyCIDRs: []string{"172.18.0.0/16"}, IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"},
	}
}

func albGateConfig() AuthGateConfig {
	return AuthGateConfig{
		Kind: "header", Preset: "aws-alb",
		ALBARN:            "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/my-alb/50dc6c495c0c9188",
		TrustedProxyCIDRs: []string{"10.0.0.0/16"}, IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"},
	}
}

func hmacGateConfig() AuthGateConfig {
	return AuthGateConfig{
		Kind: "header", Preset: "forward-auth-hmac", SecretEnv: "LOOMCYCLE_GATE_PROXY_SECRET",
		TrustedProxyCIDRs: []string{"127.0.0.1"}, IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"},
	}
}

func awsiamGateConfig() AuthGateConfig {
	return AuthGateConfig{
		Kind: "awsiam", ServerID: "lc.acme.internal", AllowedARNs: []string{"arn:aws:iam::123456789012:role/ci-*"},
		IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", Subject: "arn", Provision: "auto"},
	}
}

func mustResolve(t *testing.T, c *Config) AuthPolicy {
	t.Helper()
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	return c.ResolvedAuth
}

func mustRefuse(t *testing.T, g AuthGateConfig, want string) {
	t.Helper()
	err := resolveAuth(edgeConfig("", map[string]AuthGateConfig{"g": g}))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("resolveAuth = %v, want an error containing %q", err, want)
	}
}

func TestResolveAuth_HeaderPresetsResolve(t *testing.T) {
	p := mustResolve(t, edgeConfig("", map[string]AuthGateConfig{"cf": cfGateConfig(), "alb": albGateConfig(), "fwd": hmacGateConfig()}))
	byName := map[string]AuthGate{}
	for _, g := range p.Gates {
		byName[g.Name] = g
	}
	cf := byName["cf"].Header
	if cf == nil || cf.TeamDomain != "acme.cloudflareaccess.com" || cf.Audience != "aud-tag" ||
		cf.TrustedProxyCIDRs[0] != netip.MustParsePrefix("172.18.0.0/16") || byName["cf"].Identity.Tenant != "acme" {
		t.Fatalf("cloudflare gate %+v", byName["cf"])
	}
	if alb := byName["alb"].Header; alb.ALBRegion != "eu-west-1" {
		t.Fatalf("alb region %q", alb.ALBRegion)
	}
	if fwd := byName["fwd"].Header; fwd.SecretEnv != "LOOMCYCLE_GATE_PROXY_SECRET" ||
		fwd.TrustedProxyCIDRs[0] != netip.MustParsePrefix("127.0.0.1/32") {
		t.Fatalf("forward-auth gate %+v", fwd)
	}
}

func TestResolveAuth_HeaderGateRefusals(t *testing.T) {
	mod := func(base AuthGateConfig, f func(*AuthGateConfig)) AuthGateConfig { f(&base); return base }
	cases := []struct {
		name string
		g    AuthGateConfig
		want string
	}{
		{"unknown preset", mod(cfGateConfig(), func(g *AuthGateConfig) { g.Preset = "nginx" }), "unknown preset"},
		{"no preset", mod(cfGateConfig(), func(g *AuthGateConfig) { g.Preset = "" }), "preset is required"},
		{"no trusted_proxy_cidrs", mod(cfGateConfig(), func(g *AuthGateConfig) { g.TrustedProxyCIDRs = nil }), "trusted_proxy_cidrs is required"},
		{"trusts every address", mod(cfGateConfig(), func(g *AuthGateConfig) { g.TrustedProxyCIDRs = []string{"0.0.0.0/0"} }), "trusts every address"},
		{"bad cidr", mod(cfGateConfig(), func(g *AuthGateConfig) { g.TrustedProxyCIDRs = []string{"10.0.0.0/33"} }), "not a CIDR"},
		{"no identity_map", mod(cfGateConfig(), func(g *AuthGateConfig) { g.IdentityMap = nil }), "needs an identity_map"},
		{"team domain with a scheme", mod(cfGateConfig(), func(g *AuthGateConfig) { g.TeamDomain = "https://acme.cloudflareaccess.com" }), "team_domain"},
		{"no audience", mod(cfGateConfig(), func(g *AuthGateConfig) { g.Audience = "" }), "audience"},
		{"alb key on cloudflare", mod(cfGateConfig(), func(g *AuthGateConfig) { g.ALBARN = albGateConfig().ALBARN }), "does not apply to preset cloudflare-access"},
		{"no alb_arn", mod(albGateConfig(), func(g *AuthGateConfig) { g.ALBARN = "" }), "alb_arn"},
		{"network load balancer", mod(albGateConfig(), func(g *AuthGateConfig) {
			g.ALBARN = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/net/x/50dc6c495c0c9188"
		}), "alb_arn"},
		{"cloudflare key on alb", mod(albGateConfig(), func(g *AuthGateConfig) { g.TeamDomain = "a.b" }), "does not apply to preset aws-alb"},
		{"no secret", mod(hmacGateConfig(), func(g *AuthGateConfig) { g.SecretEnv = "" }), "needs the shared secret"},
		{"both secrets", mod(hmacGateConfig(), func(g *AuthGateConfig) { g.Secret = "$cred:proxy" }), "not both"},
		{"secret env outside the namespace", mod(hmacGateConfig(), func(g *AuthGateConfig) { g.SecretEnv = "HOME" }), "secret_env"},
		{"secret env is the admin bearer", mod(hmacGateConfig(), func(g *AuthGateConfig) { g.SecretEnv = "LOOMCYCLE_AUTH_TOKEN" }), "infrastructure secrets"},
		{"$cred with a claimed tenant", mod(hmacGateConfig(), func(g *AuthGateConfig) {
			g.SecretEnv, g.Secret = "", "$cred:proxy"
			g.IdentityMap = &AuthIdentityMapConfig{TenantClaim: "tenant", AllowedTenants: map[string]string{"acme": "acme"}}
		}), "fixed tenant"},
		{"awsiam key on a header gate", mod(cfGateConfig(), func(g *AuthGateConfig) { g.ServerID = "x" }), "apply only to kind: awsiam"},
		{"password key on a header gate", mod(cfGateConfig(), func(g *AuthGateConfig) { b := true; g.RequireMFA = &b }), "do not apply to kind: header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustRefuse(t, tc.g, tc.want) })
	}
}

func TestResolveAuth_LiteralSecretIsRefusedAndNeverQuoted(t *testing.T) {
	g := hmacGateConfig()
	g.SecretEnv, g.Secret = "", "hunter2-hunter2-hunter2-hunter2-hunter2"
	err := resolveAuth(edgeConfig("", map[string]AuthGateConfig{"g": g}))
	if err == nil || !strings.Contains(err.Error(), "$cred:") {
		t.Fatalf("literal secret: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("the refusal quotes the secret")
	}
}

func TestResolveAuth_StoredSecretNeedsAFixedTenant(t *testing.T) {
	g := hmacGateConfig()
	g.SecretEnv, g.Secret = "", "$cred:proxy-hmac"
	p := mustResolve(t, edgeConfig("", map[string]AuthGateConfig{"g": g}))
	if p.Gates[0].Header.SecretCred != "proxy-hmac" {
		t.Fatalf("secret cred %q", p.Gates[0].Header.SecretCred)
	}
}

func TestResolveAuth_AWSIAMGateResolvesAndDefaultsServerIDToThePublicHost(t *testing.T) {
	g := awsiamGateConfig()
	g.ServerID = ""
	c := edgeConfig("", map[string]AuthGateConfig{"ci": g})
	c.Env.PublicURL = "https://loomcycle.acme.com/ui"
	p := mustResolve(t, c)
	a := p.Gates[0].AWSIAM
	if a == nil || a.ServerID != "loomcycle.acme.com" || len(a.AllowedARNs) != 1 || p.Gates[0].Identity.SubjectClaim != "arn" {
		t.Fatalf("awsiam gate %+v / %+v", a, p.Gates[0].Identity)
	}
}

func TestResolveAuth_AWSIAMGateRefusals(t *testing.T) {
	mod := func(f func(*AuthGateConfig)) AuthGateConfig { g := awsiamGateConfig(); f(&g); return g }
	cases := []struct {
		name string
		g    AuthGateConfig
		want string
	}{
		{"no server id and no public url", mod(func(g *AuthGateConfig) { g.ServerID = "" }), "server_id"},
		{"no allowed_arns", mod(func(g *AuthGateConfig) { g.AllowedARNs = nil }), "allowed_arns"},
		{"any account", mod(func(g *AuthGateConfig) { g.AllowedARNs = []string{"arn:aws:iam::*:role/ci-*"} }), "literal 12-digit account"},
		{"bare star", mod(func(g *AuthGateConfig) { g.AllowedARNs = []string{"*"} }), "literal 12-digit account"},
		{"assumed-role pattern", mod(func(g *AuthGateConfig) {
			g.AllowedARNs = []string{"arn:aws:sts::123456789012:assumed-role/ci/*"}
		}), "name the role"},
		{"bad region", mod(func(g *AuthGateConfig) { g.STSRegions = []string{"mars-1"} }), "not an AWS region"},
		{"header key", mod(func(g *AuthGateConfig) { g.TrustedProxyCIDRs = []string{"10.0.0.0/8"} }), "apply only to kind: header"},
		{"no identity_map", mod(func(g *AuthGateConfig) { g.IdentityMap = nil }), "needs an identity_map"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustRefuse(t, tc.g, tc.want) })
	}
}

func TestResolveAuth_EdgeKeysOnOtherKindsAreRefused(t *testing.T) {
	// preset, issuer and client_id are shared with kind: oidc, so a token
	// gate that sets one is refused (naming the OIDC gate's keys).
	err := resolveAuth(edgeConfig("", map[string]AuthGateConfig{"paste": {Kind: "token", Preset: "cloudflare-access"}}))
	if err == nil || !strings.Contains(err.Error(), "apply only to kind") {
		t.Fatalf("token gate with preset: %v", err)
	}
	for name, g := range map[string]AuthGateConfig{
		"token gate with allowed_arns":         {Kind: "token", AllowedARNs: []string{"x"}},
		"token gate with trusted_proxy_cidrs":  {Kind: "token", TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
		"password gate with team_domain":       {Kind: "password", TeamDomain: "a.b"},
		"oidc gate with an edge-only key":      {Kind: "oidc", Preset: "google", ClientID: "x", SecretEnv: "LOOMCYCLE_X"},
		"oidc gate with trusted_proxy_cidrs":   {Kind: "oidc", Preset: "google", ClientID: "x", TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
		"header gate with an OIDC-only key":    func() AuthGateConfig { c := cfGateConfig(); c.DiscoveryURL = "https://x"; return c }(),
		"awsiam gate with an OIDC-only key":    func() AuthGateConfig { c := awsiamGateConfig(); c.Scopes = []string{"openid"}; return c }(),
		"header gate with allowed_return_to":   func() AuthGateConfig { c := cfGateConfig(); c.AllowedReturnTo = []string{"https://x"}; return c }(),
		"awsiam gate with a header preset key": func() AuthGateConfig { c := awsiamGateConfig(); c.Preset = "aws-alb"; return c }(),
	} {
		if err := resolveAuth(edgeConfig("", map[string]AuthGateConfig{"g": g})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResolveAuth_StrictCountsAHeaderGateButNotAnAWSIAMGate(t *testing.T) {
	if err := resolveAuth(edgeConfig("strict", map[string]AuthGateConfig{"cf": cfGateConfig()})); err != nil {
		t.Fatalf("strict with a header gate: %v", err)
	}
	err := resolveAuth(edgeConfig("strict", map[string]AuthGateConfig{"ci": awsiamGateConfig()}))
	if err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("strict with only an awsiam gate: %v", err)
	}
}

func TestResolveAuth_EdgeGateTakesTrusted(t *testing.T) {
	g := cfGateConfig()
	g.IdentityMap.NewUser = &AuthNewUserConfig{AccessMode: "tenant"}
	mustRefuse(t, g, "trusted")
	g.Trusted = true
	if err := resolveAuth(edgeConfig("", map[string]AuthGateConfig{"g": g})); err != nil {
		t.Fatalf("trusted header gate promoting new users: %v", err)
	}
}
