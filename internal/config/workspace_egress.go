package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// workspace_egress.go — `workspace.egress` (RFC GW §6.3, P4): the named routes
// a workspace may reach the network through, the operator's ceiling on routes
// a tenant writes, and the limits on what passes through a route. Policy, so
// yaml only (RFC GN). This file holds the shape and its checks at load; the
// proxy that enforces a route is internal/workspace/egress.
//
// What load cannot check: that a credentialed route's credential exists and
// carries allowed_urls covering the route's URL. Credentials live in the
// store, which config loads before, so that check is made where the route is
// used (and, for a tenant's route, where it is created).

// WorkspaceEgressConfig is `workspace.egress`.
type WorkspaceEgressConfig struct {
	// Routes are the operator's routes, by name. A name is the first path
	// segment of http://egress/<name>/… inside a session.
	Routes map[string]EgressRoute `yaml:"routes,omitempty"`
	// TenantRoutes lets a tenant operator write routes of its own
	// (EgressRouteDef). Default false.
	TenantRoutes bool `yaml:"tenant_routes,omitempty"`
	// TenantHosts, when set, are the only hosts a tenant's route may name.
	// Unset, any public host. Bare host names; needs TenantRoutes.
	TenantHosts []string `yaml:"tenant_hosts,omitempty"`
	// Limits bound each route per workspace.
	Limits WorkspaceEgressLimits `yaml:"limits,omitempty"`
}

// EgressRoute is one route: an upstream URL, the methods a session may send
// to it, and optionally the stored credential the core adds on the way out.
type EgressRoute struct {
	// URL is https, with no user, query or fragment, written in canonical
	// form and ending in "/". A request's path is joined under it.
	URL string `yaml:"url"`
	// Methods default to GET and HEAD (Resolved). An explicit empty list is
	// refused: it would allow nothing.
	Methods []string `yaml:"methods,omitempty"`
	// Credential is a stored credential, "$cred:<name>", resolved for the
	// run. Never a literal.
	Credential string `yaml:"credential,omitempty"`
	// Auth is how the credential is sent: bearer (the default with a
	// credential), basic, or header.
	Auth string `yaml:"auth,omitempty"`
	// Header names the header the credential is sent in, with auth header.
	Header string `yaml:"header,omitempty"`
}

// WorkspaceEgressLimits is `workspace.egress.limits`, per workspace and route
// (per replica, as the token budgets are).
type WorkspaceEgressLimits struct {
	// RatePerMinute is the requests a minute. Default 600.
	RatePerMinute int `yaml:"rate_per_minute,omitempty"`
	// MaxBytes is the request and response body bytes. Default 4 GiB.
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
}

// How a route's credential is sent.
const (
	EgressAuthBearer = "bearer"
	EgressAuthBasic  = "basic"
	EgressAuthHeader = "header"
)

// Defaults of workspace.egress.limits.
const (
	DefaultWorkspaceEgressRatePerMinute = 600
	DefaultWorkspaceEgressMaxBytes      = 4 << 30
)

// egressMethods are the methods a route may list, in the order a resolved
// default is written.
var egressMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// defaultEgressMethods is what a route that lists none allows.
var defaultEgressMethods = []string{"GET", "HEAD"}

// egressRouteNameRe is a route name: it is a URL path segment and a key a
// person reads in a listing.
var egressRouteNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidEgressRouteName reports whether name may name a route. One rule for
// the operator's routes, a tenant's, and the names an agent grant lists.
func ValidEgressRouteName(name string) bool { return egressRouteNameRe.MatchString(name) }

// Resolved is the block with its defaults filled in, in new values: the
// loaded config is never written to (its route map is shared by every copy of
// the Config).
func (e WorkspaceEgressConfig) Resolved() WorkspaceEgressConfig {
	if e.Routes != nil {
		routes := make(map[string]EgressRoute, len(e.Routes))
		for name, r := range e.Routes {
			routes[name] = r.Resolved()
		}
		e.Routes = routes
	}
	e.TenantHosts = slices.Clone(e.TenantHosts)
	if e.Limits.RatePerMinute == 0 {
		e.Limits.RatePerMinute = DefaultWorkspaceEgressRatePerMinute
	}
	if e.Limits.MaxBytes == 0 {
		e.Limits.MaxBytes = DefaultWorkspaceEgressMaxBytes
	}
	return e
}

// Resolved is the route with its defaults filled in: GET and HEAD when it
// lists no method, bearer when it has a credential and names no auth.
func (r EgressRoute) Resolved() EgressRoute {
	if len(r.Methods) == 0 {
		r.Methods = slices.Clone(defaultEgressMethods)
	} else {
		r.Methods = slices.Clone(r.Methods)
	}
	if r.Credential != "" && r.Auth == "" {
		r.Auth = EgressAuthBearer
	}
	return r
}

// Writes reports whether the resolved route allows a method that changes
// something upstream, for the listings that mark such routes.
func (r EgressRoute) Writes() bool {
	for _, m := range r.Resolved().Methods {
		if m != "GET" && m != "HEAD" && m != "OPTIONS" {
			return true
		}
	}
	return false
}

// validateWorkspaceEgress refuses a route, tenant ceiling or limit §6.3 does
// not allow. Part of validateWorkspace.
func validateWorkspaceEgress(e WorkspaceEgressConfig) error {
	names := make([]string, 0, len(e.Routes))
	for name := range e.Routes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !ValidEgressRouteName(name) {
			return fmt.Errorf("workspace.egress.routes: %q is not a route name (a lowercase letter, then up to 62 lowercase letters, digits or -)", name)
		}
		if err := ValidateEgressRoute(e.Routes[name]); err != nil {
			return fmt.Errorf("workspace.egress.routes.%s.%w", name, err)
		}
	}
	if len(e.TenantHosts) > 0 && !e.TenantRoutes {
		return errors.New("workspace.egress.tenant_hosts: set, but tenant_routes is off, so no tenant route exists for it to bound; set tenant_routes: true or remove the list")
	}
	seen := map[string]bool{}
	for i, h := range e.TenantHosts {
		if !validBareHost(h) {
			return fmt.Errorf("workspace.egress.tenant_hosts[%d]: %q is not a bare host name (lowercase, no scheme, port or path)", i, h)
		}
		if seen[h] {
			return fmt.Errorf("workspace.egress.tenant_hosts[%d]: %q is listed twice", i, h)
		}
		seen[h] = true
	}
	if e.Limits.RatePerMinute < 0 || e.Limits.MaxBytes < 0 {
		return errors.New("workspace.egress.limits: rate_per_minute and max_bytes may not be negative")
	}
	return nil
}

// ValidateEgressRoute checks one route's fields. Its error starts with the
// field (url, methods, credential, auth or header) so a caller can prefix the
// route's place. One function for the operator's routes and, later, a
// tenant's, so a route refused in yaml is not kept by a runtime write.
func ValidateEgressRoute(r EgressRoute) error {
	if r.URL == "" {
		return errors.New("url: required")
	}
	if err := validateEgressRouteURL(r.URL); err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if r.Methods != nil && len(r.Methods) == 0 {
		return errors.New("methods: an empty list allows nothing; remove the key for GET and HEAD")
	}
	seen := map[string]bool{}
	for i, m := range r.Methods {
		if !slices.Contains(egressMethods, m) {
			return fmt.Errorf("methods[%d]: %q is not one of %s", i, m, strings.Join(egressMethods, ", "))
		}
		if seen[m] {
			return fmt.Errorf("methods[%d]: %q is listed twice", i, m)
		}
		seen[m] = true
	}
	if r.Credential != "" {
		if _, ok := credential.ParseRef(r.Credential); !ok {
			// Never quoted: a literal here is a secret.
			return errors.New(`credential takes only a stored credential, written "$cred:<name>": a literal secret would be kept in the config, readable at rest`)
		}
	}
	switch r.Auth {
	case "", EgressAuthBearer, EgressAuthBasic, EgressAuthHeader:
	default:
		return fmt.Errorf("auth: %q is not one of %s, %s, %s", r.Auth, EgressAuthBearer, EgressAuthBasic, EgressAuthHeader)
	}
	if r.Auth != "" && r.Credential == "" {
		return fmt.Errorf("auth: %s needs a credential to send", r.Auth)
	}
	switch {
	case r.Auth == EgressAuthHeader && r.Header == "":
		return errors.New("header: required with auth header, naming the header the credential is sent in")
	case r.Auth != EgressAuthHeader && r.Header != "":
		return errors.New("header: set only with auth header")
	case r.Header != "":
		if !validHeaderName(r.Header) {
			return fmt.Errorf("header: %q is not an HTTP header name", r.Header)
		}
		if reservedEgressHeader(r.Header) {
			return fmt.Errorf("header: %q may not carry a credential: the proxy drops or sets it itself", r.Header)
		}
	}
	return nil
}

// validateEgressRouteURL checks a route URL against the rules a credential's
// allowed_urls entry is held to (credential.NormalizeAllowedURLs: https, no
// user, query, fragment, percent-encoding, backslash, control character or
// dotted segment), and that it is written in that canonical form followed by
// "/", so the route's URL and an allowed_urls entry of it compare equal and
// one upstream is never written two ways.
func validateEgressRouteURL(raw string) error {
	in := []string{raw}
	norm, err := credential.NormalizeAllowedURLs(&in)
	if err != nil {
		if inner := errors.Unwrap(err); inner != nil {
			err = inner // drop the allowed_urls[0] place
		}
		return err
	}
	if !strings.HasSuffix(raw, "/") {
		return fmt.Errorf("%q must end with /", raw)
	}
	if want := (*norm)[0] + "/"; raw != want {
		return fmt.Errorf("%q is not written in its canonical form (want %q)", raw, want)
	}
	return nil
}

// validHeaderName reports whether s is an HTTP field name (RFC 9110 token).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// reservedEgressHeader reports whether a header is one the proxy drops,
// rewrites or frames itself, so a credential sent in it would be lost or
// would change how the request is routed or framed.
func reservedEgressHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade",
		"host", "cookie", "forwarded", "content-length":
		return true
	}
	return strings.HasPrefix(n, "proxy-") || strings.HasPrefix(n, "x-forwarded-")
}

// validBareHost reports whether h is a lowercase DNS name or IPv4 address,
// with no scheme, port or path.
func validBareHost(h string) bool {
	if h == "" || len(h) > 253 || h[0] == '.' || h[len(h)-1] == '.' || strings.Contains(h, "..") {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (e *WorkspaceEgressConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "workspace.egress", nil, reflect.TypeOf(*e)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "limits"), "workspace.egress.limits", nil, reflect.TypeOf(WorkspaceEgressLimits{})); err != nil {
		return err
	}
	if routes := valueOf(n, "routes"); routes != nil && routes.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(routes.Content); i += 2 {
			place := "workspace.egress.routes." + routes.Content[i].Value
			if err := strictKeys(routes.Content[i+1], place, nil, reflect.TypeOf(EgressRoute{})); err != nil {
				return err
			}
		}
	}
	type plain WorkspaceEgressConfig
	return n.Decode((*plain)(e))
}
