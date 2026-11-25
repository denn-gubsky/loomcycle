// Package egress is the core's side of a workspace's way out (RFC GW §6.3).
//
// A workspace has no network of its own. What it may reach is a list of
// routes, each an upstream URL and optionally a credential. A command in the
// workspace's session asks the worker's supervisor for http://egress/<route>/…;
// the supervisor describes the request to the core; the core decides whether
// to send it, builds the upstream request itself, adds the credential and
// streams the answer back. The worker never sees a URL it was not granted, nor
// a credential.
//
// This package is that decision and that request. It knows nothing of the
// worker protocol, the store or the config: the caller hands it the routes the
// workspace holds (a Grant) and the request the worker described.
package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// How a route sends its credential.
const (
	AuthBearer = "bearer" // Authorization: Bearer <value>
	AuthBasic  = "basic"  // Authorization: Basic <username>:<value>
	AuthHeader = "header" // <Route.Header>: <value>
)

// DefaultBase is the address a session reaches the routes at.
const DefaultBase = "http://egress/"

// Route is one way out: an upstream URL and, optionally, the credential the
// core adds on the way. Written by a person, never by a model: the operator
// in yaml, or a tenant operator through EgressRouteDef.
type Route struct {
	// Name is the route's first path segment under the base.
	Name string
	// URL is the upstream prefix: https, no user, query or fragment,
	// ending in "/". A request's path is joined under it and never leaves
	// it.
	URL string
	// Methods are the HTTP methods the route allows; none means GET and
	// HEAD.
	Methods []string
	// Credential is the name of the credential to send, resolved for the
	// run; "" sends none.
	Credential string
	// Auth is how the credential is sent: AuthBearer ("" too), AuthBasic or
	// AuthHeader.
	Auth string
	// Header is the header the credential goes in, for AuthHeader.
	Header string
	// Tenant marks a route a tenant wrote. Its answers are never rewritten
	// and every request through it is audited.
	Tenant bool
}

// Grant is what the core recorded for one workspace: whose it is, for the
// credential and the audit, and the routes it may use.
type Grant struct {
	TenantID    string
	AgentName   string
	UserID      string
	RunID       string
	WorkspaceID string
	Routes      []Route
	// RelativeBase makes rewritten links and relayed redirects relative to
	// the page asked for, in place of the proxy's base: for a workspace that
	// reaches its routes at an address the core does not know (the host
	// engine's loopback port and secret).
	RelativeBase bool
}

// Request is the request the worker described, after the route's name.
type Request struct {
	// Route is the route's name.
	Route string
	// Method is the HTTP method.
	Method string
	// Path is the rest of the session's URL after "/<route>/", with its
	// query: "simple/numpy/?format=json".
	Path string
	// Header is the session's request header. The proxy drops what it
	// must not pass.
	Header http.Header
	// Body is the request body; nil for none.
	Body io.Reader
	// ContentLength is the body's length, -1 when unknown. 0 with a
	// non-nil Body is taken as unknown.
	ContentLength int64
}

// Response is the upstream's answer, cleaned. The caller must close Body;
// closing it writes the audit line.
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// Refusal is a request the core will not send, or an answer it will not
// pass. Reason is one line, safe to show the session: it never carries a
// credential.
type Refusal struct {
	Status int
	Reason string
}

func (r *Refusal) Error() string { return fmt.Sprintf("egress refused (%d): %s", r.Status, r.Reason) }

func refuse(status int, format string, a ...any) *Refusal {
	return &Refusal{Status: status, Reason: fmt.Sprintf(format, a...)}
}

// ErrByteBudget ends an answer cut at the route's byte budget.
var ErrByteBudget = errors.New("egress: the route's byte budget for this workspace is spent")

// Credentials resolves a credential for a run, for one destination.
// *credential.Engine satisfies it.
type Credentials interface {
	ResolveFor(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (credential.Resolved, bool, error)
}

// Line is one audit line: a request through a credentialed or tenant-written
// route. It never holds the query, a header or a body.
type Line struct {
	TenantID    string `json:"tenant_id"`
	RunID       string `json:"run_id"`
	WorkspaceID string `json:"workspace_id"`
	Route       string `json:"route"`
	Method      string `json:"method"`
	// Path is the request's path under the route, without its query.
	Path string `json:"path"`
	// Status is the upstream's status, or the refusal's.
	Status int `json:"status"`
	// Bytes counts the request and answer bodies that passed.
	Bytes int64 `json:"bytes"`
	// Refused is the refusal's reason, when the core refused.
	Refused string `json:"refused,omitempty"`
}

// Options configures a Proxy.
type Options struct {
	// Credentials resolves a route's credential. Required when a route
	// names one; a nil Credentials refuses every credentialed route.
	Credentials Credentials
	// PrivateHostAllowlist is the operator's vouch list for private
	// addresses (LOOMCYCLE_PRIVATE_HOST_ALLOWLIST). Every other private,
	// loopback, link-local or metadata address is refused at dial.
	PrivateHostAllowlist []string
	// TLSConfig is the client's TLS configuration; nil for the system
	// roots.
	TLSConfig *tls.Config
	// Audit receives one line per audited request. Nil writes none.
	Audit func(Line)
	// Limits bound each workspace's use of each route; zero fields take
	// the defaults.
	Limits Limits
	// Base is the address the session reaches routes at, for rewritten
	// links and redirects. Default DefaultBase.
	Base string
	// PyPIFilesRoute is the operator's route to PyPI's file host
	// (PyPIFilesRoute of the operator's routes), "" for none. PyPI's links
	// are rewritten to it whether the workspace holds it or not, so a
	// workspace without it is refused naming the route (P4 plan O34).
	PyPIFilesRoute string
	// CratesDLRoute is the operator's route to the crates download host
	// (CratesDLRoute of the operator's routes), "" for none: the crates
	// index's config.json is pointed at it whether the workspace holds it or
	// not, as PyPIFilesRoute (RFC GW P11).
	CratesDLRoute string
	// ResponseHeaderTimeout bounds the wait for the upstream's answer.
	// Default 60 s. The body has no time bound: the caller's context
	// ends it.
	ResponseHeaderTimeout time.Duration
	// Now is the clock; default time.Now.
	Now func() time.Time

	// dial replaces the guarded dialer, in this package's tests only.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Proxy sends workspaces' requests out. Safe for concurrent use.
type Proxy struct {
	client *http.Client
	creds  Credentials
	audit  func(Line)
	limits Limits
	base   string
	// filesRoute is Options.PyPIFilesRoute, dlRoute Options.CratesDLRoute.
	filesRoute string
	dlRoute    string
	now        func() time.Time

	mu    sync.Mutex
	usage map[usageKey]*usage
}

// New builds a Proxy.
func New(o Options) (*Proxy, error) {
	base := o.Base
	if base == "" {
		base = DefaultBase
	}
	if !strings.HasSuffix(base, "/") {
		return nil, errors.New("egress: base must end in /")
	}
	dial := o.dial
	if dial == nil {
		dial = netguard.GuardedDialContext(false, o.PrivateHostAllowlist)
	}
	rht := o.ResponseHeaderTimeout
	if rht <= 0 {
		rht = 60 * time.Second
	}
	var tlsConf *tls.Config
	if o.TLSConfig != nil {
		tlsConf = o.TLSConfig.Clone()
	}
	tr := &http.Transport{
		// No Proxy: an HTTP(S)_PROXY in the core's environment must not
		// carry a workspace's credential through a host nobody vouched for.
		Proxy:                 nil,
		DialContext:           dial,
		TLSClientConfig:       tlsConf,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: rht,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Proxy{
		client: &http.Client{
			Transport: tr,
			// Never followed here: a redirect is relayed to the session
			// when it lands on another route the workspace holds, and
			// refused otherwise (redirect in Do). So a credential goes
			// only to the route it was resolved for.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		creds:  o.Credentials,
		audit:  o.Audit,
		limits: o.Limits.resolved(),
		base:   base,
		// Checked as a route name, so it cannot write anything but a
		// route's first segment into a link.
		filesRoute: validFilesRoute(o.PyPIFilesRoute),
		dlRoute:    validFilesRoute(o.CratesDLRoute),
		now:        now,
		usage:      map[usageKey]*usage{},
	}, nil
}

// Do sends one request of a workspace, or refuses it with a *Refusal before
// any network call. Any other error is the upstream's (a dial, TLS or
// transport failure) and carries no credential.
func (p *Proxy) Do(ctx context.Context, g Grant, r Request) (*Response, error) {
	rt, ok := findRoute(g.Routes, r.Route)
	if !ok {
		// Not audited: the name matched no route, so there is no route to
		// write the line under.
		return nil, refuse(http.StatusForbidden, "route %q is not one this workspace holds", r.Route)
	}
	a := &auditor{p: p, g: g, rt: rt, method: r.Method, path: pathOnly(r.Path)}
	resp, err := p.do(ctx, g, rt, r, a)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			a.refused(ref)
		} else {
			a.failed()
		}
		return nil, err
	}
	return resp, nil
}

func (p *Proxy) do(ctx context.Context, g Grant, rt Route, r Request, a *auditor) (*Response, error) {
	routeURL, err := checkRoute(rt)
	if err != nil {
		return nil, refuse(http.StatusForbidden, "route %q: %v", rt.Name, err)
	}
	if !methodAllowed(rt, r.Method) {
		return nil, refuse(http.StatusForbidden, "route %q does not allow %s (it allows %s)", rt.Name, safeToken(r.Method), strings.Join(methodsOf(rt), ", "))
	}
	target, err := joinPath(routeURL, r.Path)
	if err != nil {
		return nil, refuse(http.StatusForbidden, "route %q: %v", rt.Name, err)
	}
	u := p.reserve(g.WorkspaceID, rt.Name)
	if u == nil {
		return nil, refuse(http.StatusTooManyRequests, "route %q: this workspace is over the route's request rate or byte budget", rt.Name)
	}
	a.usage = u

	// The context the credential is resolved on is the one the request is
	// sent with, so a restricted credential pins it (netguard.Pin).
	ctx = netguard.WithPinnable(ctx)
	body := r.Body
	if body != nil {
		body = &countingReader{r: body, u: u, a: a}
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		return nil, refuse(http.StatusForbidden, "route %q: the request cannot be built", rt.Name)
	}
	if body != nil && r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}
	req.Header = requestHeader(r.Header, rt)
	base := p.baseFor(g, r.Path)
	rewrite := p.rewriteFor(g, rt, base, r.Path)
	if rewrite != nil {
		// Let the transport ask for and undo compression, so the body
		// the rewrite reads is plain text.
		req.Header.Del("Accept-Encoding")
	}
	if err := p.addCredential(ctx, g, rt, routeURL, req); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, transportError(rt, err)
	}
	if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
		to, ok := p.redirect(g, req.URL, loc, base)
		if !ok {
			_ = resp.Body.Close()
			return nil, refuse(http.StatusBadGateway, "route %q redirected to %s, which is not under a route this workspace holds", rt.Name, hostOf(req.URL, loc))
		}
		resp.Header.Set("Location", to)
	}
	a.status = resp.StatusCode
	h := responseHeader(resp.Header, rt)
	var out io.ReadCloser = &countingBody{rc: resp.Body, u: u, a: a}
	if rewrite != nil && rewrite.applies(resp) {
		rewritten, err := rewrite.apply(out)
		if err != nil {
			return nil, err
		}
		out = rewritten.body
		h.Del("Content-Length")
		h.Set("Content-Length", fmt.Sprint(rewritten.n))
	}
	return &Response{Status: resp.StatusCode, Header: h, Body: &auditedBody{ReadCloser: out, a: a}}, nil
}

// addCredential resolves the route's credential for the run and sets it on
// req. A credential must carry a URL restriction covering the route's URL: an
// unrestricted one is refused, as for git (RFC GW §6.1), so the place a
// secret may go is always written beside the secret.
func (p *Proxy) addCredential(ctx context.Context, g Grant, rt Route, routeURL *url.URL, req *http.Request) error {
	if rt.Credential == "" {
		return nil
	}
	if p.creds == nil {
		return refuse(http.StatusForbidden, "route %q names credential %q, and no credential store is configured", rt.Name, rt.Credential)
	}
	res, found, err := p.creds.ResolveFor(ctx, g.TenantID, g.AgentName, g.UserID, rt.Credential, routeURL.String())
	switch {
	case errors.Is(err, credential.ErrURLNotAllowed):
		return refuse(http.StatusForbidden, "credential %q does not cover route %q's URL (its allowed_urls must)", rt.Credential, rt.Name)
	case err != nil:
		// The engine's errors name the credential, never its value.
		return refuse(http.StatusForbidden, "credential %q for route %q cannot be used: %v", rt.Credential, rt.Name, err)
	case !found:
		return refuse(http.StatusForbidden, "credential %q for route %q is not found for this run", rt.Credential, rt.Name)
	case res.AllowedURLs == nil:
		return refuse(http.StatusForbidden, "credential %q is not restricted to URLs; a route sends only a credential whose allowed_urls cover the route's URL", rt.Credential)
	}
	switch rt.Auth {
	case "", AuthBearer:
		req.Header.Set("Authorization", "Bearer "+res.Value)
	case AuthBasic:
		req.SetBasicAuth(res.Username, res.Value)
	case AuthHeader:
		req.Header.Set(rt.Header, res.Value)
	}
	return nil
}

// transportError reports an upstream failure without the request's URL
// (whose query a script wrote) and so without anything a credential could
// have been echoed into.
func transportError(rt Route, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("egress: route %q: upstream request failed: %s", rt.Name, msg)
}

// validFilesRoute is name when it is a route name, else "".
func validFilesRoute(name string) string {
	if ValidName(name) {
		return name
	}
	return ""
}

// baseFor is where a link in the answer to a request for path (under its
// route) reaches the routes: the proxy's base, or for a RelativeBase grant
// the climb from the page back to the base, one "../" for the route's
// segment and one for each "/" of the path before its query. A "%2f" is no
// separator to the session's client, so it is not counted either.
func (p *Proxy) baseFor(g Grant, path string) string {
	if !g.RelativeBase {
		return p.base
	}
	path, _, _ = strings.Cut(path, "?")
	return strings.Repeat("../", strings.Count(path, "/")+1)
}

func findRoute(routes []Route, name string) (Route, bool) {
	for _, r := range routes {
		if r.Name == name {
			return r, true
		}
	}
	return Route{}, false
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// redirect maps a redirect's Location onto a route the workspace holds: the
// session's client then follows it through the proxy, with that route's own
// credential or none. ok is false for a target under no held route.
func (p *Proxy) redirect(g Grant, from *url.URL, loc, linkBase string) (string, bool) {
	to, err := from.Parse(loc)
	if err != nil || to.Scheme != "https" || to.User != nil {
		return "", false
	}
	for _, rt := range g.Routes {
		base, err := checkRoute(rt)
		if err != nil || !strings.EqualFold(base.Host, to.Host) {
			continue
		}
		rest, ok := strings.CutPrefix(to.EscapedPath(), base.EscapedPath())
		if !ok {
			continue
		}
		if _, err := joinPath(base, rest); err != nil {
			continue
		}
		out := linkBase + rt.Name + "/" + rest
		if to.RawQuery != "" {
			out += "?" + to.RawQuery
		}
		return out, true
	}
	return "", false
}

// hostOf names a redirect's target host for a refusal, and nothing more of
// it (its path or query may carry what the upstream put there).
func hostOf(from *url.URL, loc string) string {
	to, err := from.Parse(loc)
	if err != nil || to.Host == "" {
		return "an address that is not a URL"
	}
	return safeToken(to.Hostname())
}
