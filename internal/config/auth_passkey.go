package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// RFC FA P6: passkeys (WebAuthn). A passkey is the subject's, like its
// password: it is a second factor at a password gate, and a password-less
// sign-in through a kind: passkey gate.
//
//	auth:
//	  gates:
//	    keys:
//	      kind: passkey
//	      label: Sign in with a passkey
//
// The relying party is this deployment as LOOMCYCLE_PUBLIC_URL names it: the
// RP ID is its host and the only accepted origin is its origin, so a
// passkey made here answers nowhere else and a page served from any other
// origin cannot use one. A person registers a passkey from their own Web UI
// session (a password, identity or passkey sign-in, never a pasted token),
// so a passkey gate is not a deployment's only way in.

// AuthGateKindPasskey is the password-less passkey gate.
const AuthGateKindPasskey = "passkey"

// AuthPasskeyRP is the WebAuthn relying party, derived from
// LOOMCYCLE_PUBLIC_URL.
type AuthPasskeyRP struct {
	// ID is the RP ID: the public URL's host, without a port.
	ID string
	// Origin is the public URL's origin (scheme://host[:port]).
	Origin string
}

// passkeyRP derives the relying party from the public URL. WebAuthn runs only
// in a secure context and never on an IP address, so the URL must be https
// with a domain name, or http on localhost.
func passkeyRP(publicURL string) (*AuthPasskeyRP, error) {
	v := strings.TrimSpace(publicURL)
	if v == "" {
		return nil, fmt.Errorf("LOOMCYCLE_PUBLIC_URL is not set")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("LOOMCYCLE_PUBLIC_URL %q is not an absolute URL", v)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case net.ParseIP(host) != nil:
		return nil, fmt.Errorf("LOOMCYCLE_PUBLIC_URL host %q is an IP address; passkeys need a domain name", host)
	case u.Scheme == "https":
	case u.Scheme == "http" && host == "localhost":
	default:
		return nil, fmt.Errorf("LOOMCYCLE_PUBLIC_URL %q must be https (http only on localhost) for passkeys", v)
	}
	return &AuthPasskeyRP{ID: host, Origin: u.Scheme + "://" + strings.ToLower(u.Host)}, nil
}

// resolvePasskeys sets p.Passkeys when the deployment can use passkeys: a
// password or passkey gate is configured and LOOMCYCLE_PUBLIC_URL can be a
// relying party. A passkey gate without one fails the load; a password gate
// without one simply offers no passkeys (it predates them).
func resolvePasskeys(c *Config, p *AuthPolicy) error {
	var passkeyGate string
	password := false
	for _, g := range p.Gates {
		switch g.Kind {
		case AuthGateKindPasskey:
			if passkeyGate != "" {
				return fmt.Errorf("auth.gates: at most one gate may be kind: passkey (%s, %s)", passkeyGate, g.Name)
			}
			passkeyGate = g.Name
		case AuthGateKindPassword:
			password = true
		}
	}
	if passkeyGate == "" && !password {
		return nil
	}
	rp, err := passkeyRP(c.Env.PublicURL)
	if err != nil {
		if passkeyGate != "" {
			return fmt.Errorf("auth.gates.%s: a passkey gate needs a relying party: %w", passkeyGate, err)
		}
		return nil
	}
	p.Passkeys = rp
	return nil
}

// PasskeyGate is the configured passkey gate, if any.
func (p AuthPolicy) PasskeyGate() (AuthGate, bool) {
	for _, g := range p.Gates {
		if g.Kind == AuthGateKindPasskey {
			return g, true
		}
	}
	return AuthGate{}, false
}

// hasPasskeyGateKeys reports keys a passkey gate does not take: it has a
// label and nothing else.
func hasPasskeyGateKeys(g AuthGateConfig) bool {
	return g.IdentityMap != nil || g.Trusted || g.RequireMFA != nil || len(g.AllowedReturnTo) > 0
}
