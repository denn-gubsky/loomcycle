package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// RFC FA P5: the OAuth gate — sign-in through an OAuth 2.0 provider that is
// not an OpenID Provider (GitHub, GitLab, Gitea, or any provider with a
// userinfo endpoint), on the same redirect-login machinery as the OIDC gate.
//
//	auth:
//	  gates:
//	    gh:
//	      kind: oauth
//	      preset: github                  # github | gitlab | gitea | generic
//	      client_id: Iv1.abc
//	      client_secret_env: LOOMCYCLE_GATE_GH_SECRET
//	      base_url: https://ghe.example   # GitHub Enterprise, self-hosted GitLab, Gitea (required for gitea)
//	      allowed_orgs: [acme]            # only these orgs / org/teams become groups
//	      require_org: true               # refuse a person in none of them
//	      trusted: true                   # needed for identity_map.groups
//	      identity_map: { tenant: acme, provision: auto, groups: { provision: true } }
//	    sso:
//	      kind: oauth
//	      preset: generic
//	      auth_url: https://sso.example/authorize
//	      token_url: https://sso.example/token
//	      userinfo_url: https://sso.example/userinfo
//	      claims: { subject: sub, email: email, email_verified: email_verified, groups: roles }
//
// The subject is the provider's immutable user id; an e-mail is used only when
// the provider reports it verified; and an organisation or group becomes a
// loomcycle group only when allowed_orgs lists it.

// AuthGateKindOAuth is the OAuth gate (RFC FA §3.2).
const AuthGateKindOAuth = "oauth"

// OAuth presets.
const (
	OAuthPresetGitHub  = "github"
	OAuthPresetGitLab  = "gitlab"
	OAuthPresetGitea   = "gitea"
	OAuthPresetGeneric = "generic"
)

// AuthGateOAuthConfig holds the OAuth gate's own keys, inlined into
// AuthGateConfig. It shares preset, client_id, client_secret_env, scopes,
// token_auth, trusted and allowed_return_to with the OIDC gate.
type AuthGateOAuthConfig struct {
	BaseURL     string                 `yaml:"base_url,omitempty"`
	AuthURL     string                 `yaml:"auth_url,omitempty"`
	TokenURL    string                 `yaml:"token_url,omitempty"`
	UserinfoURL string                 `yaml:"userinfo_url,omitempty"`
	Claims      *AuthOAuthClaimsConfig `yaml:"claims,omitempty"`
	AllowedOrgs []string               `yaml:"allowed_orgs,omitempty"`
	RequireOrg  bool                   `yaml:"require_org,omitempty"`
	// AllowAnyAccount lets a gate on public github.com or gitlab.com admit any
	// account there. Without it such a gate must restrict sign-in to
	// allowed_orgs with require_org, the way the OIDC google preset requires
	// allowed_domains: otherwise anyone with an account could sign in.
	AllowAnyAccount bool `yaml:"allow_any_account,omitempty"`
	// TrustEmail lets the gate assert the provider's verified e-mail. Nil:
	// on for github.com and gitlab.com, off for a self-managed instance
	// (base_url) and for generic, whose address confirmation loomcycle
	// cannot see.
	TrustEmail *bool `yaml:"trust_email,omitempty"`
}

// AuthOAuthClaimsConfig names the generic preset's userinfo fields.
type AuthOAuthClaimsConfig struct {
	Subject       string `yaml:"subject,omitempty"`
	Email         string `yaml:"email,omitempty"`
	EmailVerified string `yaml:"email_verified,omitempty"`
	Name          string `yaml:"name,omitempty"`
	Login         string `yaml:"login,omitempty"`
	Groups        string `yaml:"groups,omitempty"`
}

// AuthOAuthGate is a validated OAuth gate.
type AuthOAuthGate struct {
	Preset   string
	ClientID string
	// ClientSecret is read from ClientSecretEnv at load. Never logged or
	// serialised.
	ClientSecret    string `json:"-" yaml:"-"`
	ClientSecretEnv string
	BaseURL         string
	AuthURL         string
	TokenURL        string
	UserinfoURL     string
	Claims          AuthOAuthClaimsConfig
	Scopes          []string
	TokenAuth       string
	AllowedOrgs     []string
	RequireOrg      bool
	TrustEmail      bool
	AllowedReturnTo []string
	// RedirectURI is <LOOMCYCLE_PUBLIC_URL>/v1/_auth/complete/<gate>.
	RedirectURI string
}

func hasOAuthKeys(g AuthGateConfig) bool {
	o := g.AuthGateOAuthConfig
	// allow_any_account is not here: the OIDC google preset takes it too.
	return o.BaseURL != "" || o.AuthURL != "" || o.TokenURL != "" || o.UserinfoURL != "" || o.Claims != nil ||
		len(o.AllowedOrgs) > 0 || o.RequireOrg || o.TrustEmail != nil
}

// resolveOAuthGate validates one kind: oauth gate.
func resolveOAuthGate(c *Config, name string, g AuthGateConfig) (*AuthOAuthGate, error) {
	where := "auth.gates." + name
	// The OIDC-only keys: an OAuth provider has no ID token to check them on.
	if g.Issuer != "" || g.DiscoveryURL != "" || len(g.AllowedDomains) > 0 || len(g.AllowedTIDs) > 0 || g.EndSession {
		return nil, fmt.Errorf("%s: issuer, discovery_url, allowed_domains, allowed_tids and end_session apply only to kind: oidc", where)
	}
	if strings.TrimSpace(c.Env.PublicURL) == "" {
		return nil, fmt.Errorf("%s: an OAuth gate needs LOOMCYCLE_PUBLIC_URL: its redirect URI is built from it", where)
	}
	pub, err := providerURL("LOOMCYCLE_PUBLIC_URL", c.Env.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	o := &AuthOAuthGate{Preset: strings.TrimSpace(g.Preset), ClientID: strings.TrimSpace(g.ClientID),
		RequireOrg: g.RequireOrg, RedirectURI: strings.TrimRight(pub, "/") + "/v1/_auth/complete/" + name}
	if o.ClientID == "" {
		return nil, fmt.Errorf("%s.client_id: required", where)
	}
	if g.ClientSecret != "" {
		return nil, fmt.Errorf("%s.client_secret: a literal secret is refused; set client_secret_env to the name of an environment variable", where)
	}
	envName := strings.TrimSpace(g.ClientSecretEnv)
	switch {
	case envName == "":
		return nil, fmt.Errorf("%s.client_secret_env: required (the gate is a confidential client)", where)
	case !ExpandEnvAllowed(envName):
		return nil, fmt.Errorf("%s.client_secret_env: %q must be LOOMCYCLE_*-prefixed (or an allowlisted name)", where, envName)
	case expandDenyNames[envName]:
		return nil, fmt.Errorf("%s.client_secret_env: %q is one of loomcycle's own infrastructure secrets", where, envName)
	}
	o.ClientSecretEnv, o.ClientSecret = envName, os.Getenv(envName)
	if o.ClientSecret == "" {
		return nil, fmt.Errorf("%s.client_secret_env: %s is empty or unset", where, envName)
	}
	principalBearerEnvs.add(envName)
	switch t := strings.TrimSpace(g.TokenAuth); t {
	case "", "basic", "post":
		o.TokenAuth = t
	default:
		return nil, fmt.Errorf("%s.token_auth: %q (want basic or post)", where, t)
	}
	for _, sc := range g.Scopes {
		if sc = strings.TrimSpace(sc); !oidcScopeRE.MatchString(sc) {
			return nil, fmt.Errorf("%s.scopes: %q is not a scope", where, sc)
		}
		o.Scopes = append(o.Scopes, sc)
	}
	for _, rt := range g.AllowedReturnTo {
		if u, err := providerURL(where+".allowed_return_to", strings.Split(rt, "?")[0]); err != nil || u == "" {
			return nil, fmt.Errorf("%s.allowed_return_to: %q must be an absolute https URL (http only on loopback)", where, rt)
		}
		o.AllowedReturnTo = append(o.AllowedReturnTo, strings.TrimSpace(rt))
	}
	for _, org := range g.AllowedOrgs {
		if org = strings.TrimSpace(org); org == "" || strings.ContainsAny(org, "/ \t") || len(org) > 255 {
			return nil, fmt.Errorf("%s.allowed_orgs: %q is not an organisation name", where, org)
		}
		o.AllowedOrgs = append(o.AllowedOrgs, org)
	}
	if o.RequireOrg && len(o.AllowedOrgs) == 0 {
		return nil, fmt.Errorf("%s.require_org: needs allowed_orgs", where)
	}
	if g.BaseURL != "" {
		if o.BaseURL, err = providerURL(where+".base_url", g.BaseURL); err != nil {
			return nil, err
		}
		// A base_url naming the public instance IS the public instance. Left
		// as written it would read as self-managed and skip the open-sign-in
		// rule below, so it is folded into the preset's default.
		if publicOAuthInstance(o.Preset, o.BaseURL) {
			o.BaseURL = ""
		}
	}
	switch o.Preset {
	case OAuthPresetGitHub, OAuthPresetGitLab:
		// The public SaaS instance has an account for anyone, so sign-in must be
		// confined to an organisation unless the operator chose open sign-in.
		// A self-managed instance (base_url) controls its own accounts.
		if o.BaseURL == "" && !g.AllowAnyAccount && (len(o.AllowedOrgs) == 0 || !o.RequireOrg) {
			return nil, fmt.Errorf("%s: preset %s on the public instance admits any account there; set allowed_orgs with "+
				"require_org: true, or allow_any_account: true to allow that deliberately", where, o.Preset)
		}
	case OAuthPresetGitea:
		if o.BaseURL == "" {
			return nil, fmt.Errorf("%s.base_url: required for preset gitea", where)
		}
	case OAuthPresetGeneric:
		if o.BaseURL != "" {
			return nil, fmt.Errorf("%s.base_url: preset generic takes auth_url, token_url and userinfo_url instead", where)
		}
		// A fixed order, so the first missing key is the one reported.
		for _, kv := range []struct {
			key string
			v   string
		}{{"auth_url", g.AuthURL}, {"token_url", g.TokenURL}, {"userinfo_url", g.UserinfoURL}} {
			if strings.TrimSpace(kv.v) == "" {
				return nil, fmt.Errorf("%s.%s: required for preset generic", where, kv.key)
			}
			if _, err := providerURL(where+"."+kv.key, kv.v); err != nil {
				return nil, err
			}
		}
		o.AuthURL, o.TokenURL, o.UserinfoURL = strings.TrimSpace(g.AuthURL), strings.TrimSpace(g.TokenURL), strings.TrimSpace(g.UserinfoURL)
		if g.Claims != nil {
			o.Claims = *g.Claims
		}
		if o.Claims.Groups == "" && len(o.AllowedOrgs) > 0 {
			return nil, fmt.Errorf("%s.allowed_orgs: preset generic reads groups from claims.groups; set it", where)
		}
	case "":
		return nil, fmt.Errorf("%s.preset: required (github, gitlab, gitea or generic)", where)
	default:
		return nil, fmt.Errorf("%s.preset: unknown preset %q (want github, gitlab, gitea or generic)", where, o.Preset)
	}
	if o.Preset != OAuthPresetGeneric && (g.AuthURL != "" || g.TokenURL != "" || g.UserinfoURL != "" || g.Claims != nil) {
		return nil, fmt.Errorf("%s: auth_url, token_url, userinfo_url and claims apply only to preset generic", where)
	}
	if g.IdentityMap == nil {
		return nil, fmt.Errorf("%s.identity_map: required (which tenant an OAuth identity lands in)", where)
	}
	// A provider's login, name and username are the user's to rename, so an
	// identity map keyed on them would let a rename land on someone else's
	// account or tenant. The gate asserts only sub (the immutable id), a
	// verified e-mail and its own org-filtered groups; the map may use only
	// those.
	m := g.IdentityMap
	if s := strings.TrimSpace(m.Subject); s != "" && s != "sub" {
		return nil, fmt.Errorf("%s.identity_map.subject: an OAuth gate's subject is the provider's immutable id (sub), not %q", where, s)
	}
	if tc := strings.TrimSpace(m.TenantClaim); tc != "" && tc != EmailDomainClaim {
		return nil, fmt.Errorf("%s.identity_map.tenant_claim: an OAuth gate picks a tenant only by %s (a verified e-mail), not %q",
			where, EmailDomainClaim, tc)
	}
	if m.Groups != nil {
		if gc := strings.TrimSpace(m.Groups.Claim); gc != "" && gc != "groups" {
			return nil, fmt.Errorf("%s.identity_map.groups.claim: an OAuth gate's groups are its own allowed_orgs-filtered list; leave claim empty, not %q",
				where, gc)
		}
	}
	o.TrustEmail = (o.Preset == OAuthPresetGitHub || o.Preset == OAuthPresetGitLab) && o.BaseURL == ""
	if g.TrustEmail != nil {
		o.TrustEmail = *g.TrustEmail
	}
	return o, nil
}

// publicOAuthHosts are the hosts of the public instances the github and gitlab
// presets default to: anyone can hold an account there.
var publicOAuthHosts = map[string][]string{
	OAuthPresetGitHub: {"github.com", "www.github.com", "api.github.com"},
	OAuthPresetGitLab: {"gitlab.com", "www.gitlab.com"},
}

// publicOAuthInstance reports whether base names the preset's public instance,
// however its host is spelled (case, a trailing dot, the default port).
func publicOAuthInstance(preset, base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	for _, h := range publicOAuthHosts[preset] {
		if host == h {
			return true
		}
	}
	return false
}
