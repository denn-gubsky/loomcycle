package builtin

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
