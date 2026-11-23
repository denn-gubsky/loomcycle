package egress

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// routeName is the syntax of a route's name: the first path segment of the
// session's URL, so nothing that needs escaping.
var routeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// The methods a route may allow, and those it allows when it lists none.
var (
	allowedMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	defaultMethods = []string{"GET", "HEAD"}
)

// ValidName reports whether name is a valid route name.
func ValidName(name string) bool { return routeName.MatchString(name) }

// CheckRoute checks a route as written: its name, its URL, its methods and how
// it sends its credential. Every author of a route calls it, and the proxy
// calls it again before each use, so a route that passed an older check is
// not trusted because it is stored.
func CheckRoute(rt Route) error {
	_, err := checkRoute(rt)
	return err
}

func checkRoute(rt Route) (*url.URL, error) {
	if !ValidName(rt.Name) {
		return nil, fmt.Errorf("name %q is not a route name (a lowercase letter, then letters, digits and -, at most 63)", safeToken(rt.Name))
	}
	u, err := checkRouteURL(rt.URL)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range rt.Methods {
		if !slices.Contains(allowedMethods, m) {
			return nil, fmt.Errorf("method %q is not one a route may allow (%s)", safeToken(m), strings.Join(allowedMethods, ", "))
		}
		if seen[m] {
			return nil, fmt.Errorf("method %s is listed twice", m)
		}
		seen[m] = true
	}
	switch rt.Auth {
	case "", AuthBearer, AuthBasic:
		if rt.Header != "" {
			return nil, errors.New("header is set but auth is not header")
		}
	case AuthHeader:
		if err := CheckAuthHeader(rt.Header); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("auth %q is not one of bearer, basic, header", safeToken(rt.Auth))
	}
	if rt.Auth != "" && rt.Credential == "" {
		return nil, errors.New("auth is set but no credential is named")
	}
	return u, nil
}

// checkRouteURL checks a route's URL: https, a host, no user, query or
// fragment, no dot segment, no percent-encoding, backslash, space or control
// character, and a path ending in "/" so that a request's path is joined
// under it rather than beside it.
func checkRouteURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("url is empty")
	}
	if len(raw) > 512 {
		return nil, errors.New("url is longer than 512 characters")
	}
	for _, c := range raw {
		if c <= 0x20 || c == 0x7f || c == '\\' || c == '%' {
			return nil, errors.New("url has a space, a control character, a backslash or a percent-encoding")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("url is not a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return nil, errors.New("url must be https")
	case u.User != nil:
		return nil, errors.New("url may not carry a user or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, errors.New("url may not carry a query or a fragment")
	case u.Hostname() == "":
		return nil, errors.New("url has no host")
	case !strings.HasSuffix(u.Path, "/"):
		return nil, errors.New("url must end in /")
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for _, s := range segs {
		if s == "." || s == ".." {
			return nil, errors.New("url may not have a . or .. path segment")
		}
	}
	if u.Path != "/" && slices.Contains(segs, "") {
		return nil, errors.New("url may not have an empty path segment")
	}
	return u, nil
}

// forbiddenAuthHeaders may not carry a route's credential: the proxy drops or
// sets them itself, or they would split, route or misframe the request.
var forbiddenAuthHeaders = []string{
	"Host", "Cookie", "Set-Cookie", "Forwarded", "Content-Length", "Content-Type",
	"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// CheckAuthHeader checks the name of the header a route sends its credential
// in.
func CheckAuthHeader(name string) error {
	if name == "" {
		return errors.New("auth header needs header: the header's name")
	}
	if len(name) > 64 {
		return errors.New("header name is longer than 64 characters")
	}
	for _, c := range name {
		if !isTokenChar(c) {
			return fmt.Errorf("header %q is not a valid header name", safeToken(name))
		}
	}
	canon := strings.ToLower(name)
	for _, f := range forbiddenAuthHeaders {
		if canon == strings.ToLower(f) {
			return fmt.Errorf("header %s may not carry a credential", name)
		}
	}
	if strings.HasPrefix(canon, "proxy-") || strings.HasPrefix(canon, "x-forwarded-") {
		return fmt.Errorf("header %s may not carry a credential", name)
	}
	return nil
}

// isTokenChar reports whether c may appear in an HTTP token (RFC 9110 §5.6.2).
func isTokenChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

func methodsOf(rt Route) []string {
	if len(rt.Methods) == 0 {
		return defaultMethods
	}
	return rt.Methods
}

// methodAllowed reports whether the route allows method, compared exactly: a
// lower-case "get" is not GET to every upstream.
func methodAllowed(rt Route, method string) bool {
	return slices.Contains(methodsOf(rt), method)
}

// joinPath joins a session's path, with its query, under a route's URL. It
// refuses anything that could make the request leave the route's URL, or be
// read two ways by the upstream (checkSegment); an empty segment; a leading
// slash; a backslash, a fragment, a space or a control character. The joined
// URL is checked once more to be under the route's URL on the route's host.
func joinPath(base *url.URL, p string) (*url.URL, error) {
	if len(p) > 8192 {
		return nil, errors.New("the path is longer than 8192 characters")
	}
	for _, c := range p {
		if c <= 0x20 || c == 0x7f || c == '\\' || c == '#' {
			return nil, errors.New("the path has a space, a control character, a backslash or a fragment")
		}
	}
	rawPath, rawQuery, hasQuery := strings.Cut(p, "?")
	if strings.HasPrefix(rawPath, "/") {
		return nil, errors.New("the path may not start with /")
	}
	if rawPath != "" {
		segs := strings.Split(rawPath, "/")
		for i, seg := range segs {
			if seg == "" {
				if i == len(segs)-1 {
					continue // a trailing slash
				}
				return nil, errors.New("the path may not have an empty segment")
			}
			if err := checkSegment(seg); err != nil {
				return nil, err
			}
		}
	}
	s := base.String() + rawPath
	if hasQuery {
		s += "?" + rawQuery
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("the path does not make a valid URL")
	}
	if u.Scheme != "https" || u.User != nil || u.Host != base.Host || u.Fragment != "" ||
		!strings.HasPrefix(u.EscapedPath(), base.EscapedPath()) {
		return nil, errors.New("the path leaves the route's URL")
	}
	return u, nil
}

// segmentChars are the characters a path segment may carry as written: RFC
// 3986's unreserved characters, the sub-delims but ";", ":" and "@" (npm's
// scoped names), and "%" to start a percent-encoding. ";" is left out
// because Tomcat and Spring cut a segment at it ("..;x" reaches them as
// ".."), and the rest of what a URL may hold is left out because some server
// somewhere reads it as something else.
const segmentChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~!$&'()*+,=:@%"

// checkSegment refuses a path segment an upstream could read as another path
// than the one checked here (a parser differential): a character outside
// segmentChars; a percent-encoding that is malformed, or decodes to anything
// but valid UTF-8 (an over-long "%c0%ae" is "." to some servers), to a slash,
// a backslash, a ";", a control byte, or another "%" (a double encoding); and
// a segment that is "." or ".." once decoded.
func checkSegment(seg string) error {
	for i := 0; i < len(seg); i++ {
		if strings.IndexByte(segmentChars, seg[i]) < 0 {
			return fmt.Errorf("the path holds %q, which a route's path may not carry", seg[i:i+1])
		}
	}
	dec, err := url.PathUnescape(seg)
	if err != nil {
		return errors.New("the path has a malformed percent-encoding")
	}
	if !utf8.ValidString(dec) {
		return errors.New("the path has a percent-encoding that is not valid UTF-8")
	}
	for _, c := range dec {
		switch {
		case c == '/' || c == '\\':
			return errors.New("the path may not hold an encoded slash or backslash")
		case c == ';':
			return errors.New("the path may not hold a ; (some servers cut a segment at it)")
		case c == '%':
			return errors.New("the path may not hold a percent-encoded percent sign")
		case c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0):
			return errors.New("the path may not hold an encoded control character")
		}
	}
	if dec == "." || dec == ".." {
		return errors.New("the path may not have a . or .. segment")
	}
	return nil
}

// pathOnly is a session's path without its query, bounded, for an audit line.
func pathOnly(p string) string {
	p, _, _ = strings.Cut(p, "?")
	return safeText(p, 512)
}

// safeToken bounds a short value the session wrote before it goes into a
// reason or a log.
func safeToken(s string) string { return safeText(s, 64) }

func safeText(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
