package credential

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// A credential may carry a list of URLs it is allowed to be sent to. The list
// answers one question a secret store otherwise cannot: when the destination
// of a request comes from somewhere untrusted (a repository URL in a webhook
// body, a server address an agent wrote), is this the place the secret was
// stored FOR?
//
// Three states, and the difference between the last two matters:
//
//	no list        the credential is unrestricted, as every credential was
//	               before the field existed
//	an empty list  the credential is sent nowhere; it can still be compared
//	               against (a webhook signing secret)
//	a list         the credential is sent only to a URL under one entry

// Limits on a list, so it stays something a person can read in a listing.
const (
	maxAllowedURLs      = 32
	maxAllowedURLLength = 512
)

// ErrRestricted is returned when a credential that carries a URL restriction
// is asked for by a caller that does not say where the secret is going.
var ErrRestricted = errors.New("credential: this credential is restricted to specific URLs and cannot be used here")

// ErrURLNotAllowed is returned when the destination is not under any entry of
// the credential's list.
var ErrURLNotAllowed = errors.New("credential: the destination URL is not one this credential is allowed to be sent to")

// ErrNotPinnable is returned when a restricted credential is asked for with a
// context the request cannot be pinned on (netguard.WithPinnable): nothing
// would then stop a redirect from carrying it to another URL.
var ErrNotPinnable = errors.New("credential: this credential is restricted to specific URLs, and the request would not be kept from following a redirect")

// NormalizeAllowedURLs checks a list as written and returns its stored form:
// every entry canonical, the list sorted and without duplicates. nil stays
// nil (no restriction); an empty list stays empty (sent nowhere).
func NormalizeAllowedURLs(in *[]string) (*[]string, error) {
	if in == nil {
		return nil, nil
	}
	if len(*in) > maxAllowedURLs {
		return nil, fmt.Errorf("allowed_urls: at most %d entries (got %d)", maxAllowedURLs, len(*in))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(*in))
	for i, raw := range *in {
		c, err := canonicalURL(raw)
		if err != nil {
			return nil, fmt.Errorf("allowed_urls[%d]: %w", i, err)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return &out, nil
}

// URLAllowed reports whether target is under one entry of an allowed list. A
// nil list allows everything; an empty one nothing. A target that is not a
// clean https URL is never allowed.
func URLAllowed(allowed *[]string, target string) bool {
	if allowed == nil {
		return true
	}
	t, err := canonicalURL(target)
	if err != nil {
		return false
	}
	for _, entry := range *allowed {
		if entry == t || strings.HasPrefix(t, entry+"/") {
			return true
		}
	}
	return false
}

// canonicalURL reduces a URL to the form entries and targets are compared in:
// "https://host[:port]" followed by its path segments, with no trailing slash
// and no ".git" on the last one. Comparing canonical forms segment by segment
// is what makes https://h/acme/app match https://h/acme/app.git and its
// sub-paths, and not https://h/acme/app-fork or https://h/acme.
//
// It refuses anything that could make two different destinations compare
// equal, or one destination be written two ways: another scheme, a user or
// password, a query, a fragment, an encoded or dotted path segment.
func canonicalURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty URL")
	}
	if len(raw) > maxAllowedURLLength {
		return "", fmt.Errorf("URL longer than %d characters", maxAllowedURLLength)
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f || r == '\\' || r == '%' {
			return "", errors.New("URL has a space, a control character, a backslash or a percent-encoding")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if u.Scheme != "https" {
		return "", errors.New("only https URLs are allowed")
	}
	if u.User != nil {
		return "", errors.New("a URL with a user or password is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("a URL with a query or a fragment is not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("the URL has no host")
	}
	// JoinHostPort, and brackets on an IPv6 host without a port: written
	// bare, the address 2001:db8::5:8080 and 2001:db8::5 on port 8080 would
	// compare equal.
	if port := u.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	var segs []string
	for _, seg := range strings.Split(u.Path, "/") {
		switch seg {
		case "":
			continue // leading, trailing or doubled slash
		case ".", "..":
			return "", errors.New("a URL with a . or .. path segment is not allowed")
		}
		segs = append(segs, seg)
	}
	if n := len(segs); n > 0 {
		if trimmed := strings.TrimSuffix(segs[n-1], ".git"); trimmed != "" {
			segs[n-1] = trimmed
		}
	}
	out := "https://" + host
	if len(segs) > 0 {
		out += "/" + strings.Join(segs, "/")
	}
	return out, nil
}

// boundURLs renders a restriction as the bytes the secret is sealed together
// with (Identity.Bound). nil gives nil, so an unrestricted credential seals
// as it always did. An empty list gives a non-empty marker: "sent nowhere"
// must not seal the same as "no restriction".
func boundURLs(allowed *[]string) []byte {
	if allowed == nil {
		return nil
	}
	// Entries are canonical URLs, which cannot contain the separator.
	return []byte("urls\x1e" + strings.Join(*allowed, "\x1e"))
}

// maxUsernameLen bounds a stored user name.
const maxUsernameLen = 256

// ValidUsername checks a user name to store with a credential. It goes into
// HTTP basic auth as "<username>:<value>", so it may not hold a colon, nor a
// control character (a header could be split by one). Empty is valid: it
// means none.
func ValidUsername(u string) error {
	if len(u) > maxUsernameLen {
		return fmt.Errorf("credential: username is longer than %d bytes", maxUsernameLen)
	}
	for _, r := range u {
		if r == ':' {
			return errors.New("credential: username may not contain ':'")
		}
		if r < 0x20 || r == 0x7f {
			return errors.New("credential: username may not contain a control character")
		}
	}
	return nil
}
