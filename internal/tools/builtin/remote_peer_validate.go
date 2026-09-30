package builtin

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Authoring-time checks shared by the MemoryBackendDef and DocumentSourceDef
// tools. Both defs are dialed at base_url with an operator credential attached,
// and both are authored at runtime, so what an author may store is narrower
// than what an operator may declare in yaml (config.Validate, untouched here).

// requirePublicIPLiteral refuses a base_url whose host is a LITERAL private,
// loopback, link-local (incl. the 169.254.169.254 metadata service), multicast
// or unspecified IP, IPv4 or IPv6.
//
// It is the early, clear error; the load-bearing defence is the dial guard,
// which also covers hostnames that resolve to private addresses (DNS can change
// between authoring and dial, so no hostname is resolved here). A runtime
// author who needs a private peer names it by a hostname the operator lists in
// LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST; an operator can instead declare the def
// in yaml, where private IPs stay allowed.
func requirePublicIPLiteral(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return nil // requireHTTPURL reports the malformed URL
	}
	host := u.Hostname()
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // an IPv6 zone ("fe80::1%eth0") does not parse as an IP
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
		return fmt.Errorf("%s %q is a private, loopback, link-local or metadata address — a definition authored at runtime may not name one. "+
			"Use a hostname the operator lists in LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST, or ask the operator to declare this peer in the server config", field, raw)
	}
	return nil
}

// requireCredentialSafeEnvPattern checks a key_per_tenant env_pattern the way
// api_key_env is checked: the env-var name it produces is resolved and SENT to
// base_url, so it must be a name an author may use as a credential.
//
// The name depends on the tenant id, which is not known at authoring, so the
// pattern is checked under two substitutions:
//   - the EMPTY tenant — what the shared tenant really resolves, and the case
//     that turns "LOOMCYCLE_AUTH_TOKEN{tenant_id}" into the operator bearer.
//   - a representative non-empty one — which refuses a pattern outside the
//     LOOMCYCLE_ namespace ("PEER_{tenant_id}_KEY", "BRAVE_API_KEY{tenant_id}"):
//     the other allowlisted names are exact, so a pattern can only ever
//     resolve inside LOOMCYCLE_.
//
// The part the tenant id supplies is the operator's, not the author's; the
// runtime resolver (resolveCredentialEnv) still re-checks every resolved name,
// so a tenant id that happens to complete a denied name is refused there.
func requireCredentialSafeEnvPattern(field, pattern string) error {
	for _, sample := range []string{"", "t"} {
		if name := strings.ReplaceAll(pattern, "{tenant_id}", sample); !config.EnvNameCredentialSafe(name) {
			return fmt.Errorf("%s %q is not an allowed credential env var pattern (with tenant %q it names %q): "+
				"it must be LOOMCYCLE_-prefixed and must not produce one of loomcycle's own infrastructure secrets", field, pattern, sample, name)
		}
	}
	return nil
}

// remotePeerHostListed reports whether the operator lists baseURL's host in
// LOOMCYCLE_HTTP_HOST_ALLOWLIST or LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST — the
// floor a runtime-authored MCPServerDef already meets. Empty lists deny.
//
// It is a NAME check beside the dial guard's ADDRESS check: the guard stops a
// runtime def reaching a private host, but not a public one, and a public host
// is where a def could send the credential it names.
func remotePeerHostListed(cfg *config.Config, baseURL string) bool {
	if cfg == nil {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return hostAllowed(host, cfg.Env.HTTPHostAllowlist) || hostAllowed(host, cfg.Env.HTTPPrivateHostAllowlist)
}

// requireListedPeerHost is the host floor as authoring and restore apply it.
//
// operatorBaseURL is the base_url the operator declares in yaml under the
// def's name ("" when none), and a def at exactly that URL is exempt: a row
// the def tool bootstrapped from yaml resolves as operator-authored at dial
// and keeps the operator's host, so a restore must not refuse it. A create or
// fork passes "": what it writes is a runtime def, dialed under the floor
// even at the yaml URL, so accepting it would store a peer that never dials.
func requireListedPeerHost(cfg *config.Config, field, baseURL, operatorBaseURL string) error {
	if operatorBaseURL != "" && baseURL == operatorBaseURL {
		return nil
	}
	if remotePeerHostListed(cfg, baseURL) {
		return nil
	}
	host := baseURL
	if u, err := url.Parse(baseURL); err == nil {
		host = u.Hostname()
	}
	return fmt.Errorf("%s host %q is not in LOOMCYCLE_HTTP_HOST_ALLOWLIST or LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST: "+
		"a peer defined at runtime is reached only at a host the operator lists. "+
		"Ask the operator to list it, or to declare this peer in the server config", field, host)
}

// requireDialablePeerHost is the host floor at dial: a remote memory backend
// or document source that is not operator-authored is refused before any
// client is built, so nothing — no request, no Authorization header — reaches
// an unlisted host. This is the load-bearing copy of the authoring check: a
// def stored before the floor existed never passed it.
func requireDialablePeerHost(cfg *config.Config, baseURL string, origin lookup.Origin) error {
	if origin == lookup.OriginOperator {
		return nil
	}
	return requireListedPeerHost(cfg, "base_url", baseURL, "")
}

// remotePeerCredential is what decides which credential a remote peer def
// sends to its base_url.
type remotePeerCredential struct {
	BaseURL     string
	APIKeyEnv   string
	TenancyKind string
	EnvPattern  string
}

// requireAuthorBoundCredential limits the credential a NON-ADMIN author may
// attach to a remote memory backend or document source. api_key_env may name
// any credential-safe env var — another tenant's LOOMCYCLE_PEER_KEY_<tenant>,
// a GITHUB_TOKEN — so, paired with a base_url the author picks, it would send
// that key wherever the author points it. A non-admin may name only:
//
//   - no credential;
//   - a key_per_tenant env_pattern (the shape check requires {tenant_id} in
//     it). The name is completed from the RUN's tenant at each call, and a def
//     authored in a tenant resolves only for that tenant's runs, so the key
//     sent is the run's own. Refused for an author with no tenant: that def
//     lands in the shared layer every tenant resolves, so each tenant's key
//     would go to the author's host.
//
// The operator's own pairing is kept: a fork that leaves base_url and the
// credential exactly as the yaml entry for this name declares them (static)
// sends the operator's key to the operator's host.
//
// An admin (substrate:admin) keeps free api_key_env. Called by the def tools
// only: a snapshot restore is an admin action, so its validators check the
// shape and the host floor, not this.
func requireAuthorBoundCredential(ctx context.Context, def remotePeerCredential, static *remotePeerCredential) error {
	if defCallerIsAdmin(ctx) {
		return nil
	}
	if static != nil && def == *static {
		return nil
	}
	if def.APIKeyEnv != "" {
		return fmt.Errorf("config.api_key_env %q may be set only by an admin: it names a credential this server holds, and the definition sends it to its own base_url. "+
			"Leave it unset, or use tenancy_strategy {\"kind\":\"key_per_tenant\",\"env_pattern\":\"LOOMCYCLE_..._{tenant_id}\"} so each run sends its own tenant's key", def.APIKeyEnv)
	}
	if def.TenancyKind == "key_per_tenant" && def.EnvPattern != "" && tools.RunIdentity(ctx).TenantID == "" {
		return fmt.Errorf("tenancy_strategy.env_pattern %q may not be set by an author with no tenant: the definition would be shared by every tenant, "+
			"and each tenant's key would be sent to its base_url", def.EnvPattern)
	}
	return nil
}
