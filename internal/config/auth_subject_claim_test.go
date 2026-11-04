package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A gate's subject names the account a sign-in reaches, so it must come from
// a claim the provider never reassigns. Keyed on anything else (an e-mail, a
// username) a person who gets the provider to put another person's value in
// that claim signs in as them, including into a user an operator made in
// advance. Such a claim loads only when the operator declares it immutable.
func TestResolveAuth_ANonImmutableSubjectClaimNeedsADeclaration(t *testing.T) {
	const tid = "11111111-1111-1111-1111-111111111111"
	oidc := func(t *testing.T, preset, issuer string, m *AuthIdentityMapConfig) *Config {
		g := AuthGateConfig{Preset: preset, Issuer: issuer, IdentityMap: m}
		if preset == OIDCPresetEntra {
			g.Issuer, g.AllowedTIDs = "", []string{tid}
		}
		return oidcConfig(t, "", g)
	}
	header := func(_ *testing.T, m *AuthIdentityMapConfig) *Config {
		g := cfGateConfig()
		g.IdentityMap = m
		return edgeConfig("", map[string]AuthGateConfig{"cf": g})
	}
	awsiam := func(_ *testing.T, m *AuthIdentityMapConfig) *Config {
		g := awsiamGateConfig()
		g.IdentityMap = m
		return edgeConfig("", map[string]AuthGateConfig{"ci": g})
	}
	external := func(t *testing.T, m *AuthIdentityMapConfig) *Config {
		g := AuthGateConfig{Kind: "external", IdentityMap: m}
		g.External.PublicKey = edJWK(t)
		return externalCfg(g)
	}
	subject := func(claim string, declared bool) *AuthIdentityMapConfig {
		return &AuthIdentityMapConfig{Tenant: "acme", Subject: claim, SubjectClaimImmutable: declared}
	}
	refused := map[string]func(t *testing.T) *Config{
		"oidc generic email": func(t *testing.T) *Config {
			return oidc(t, "generic", "https://idp.example", subject("email", false))
		},
		"oidc keycloak preferred_username": func(t *testing.T) *Config {
			return oidc(t, "keycloak", "https://idp.example/realms/acme", subject("preferred_username", false))
		},
		"oidc entra upn": func(t *testing.T) *Config { return oidc(t, "entra", "", subject("upn", false)) },
		"oidc auth0 oid": func(t *testing.T) *Config {
			return oidc(t, "auth0", "https://acme.eu.auth0.com/", subject("oid", false))
		},
		"header email":         func(t *testing.T) *Config { return header(t, subject("email", false)) },
		"external email":       func(t *testing.T) *Config { return external(t, subject("email", false)) },
		"awsiam caller_arn":    func(t *testing.T) *Config { return awsiam(t, subject("caller_arn", false)) },
		"awsiam user_id":       func(t *testing.T) *Config { return awsiam(t, subject("user_id", false)) },
		"external custom name": func(t *testing.T) *Config { return external(t, subject("name", false)) },
	}
	for name, mk := range refused {
		err := resolveAuth(mk(t))
		if err == nil || !strings.Contains(err.Error(), "identity_map.subject") || !strings.Contains(err.Error(), "subject_claim_immutable") {
			t.Errorf("%s: err = %v, want an identity_map.subject refusal naming subject_claim_immutable", name, err)
		}
	}
	accepted := map[string]func(t *testing.T) *Config{
		"oidc default":   func(t *testing.T) *Config { return oidc(t, "generic", "https://idp.example", subject("", false)) },
		"oidc sub":       func(t *testing.T) *Config { return oidc(t, "generic", "https://idp.example", subject("sub", false)) },
		"oidc entra oid": func(t *testing.T) *Config { return oidc(t, "entra", "", subject("oid", false)) },
		"awsiam arn":     func(t *testing.T) *Config { return awsiam(t, subject("arn", false)) },
		"header sub":     func(t *testing.T) *Config { return header(t, subject("sub", false)) },
		"oidc generic email declared": func(t *testing.T) *Config {
			return oidc(t, "generic", "https://idp.example", subject("email", true))
		},
		"header email declared":   func(t *testing.T) *Config { return header(t, subject("email", true)) },
		"external email declared": func(t *testing.T) *Config { return external(t, subject("email", true)) },
	}
	for name, mk := range accepted {
		if err := resolveAuth(mk(t)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The declaration is a statement about a claim other than the provider's own
// id. On the provider's id it says nothing, and on a gate whose subject is
// fixed (OAuth: the provider's id; SCIM: the SCIM id) it cannot widen it.
func TestResolveAuth_TheSubjectClaimDeclarationNeverWidensAFixedSubject(t *testing.T) {
	c := oidcConfig(t, "", AuthGateConfig{Preset: "generic", Issuer: "https://idp.example",
		IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", SubjectClaimImmutable: true}})
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "subject_claim_immutable") {
		t.Errorf("declaration on the default sub: %v", err)
	}
	o := oauthGateConfig(t, AuthGateConfig{Preset: "github", IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", Subject: "email", SubjectClaimImmutable: true}})
	if err := resolveAuth(o); err == nil || !strings.Contains(err.Error(), "identity_map.subject") {
		t.Errorf("an OAuth gate took a declared e-mail subject: %v", err)
	}
	s := scimConfig(map[string]AuthGateConfig{"scim": {Kind: AuthGateKindSCIM, IdentityMap: &AuthIdentityMapConfig{SubjectClaimImmutable: true}}})
	if err := resolveAuth(s); err == nil || !strings.Contains(err.Error(), "subject_claim_immutable") {
		t.Errorf("a SCIM gate took the declaration: %v", err)
	}
}

// The yaml key is subject_claim_immutable, beside subject.
func TestResolveAuth_SubjectClaimImmutableDecodesFromYAML(t *testing.T) {
	var m AuthIdentityMapConfig
	if err := yaml.Unmarshal([]byte("tenant: acme\nsubject: email\nsubject_claim_immutable: true\n"), &m); err != nil {
		t.Fatal(err)
	}
	if m.Subject != "email" || !m.SubjectClaimImmutable {
		t.Fatalf("decoded = %+v", m)
	}
}
