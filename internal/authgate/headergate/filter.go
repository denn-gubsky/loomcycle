// Package headergate verifies identity assertions a trusted edge adds to
// every request (RFC FA §3.2 HeaderGate): a Cloudflare Access JWT, an AWS ALB
// OIDC data token, or a forward-auth proxy's HMAC-signed identity header.
//
// Never a plain identity header. The ForwardAuth header-smuggling CVEs
// (CVE-2026-88011, -88879, -39858, -35051) are a backend believing a header
// its proxy did not canonicalise: the proxy strips X-Forwarded-User, the
// client sends X_Forwarded_User, and a backend that merges the two believes
// the client. So every assertion here is signed, it is read only from a
// pinned peer, and every alias of its header is removed from the request
// before anything reads it.
package headergate

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/textproto"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

// ErrUntrustedPeer: the gate's header arrived from a peer outside the gate's
// trusted_proxy_cidrs. It wraps authgate.ErrInvalidCredential: the request
// presented an identity and it is refused, not ignored.
var ErrUntrustedPeer = fmt.Errorf("%w: assertion header from an untrusted peer", authgate.ErrInvalidCredential)

// errAmbiguous: the header appeared more than once. A proxy sets it once; two
// copies mean someone else added one.
var errAmbiguous = fmt.Errorf("%w: assertion header repeated", authgate.ErrInvalidCredential)

// Source pins where a header gate's headers may come from.
type Source struct {
	// Headers are the gate's header names. Compared by alias (see aliasKey),
	// so the form written here does not matter.
	Headers []string
	// Trusted are the peers allowed to send them (the gate's
	// trusted_proxy_cidrs). Empty trusts nobody.
	Trusted []netip.Prefix
}

// Take reads the gate's headers from r. First it removes every alias of each
// header — a name that differs from the canonical one only in case, dashes,
// underscores, dots or any other non-alphanumeric byte — so neither this gate
// nor anything later can read a smuggled copy. Then:
//
//   - none of the headers present: authgate.ErrNoCredential;
//   - present, but the peer is not trusted: the headers are removed as well,
//     and ErrUntrustedPeer;
//   - a header given twice: authgate.ErrInvalidCredential;
//   - otherwise the values, by canonical name (a missing one is "").
//
// The canonical headers stay on a trusted request, so a later check in the
// same request (a session's per-request re-verification) reads the same
// values.
func (s Source) Take(r *http.Request) (map[string]string, error) {
	StripAliases(r.Header, s.Headers...)
	present := false
	for _, h := range s.Headers {
		if len(r.Header.Values(h)) > 0 {
			present = true
		}
	}
	if !present {
		return nil, authgate.ErrNoCredential
	}
	peer, ok := authgate.PeerAddr(r)
	if !ok || !authgate.InPrefixes(peer, s.Trusted) {
		for _, h := range s.Headers {
			r.Header.Del(h)
		}
		return nil, ErrUntrustedPeer
	}
	out := make(map[string]string, len(s.Headers))
	for _, h := range s.Headers {
		vs := r.Header.Values(h)
		if len(vs) > 1 {
			return nil, errAmbiguous
		}
		if len(vs) == 1 {
			out[textproto.CanonicalMIMEHeaderKey(h)] = vs[0]
		}
	}
	return out, nil
}

// StripAliases deletes from h every key that is an alias of one of names but
// not its canonical form. Go's server canonicalises dashed names only, so
// "x-loomcycle-identity" already merged into "X-Loomcycle-Identity" while
// "X_Loomcycle_Identity" and "X.Loomcycle.Identity" arrive as keys of their
// own; a proxy or a library downstream may merge them. They are deleted, not
// merged: nothing legitimate sends them.
func StripAliases(h http.Header, names ...string) {
	if len(names) == 0 {
		return
	}
	want := make(map[string]string, len(names))
	for _, n := range names {
		want[aliasKey(n)] = textproto.CanonicalMIMEHeaderKey(n)
	}
	for k := range h {
		if canon, ok := want[aliasKey(k)]; ok && k != canon {
			delete(h, k)
		}
	}
}

// aliasKey folds a header name to its letters and digits, lower-cased: the
// key under which every spelling a lenient proxy might merge collides.
func aliasKey(name string) string {
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
			b = append(b, c)
		case 'A' <= c && c <= 'Z':
			b = append(b, c+('a'-'A'))
		}
	}
	return string(b)
}
