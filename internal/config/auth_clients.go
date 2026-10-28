package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// RFC FL: loomcycle's own apps sign people in through loomcycle's gates, as
// clients of an OAuth 2.0 authorization server loomcycle runs.
//
//	auth:
//	  clients:
//	    loomboard:
//	      kind: public                                   # no secret: PKCE S256
//	      redirect_uris:
//	        - https://board.example.com/auth/callback    # exact match
//	        - http://127.0.0.1/callback                  # loopback: any port
//	        - loomboard://auth/callback                  # private-use scheme
//	      allowed_origins: [https://board.example.com]   # CORS on token/revoke
//	      access_token_ttl: 1h
//	      refresh: { idle: 7d, absolute: 30d }
//	      restrictions: {}                               # narrows every token
//
// A client is also authored at runtime as a ClientDef, with the same fields;
// ResolveAppClient validates both, so a runtime client can never be looser
// than one an operator could write here.

// AppClientKindPublic is the only client kind: a public client holds no
// secret, so PKCE is what binds a code to the app that asked for it.
const AppClientKindPublic = "public"

// App client lifetimes (RFC FL D4) and their bounds.
const (
	DefaultAppAccessTTL       = time.Hour
	DefaultAppRefreshIdle     = 7 * 24 * time.Hour
	DefaultAppRefreshAbsolute = 30 * 24 * time.Hour
	minAppAccessTTL           = time.Minute
	maxAppAccessTTL           = 24 * time.Hour
	maxAppRefreshAbsolute     = 365 * 24 * time.Hour
	maxAppRedirectURIs        = 32
	maxAppAllowedOrigins      = 32
	maxAppRedirectURILen      = 2048
	maxAppLabelLen            = 64
)

// appClientIDRE is a client id: it travels in every authorize URL and names
// the access token (u-app-<id>), so it is a short slug.
var appClientIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// appSchemeRE is a URI scheme (RFC 3986 §3.1), lowercased.
var appSchemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]{1,63}$`)

// browserSchemes are schemes a browser or the OS gives a meaning of its own.
// A private-use redirect scheme names an app (RFC 8252 §7.1); one of these
// would hand the code to something that is not the app at all.
var browserSchemes = map[string]bool{
	"javascript": true, "data": true, "file": true, "blob": true, "about": true, "vbscript": true,
	"filesystem": true, "view-source": true, "ftp": true, "ws": true, "wss": true, "mailto": true,
	"tel": true, "sms": true, "intent": true, "content": true, "chrome": true, "edge": true,
}

// AuthClientConfig is one entry of `auth.clients`, and the body of a
// ClientDef.
type AuthClientConfig struct {
	Kind           string            `yaml:"kind" json:"kind"`
	Label          string            `yaml:"label,omitempty" json:"label,omitempty"`
	RedirectURIs   []string          `yaml:"redirect_uris" json:"redirect_uris"`
	AllowedOrigins []string          `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty"`
	AccessTokenTTL string            `yaml:"access_token_ttl,omitempty" json:"access_token_ttl,omitempty"`
	Refresh        AuthClientRefresh `yaml:"refresh,omitempty" json:"refresh,omitempty"`
	// Restrictions narrow every token the client's sign-ins mint (the RFC EF
	// record a token carries). A map so yaml and JSON spell it the same way
	// the token mint surfaces do; it is checked with auth.ParseRestrictions.
	Restrictions map[string]any `yaml:"restrictions,omitempty" json:"restrictions,omitempty"`
}

// AuthClientRefresh is a client's refresh-token lifetimes: Go durations or
// whole days ("7d").
type AuthClientRefresh struct {
	Idle     string `yaml:"idle,omitempty" json:"idle,omitempty"`
	Absolute string `yaml:"absolute,omitempty" json:"absolute,omitempty"`
}

// AppClient is a validated client.
type AppClient struct {
	ID             string
	Label          string
	RedirectURIs   []string
	AllowedOrigins []string
	AccessTTL      time.Duration
	RefreshIdle    time.Duration
	RefreshAbs     time.Duration
	// RestrictionsJSON is the client's restrictions record, "" for none.
	RestrictionsJSON string
	// TenantID confines the client to one tenant's users (RFC FL D10); ""
	// serves every tenant. A config client always serves every tenant.
	TenantID string
	// DefID is the ClientDef version this client was read from, "" for a
	// config client.
	DefID string
}

// FromConfig reports whether the client is declared in config (and so may
// not be changed or retired through the API).
func (c AppClient) FromConfig() bool { return c.DefID == "" }

// ServesTenant reports whether the client may sign in a user of tenant.
func (c AppClient) ServesTenant(tenant string) bool {
	return c.TenantID == "" || c.TenantID == tenant
}

// MatchRedirectURI reports whether uri is one of the client's redirect URIs:
// an exact string match, except that a loopback http URI matches on any port
// with everything else exact (RFC 8252 §7.3, where a desktop app listens on a
// port the OS gives it).
func (c AppClient) MatchRedirectURI(uri string) bool {
	_, ok := c.RegisteredRedirectURI(uri)
	return ok
}

// RegisteredRedirectURI is the redirect target for uri, built from the
// client's own registration rather than the caller's string: the registered
// URI itself, or for a loopback URI the registered one with the caller's port
// re-written from its parsed number. A uri that does not rebuild to exactly
// itself (an odd port spelling, say) does not match, so the target always
// equals what the app sent and is never the app's input.
func (c AppClient) RegisteredRedirectURI(uri string) (string, bool) {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return r, true
		}
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
		return "", false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	for _, r := range c.RedirectURIs {
		ru, err := url.Parse(r)
		if err != nil || ru.Scheme != "http" {
			continue
		}
		if ru.Hostname() == u.Hostname() && ru.EscapedPath() == u.EscapedPath() && ru.RawQuery == u.RawQuery &&
			ru.User == nil && u.User == nil && u.Fragment == "" && !strings.Contains(uri, "#") {
			target := *ru
			target.Host = net.JoinHostPort(ru.Hostname(), strconv.Itoa(port))
			if out := target.String(); out == uri {
				return out, true
			}
			return "", false
		}
	}
	return "", false
}

// OriginAllowed reports whether a browser origin may call the token and
// revoke endpoints for this client (CORS).
func (c AppClient) OriginAllowed(origin string) bool {
	for _, o := range c.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// ResolveAppClient validates one client's fields. where prefixes errors.
func ResolveAppClient(where, id string, cc AuthClientConfig) (AppClient, error) {
	if !appClientIDRE.MatchString(id) {
		return AppClient{}, fmt.Errorf("%s: invalid client id %q (lowercase letters, digits, - and _, at most 48)", where, id)
	}
	switch strings.ToLower(strings.TrimSpace(cc.Kind)) {
	case AppClientKindPublic:
	case "":
		return AppClient{}, fmt.Errorf("%s: kind is required (public)", where)
	case "confidential":
		return AppClient{}, fmt.Errorf("%s: kind confidential is not supported in this release yet; only public clients (PKCE) are", where)
	default:
		return AppClient{}, fmt.Errorf("%s: unknown kind %q (want public)", where, cc.Kind)
	}
	c := AppClient{ID: id, Label: strings.TrimSpace(cc.Label)}
	if c.Label == "" {
		c.Label = id
	}
	if len(c.Label) > maxAppLabelLen || strings.ContainsFunc(c.Label, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return AppClient{}, fmt.Errorf("%s.label: at most %d characters and no control characters", where, maxAppLabelLen)
	}
	if len(cc.RedirectURIs) == 0 {
		return AppClient{}, fmt.Errorf("%s.redirect_uris: at least one redirect URI is required", where)
	}
	if len(cc.RedirectURIs) > maxAppRedirectURIs {
		return AppClient{}, fmt.Errorf("%s.redirect_uris: at most %d", where, maxAppRedirectURIs)
	}
	seen := map[string]bool{}
	for _, r := range cc.RedirectURIs {
		if err := ValidateAppRedirectURI(r); err != nil {
			return AppClient{}, fmt.Errorf("%s.redirect_uris: %w", where, err)
		}
		if !seen[r] {
			seen[r] = true
			c.RedirectURIs = append(c.RedirectURIs, r)
		}
	}
	if len(cc.AllowedOrigins) > maxAppAllowedOrigins {
		return AppClient{}, fmt.Errorf("%s.allowed_origins: at most %d", where, maxAppAllowedOrigins)
	}
	for _, o := range cc.AllowedOrigins {
		if err := validateAppOrigin(o); err != nil {
			return AppClient{}, fmt.Errorf("%s.allowed_origins: %w", where, err)
		}
		c.AllowedOrigins = append(c.AllowedOrigins, o)
	}
	var err error
	if c.AccessTTL, err = appDuration(where+".access_token_ttl", cc.AccessTokenTTL, DefaultAppAccessTTL); err != nil {
		return AppClient{}, err
	}
	if c.AccessTTL < minAppAccessTTL || c.AccessTTL > maxAppAccessTTL {
		return AppClient{}, fmt.Errorf("%s.access_token_ttl: must be between %s and %s", where, minAppAccessTTL, maxAppAccessTTL)
	}
	if c.RefreshIdle, err = appDuration(where+".refresh.idle", cc.Refresh.Idle, DefaultAppRefreshIdle); err != nil {
		return AppClient{}, err
	}
	if c.RefreshAbs, err = appDuration(where+".refresh.absolute", cc.Refresh.Absolute, DefaultAppRefreshAbsolute); err != nil {
		return AppClient{}, err
	}
	if c.RefreshAbs > maxAppRefreshAbsolute {
		return AppClient{}, fmt.Errorf("%s.refresh.absolute: at most %s", where, maxAppRefreshAbsolute)
	}
	if c.RefreshIdle > c.RefreshAbs {
		return AppClient{}, fmt.Errorf("%s.refresh: idle (%s) is longer than absolute (%s)", where, c.RefreshIdle, c.RefreshAbs)
	}
	if c.AccessTTL > c.RefreshIdle {
		return AppClient{}, fmt.Errorf("%s: access_token_ttl (%s) is longer than refresh.idle (%s)", where, c.AccessTTL, c.RefreshIdle)
	}
	if len(cc.Restrictions) > 0 {
		b, err := json.Marshal(cc.Restrictions)
		if err != nil {
			return AppClient{}, fmt.Errorf("%s.restrictions: %w", where, err)
		}
		r, err := auth.ParseRestrictions(string(b))
		if err != nil {
			return AppClient{}, fmt.Errorf("%s.restrictions: %w", where, err)
		}
		if err := r.Validate(r.Agents != nil); err != nil {
			return AppClient{}, fmt.Errorf("%s.restrictions: %w", where, err)
		}
		c.RestrictionsJSON = string(b)
	}
	return c, nil
}

func appDuration(where, v string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := parseLifecycleAge(where, v, false)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive", where)
	}
	return d, nil
}

// ValidateAppRedirectURI checks one redirect URI against RFC FL §3: https with
// a host; http only to a loopback IP literal (127.0.0.1 or [::1]), whose port
// is not part of the match; or a private-use scheme that names an app (RFC
// 8252 §7.1). Never a fragment, never a wildcard, never userinfo.
func ValidateAppRedirectURI(v string) error {
	if v == "" || len(v) > maxAppRedirectURILen {
		return fmt.Errorf("a redirect URI must be 1 to %d characters", maxAppRedirectURILen)
	}
	for _, r := range v {
		if r <= 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("%q: no spaces, control characters or backslashes", v)
		}
	}
	if strings.Contains(v, "*") {
		return fmt.Errorf("%q: a redirect URI is matched exactly; wildcards are not allowed", v)
	}
	if strings.Contains(v, "#") {
		return fmt.Errorf("%q: a redirect URI may not have a fragment", v)
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("%q: %w", v, err)
	}
	if u.User != nil {
		return fmt.Errorf("%q: a redirect URI may not carry userinfo", v)
	}
	// url.Parse lowercases the scheme; the exact-match comparison at the
	// authorize step does not, so a mixed-case scheme could never match.
	scheme := u.Scheme
	if !strings.HasPrefix(v, scheme+":") || !appSchemeRE.MatchString(scheme) {
		return fmt.Errorf("%q: needs a lowercase scheme", v)
	}
	switch scheme {
	case "https":
		if u.Hostname() == "" || u.Opaque != "" {
			return fmt.Errorf("%q: an https redirect URI needs a host", v)
		}
		return nil
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("%q: http is allowed only to a loopback address (http://127.0.0.1/… or http://[::1]/…); use https", v)
		}
		return nil
	}
	if browserSchemes[scheme] {
		return fmt.Errorf("%q: scheme %s is not an app's; use https, a loopback http URI or a private-use scheme", v, scheme)
	}
	// A private-use scheme: either a reversed domain name the app's owner
	// controls (com.example.app:/callback), or a single-label scheme the app
	// registered with the OS (loomboard://auth/callback). Either way the URI
	// must say where in the app the code lands.
	if u.Opaque == "" && u.Path == "" && u.Host == "" {
		return fmt.Errorf("%q: a private-use redirect URI needs a path", v)
	}
	return nil
}

// validateAppOrigin checks a CORS origin: scheme://host[:port] with nothing
// after it, https or a loopback http origin, or an extension's origin.
func validateAppOrigin(o string) error {
	if o == "" || o == "null" || strings.Contains(o, "*") {
		return fmt.Errorf("%q: an origin is matched exactly; not empty, null or a wildcard", o)
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		strings.HasSuffix(o, "/") {
		return fmt.Errorf("%q: an origin is scheme://host[:port], nothing after it", o)
	}
	switch u.Scheme {
	case "https", "chrome-extension", "moz-extension", "tauri":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) || u.Hostname() == "localhost" || strings.HasSuffix(u.Hostname(), ".localhost") {
			return nil
		}
		return fmt.Errorf("%q: an http origin must be loopback; use https", o)
	}
	return fmt.Errorf("%q: origin scheme %s is not allowed (https, a loopback http origin or an extension's)", o, u.Scheme)
}

// isLoopbackHost reports whether host is a loopback IP literal. A name
// ("localhost") is not: RFC 8252 §8.3 warns it may resolve elsewhere.
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// resolveAuthClients validates c.Auth.Clients into p.Clients, sorted by id.
func resolveAuthClients(c *Config, p *AuthPolicy) error {
	ids := make([]string, 0, len(c.Auth.Clients))
	for id := range c.Auth.Clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cl, err := ResolveAppClient("auth.clients."+id, id, c.Auth.Clients[id])
		if err != nil {
			return err
		}
		p.Clients = append(p.Clients, cl)
	}
	return nil
}

// Client returns the config client with that id.
func (p AuthPolicy) Client(id string) (AppClient, bool) {
	for _, c := range p.Clients {
		if c.ID == id {
			return c, true
		}
	}
	return AppClient{}, false
}
