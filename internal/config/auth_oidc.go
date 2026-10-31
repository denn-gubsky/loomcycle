package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// RFC FA §3.2 / §9: the OIDC gate (kind: oidc).
//
//	auth:
//	  gates:
//	    google:
//	      kind: oidc
//	      preset: google                 # google | entra | okta | keycloak | cognito | identity-center | auth0 | generic
//	      client_id: 1234.apps.googleusercontent.com
//	      client_secret_env: LOOMCYCLE_GATE_GOOGLE_SECRET
//	      allowed_domains: [acme.com]    # google: the hd claim
//	      identity_map: { tenant: acme, provision: auto }
//
// The gate is a confidential client (a BFF): it holds the client secret and
// the provider's tokens, the browser only ever gets loomcycle's session
// cookie. The secret is named by an env var, never written in yaml; the
// redirect URI is always <LOOMCYCLE_PUBLIC_URL>/v1/_auth/complete/<gate>.

// AuthGateKindOIDC is the OpenID Connect gate.
const AuthGateKindOIDC = "oidc"

// OIDC presets.
const (
	OIDCPresetGoogle         = "google"
	OIDCPresetEntra          = "entra"
	OIDCPresetOkta           = "okta"
	OIDCPresetKeycloak       = "keycloak"
	OIDCPresetCognito        = "cognito"
	OIDCPresetIdentityCenter = "identity-center"
	OIDCPresetAuth0          = "auth0"
	OIDCPresetGeneric        = "generic"
)

// GoogleIssuer is Google's OIDC issuer.
const GoogleIssuer = "https://accounts.google.com"

// EntraAuthority is the Microsoft identity platform host.
const EntraAuthority = "https://login.microsoftonline.com"

var (
	oidcScopeRE    = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,128}$`)
	entraTIDRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	cognitoIssRE   = regexp.MustCompile(`^https://cognito-idp\.[a-z0-9-]+\.amazonaws\.com/[a-z0-9-]+_[A-Za-z0-9]+$`)
	googleDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// AuthOIDCGate is a validated OIDC gate.
type AuthOIDCGate struct {
	Preset   string
	ClientID string
	// ClientSecret is read from ClientSecretEnv at load. Never logged or
	// serialised.
	ClientSecret    string `json:"-" yaml:"-"`
	ClientSecretEnv string
	// Issuer is the exact iss the ID token must carry, or for Entra with
	// several tenants the template https://login.microsoftonline.com/{tenantid}/v2.0
	// (IssuerTemplate set).
	Issuer         string
	IssuerTemplate bool
	// DiscoveryURL is where the provider's metadata is read.
	DiscoveryURL string
	// AllowedDomains (google) are the hd values admitted.
	AllowedDomains []string
	// AnyAccount (google, allow_any_account) admits every Google account,
	// personal ones included, in place of AllowedDomains. The gate's
	// identity_map must then hold new users for approval or admit only users
	// an operator made (checked where the identity map is resolved).
	AnyAccount bool
	// AllowedTIDs (entra) are the Entra tenant ids admitted.
	AllowedTIDs []string
	Scopes      []string
	// EndSession sends the browser to the provider's end_session_endpoint
	// at logout.
	EndSession bool
	// TokenAuth is how the client authenticates at the token endpoint:
	// "" (detect), basic or post.
	TokenAuth       string
	AllowedReturnTo []string
	// RedirectURI is <LOOMCYCLE_PUBLIC_URL>/v1/_auth/complete/<gate>.
	RedirectURI string
}

// hasOIDCKeys reports whether any OIDC-only key is set.
func hasOIDCKeys(g AuthGateConfig) bool {
	return g.Preset != "" || g.ClientID != "" || g.ClientSecretEnv != "" || g.ClientSecret != "" || g.Issuer != "" ||
		g.DiscoveryURL != "" || len(g.AllowedDomains) > 0 || len(g.AllowedTIDs) > 0 || len(g.Scopes) > 0 ||
		g.EndSession || g.TokenAuth != ""
}

// providerURL accepts an https URL, or http on a loopback host (a local
// provider in a test or a lab).
func providerURL(where, v string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", fmt.Errorf("%s: %q is not an absolute URL without user info, query or fragment", where, v)
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("%s: %q must be https (http only on loopback)", where, v)
	}
	return strings.TrimSpace(v), nil
}

// isGoogleURL reports whether u is on Google's sign-in host.
func isGoogleURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.EqualFold(strings.TrimSuffix(p.Hostname(), "."), "accounts.google.com")
}

// resolveOIDCGate validates one kind: oidc gate.
func resolveOIDCGate(c *Config, name string, g AuthGateConfig) (*AuthOIDCGate, error) {
	where := "auth.gates." + name
	if strings.TrimSpace(c.Env.PublicURL) == "" {
		return nil, fmt.Errorf("%s: an OIDC gate needs LOOMCYCLE_PUBLIC_URL: its redirect URI is built from it", where)
	}
	pub, err := providerURL("LOOMCYCLE_PUBLIC_URL", c.Env.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	o := &AuthOIDCGate{Preset: strings.TrimSpace(g.Preset), ClientID: strings.TrimSpace(g.ClientID), EndSession: g.EndSession,
		RedirectURI: strings.TrimRight(pub, "/") + "/v1/_auth/complete/" + name}
	if o.Preset == "" {
		o.Preset = OIDCPresetGeneric
	}
	if o.ClientID == "" {
		return nil, fmt.Errorf("%s.client_id: required", where)
	}
	// The client secret: an env var name only. A literal in yaml would sit in
	// every copy of the config file and every backup of it.
	if g.ClientSecret != "" {
		return nil, fmt.Errorf("%s.client_secret: a literal secret is refused, and $cred: references are not supported "+
			"by this release yet; set client_secret_env to the name of an environment variable", where)
	}
	envName := strings.TrimSpace(g.ClientSecretEnv)
	if envName == "" {
		return nil, fmt.Errorf("%s.client_secret_env: required (the gate is a confidential client)", where)
	}
	if !ExpandEnvAllowed(envName) {
		return nil, fmt.Errorf("%s.client_secret_env: %q must be LOOMCYCLE_*-prefixed (or an allowlisted name)", where, envName)
	}
	if expandDenyNames[envName] {
		return nil, fmt.Errorf("%s.client_secret_env: %q is one of loomcycle's own infrastructure secrets", where, envName)
	}
	o.ClientSecretEnv = envName
	o.ClientSecret = os.Getenv(envName)
	if o.ClientSecret == "" {
		return nil, fmt.Errorf("%s.client_secret_env: %s is empty or unset", where, envName)
	}
	// Never interpolated into a definition, like a principal's bearer env.
	principalBearerEnvs.add(envName)
	switch t := strings.TrimSpace(g.TokenAuth); t {
	case "", "basic", "post":
		o.TokenAuth = t
	default:
		return nil, fmt.Errorf("%s.token_auth: %q (want basic or post)", where, t)
	}
	o.Scopes = []string{"openid", "email", "profile"}
	if len(g.Scopes) > 0 {
		o.Scopes = []string{"openid"}
		for _, sc := range g.Scopes {
			sc = strings.TrimSpace(sc)
			if sc == "openid" {
				continue
			}
			if !oidcScopeRE.MatchString(sc) {
				return nil, fmt.Errorf("%s.scopes: %q is not a scope", where, sc)
			}
			o.Scopes = append(o.Scopes, sc)
		}
	}
	for _, rt := range g.AllowedReturnTo {
		u, err := providerURL(where+".allowed_return_to", strings.Split(rt, "?")[0])
		if err != nil || u == "" {
			return nil, fmt.Errorf("%s.allowed_return_to: %q must be an absolute https URL (http only on loopback)", where, rt)
		}
		o.AllowedReturnTo = append(o.AllowedReturnTo, strings.TrimSpace(rt))
	}
	if len(g.AllowedDomains) > 0 && o.Preset != OIDCPresetGoogle {
		return nil, fmt.Errorf("%s.allowed_domains: applies to preset: google only", where)
	}
	if g.AllowAnyAccount && o.Preset != OIDCPresetGoogle {
		return nil, fmt.Errorf("%s.allow_any_account: applies to preset: google (and kind: oauth) only", where)
	}
	if len(g.AllowedTIDs) > 0 && o.Preset != OIDCPresetEntra {
		return nil, fmt.Errorf("%s.allowed_tids: applies to preset: entra only", where)
	}
	issuer := strings.TrimSpace(g.Issuer)
	switch o.Preset {
	case OIDCPresetGoogle:
		if issuer != "" && issuer != GoogleIssuer {
			return nil, fmt.Errorf("%s.issuer: preset google uses %s", where, GoogleIssuer)
		}
		o.Issuer = GoogleIssuer
		o.AnyAccount = g.AllowAnyAccount
		if o.AnyAccount && len(g.AllowedDomains) > 0 {
			return nil, fmt.Errorf("%s: allow_any_account and allowed_domains exclude each other "+
				"(allowed_domains admits only those Workspace domains)", where)
		}
		if len(g.AllowedDomains) == 0 && !o.AnyAccount {
			// Without a Workspace domain any Google account signs in, so
			// that takes saying so.
			return nil, fmt.Errorf("%s.allowed_domains: required for preset google (the Workspace domains whose hd claim is admitted), "+
				"or allow_any_account: true to admit every Google account, personal ones included, on purpose", where)
		}
		for _, d := range g.AllowedDomains {
			d = strings.ToLower(strings.TrimSpace(d))
			if !googleDomainRE.MatchString(d) {
				return nil, fmt.Errorf("%s.allowed_domains: %q is not a domain", where, d)
			}
			o.AllowedDomains = append(o.AllowedDomains, d)
		}
	case OIDCPresetEntra:
		if len(g.AllowedTIDs) == 0 {
			return nil, fmt.Errorf("%s.allowed_tids: required for preset entra (the Entra tenant ids admitted)", where)
		}
		for _, t := range g.AllowedTIDs {
			t = strings.ToLower(strings.TrimSpace(t))
			if !entraTIDRE.MatchString(t) {
				return nil, fmt.Errorf("%s.allowed_tids: %q is not an Entra tenant id (a GUID)", where, t)
			}
			o.AllowedTIDs = append(o.AllowedTIDs, t)
		}
		if issuer != "" {
			return nil, fmt.Errorf("%s.issuer: preset entra derives the issuer from allowed_tids", where)
		}
		if len(o.AllowedTIDs) == 1 {
			o.Issuer = EntraAuthority + "/" + o.AllowedTIDs[0] + "/v2.0"
		} else {
			// Several tenants: the organizations endpoint, whose ID tokens
			// carry each tenant's own issuer.
			o.Issuer, o.IssuerTemplate = EntraAuthority+"/{tenantid}/v2.0", true
			o.DiscoveryURL = EntraAuthority + "/organizations/v2.0/.well-known/openid-configuration"
		}
	case OIDCPresetCognito:
		if !cognitoIssRE.MatchString(issuer) {
			return nil, fmt.Errorf("%s.issuer: preset cognito wants https://cognito-idp.<region>.amazonaws.com/<user pool id>", where)
		}
		o.Issuer = issuer
	case OIDCPresetOkta, OIDCPresetKeycloak, OIDCPresetIdentityCenter, OIDCPresetAuth0, OIDCPresetGeneric:
		if issuer == "" {
			return nil, fmt.Errorf("%s.issuer: required for preset %s", where, o.Preset)
		}
		if o.Issuer, err = providerURL(where+".issuer", issuer); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s.preset: unknown preset %q (want google, entra, okta, keycloak, cognito, identity-center, auth0 or generic)", where, o.Preset)
	}
	if d := strings.TrimSpace(g.DiscoveryURL); d != "" {
		if o.Preset != OIDCPresetGeneric {
			return nil, fmt.Errorf("%s.discovery_url: applies to preset: generic only", where)
		}
		if o.DiscoveryURL, err = providerURL(where+".discovery_url", d); err != nil {
			return nil, err
		}
	}
	if o.DiscoveryURL == "" {
		o.DiscoveryURL = strings.TrimRight(o.Issuer, "/") + "/.well-known/openid-configuration"
	}
	// Google through any other preset would skip the google preset's own
	// rule (allowed_domains, or an explicit allow_any_account) and admit
	// every Google account in the world.
	if o.Preset != OIDCPresetGoogle && (isGoogleURL(o.Issuer) || isGoogleURL(o.DiscoveryURL)) {
		return nil, fmt.Errorf("%s: Google is the google preset's (preset: google, with allowed_domains or allow_any_account)", where)
	}
	return o, nil
}
