package http

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// navSurface is one Web UI page — a left-nav item, a Settings section or a
// settings-menu link — and the route its page loads first. GET /v1/_me
// reports, per page, whether the caller may reach that route, so the UI
// shows the pages a principal can actually use instead of guessing from its
// role (RFC EZ §4). The route is the page's PRIMARY call: the one whose
// refusal leaves the page with nothing to show.
type navSurface struct {
	// ID is the page's id in the Web UI: NavItem.id in
	// web/src/components/navItems.ts, Section or AccountLinkId in
	// web/src/lib/settingsSections.ts. TestNavSurfaces_CoverEveryWebUIPage
	// holds the two lists equal.
	ID     string
	Method string
	// Path is a concrete request path; "{subject}" becomes the caller's own
	// subject (the page reads the caller's own rows).
	Path string
	// Operator marks a page that administers the tenant through a route
	// which also admits members or isolated users for something narrower —
	// the credential store's self-service path, the any-principal user
	// list. Its handler serves the page's purpose only to a tenant operator,
	// admin or legacy (the same rule aclEnter applies), so the page needs
	// that role as well as the route.
	Operator bool
}

// navSurfaces is the one table. A page with no row falls back to the UI's
// role rule; TestNavSurfaces_EveryEntryRouteIsRegistered holds every route
// to one the mux serves and an ACL governs.
var navSurfaces = []navSurface{
	// The left nav.
	{ID: "run", Method: "POST", Path: "/v1/runs"},
	{ID: "runs", Method: "GET", Path: "/v1/users/{subject}/agents"},
	{ID: "library", Method: "GET", Path: "/v1/_library/agents"},
	{ID: "integrations", Method: "GET", Path: "/v1/_webhookdef/names"},
	{ID: "volumes", Method: "GET", Path: "/v1/_volumes"},
	{ID: "paths", Method: "POST", Path: "/v1/_path"},
	{ID: "channels", Method: "GET", Path: "/v1/_channels"},
	{ID: "schedules", Method: "GET", Path: "/v1/_schedules/list-all"},
	{ID: "teams", Method: "GET", Path: "/v1/_teamdef/names"},
	{ID: "interrupts", Method: "GET", Path: "/v1/users/{subject}/interrupts"},
	{ID: "memory", Method: "GET", Path: "/v1/_memory/scopes"},
	{ID: "snapshots", Method: "GET", Path: "/v1/_snapshots"},
	{ID: "audit", Method: "GET", Path: "/v1/_events"},
	{ID: "usage", Method: "GET", Path: "/v1/_usage"},
	{ID: "activity", Method: "GET", Path: "/v1/_metrics/summary"},
	// The left nav's access group: each section of /access is gated by the
	// route it loads first, so an operator an ACL denies the rule lists
	// still manages users.
	{ID: "users", Method: "GET", Path: "/v1/_users", Operator: true},
	{ID: "groups", Method: "GET", Path: "/v1/_groups"},
	{ID: "policies", Method: "POST", Path: "/v1/_acldef"},
	{ID: "bindings", Method: "GET", Path: "/v1/_aclbinding"},
	// The simulator's own route asks no ACL question (any principal may ask
	// about itself), and the page cannot build a question without the rule
	// catalog, which it reads from the policies route.
	{ID: "simulator", Method: "POST", Path: "/v1/_acldef"},
	{ID: "tokens", Method: "POST", Path: "/v1/_operatortokendef"},
	// Settings sections. Credentials is the tenant's credential authoring;
	// a member's own credentials are the account menu's my-credentials
	// page below.
	{ID: "credentials", Method: "POST", Path: "/v1/_credentialdef", Operator: true},
	{ID: "limits", Method: "GET", Path: "/v1/_limits"},
	{ID: "routing", Method: "GET", Path: "/v1/_routing"},
	{ID: "ontology", Method: "GET", Path: "/v1/_ontology"},
	{ID: "retention", Method: "GET", Path: "/v1/_retention"},
	{ID: "erasure", Method: "GET", Path: "/v1/_erasure"},
	{ID: "presets", Method: "GET", Path: "/v1/_presets"},
	{ID: "runtime", Method: "GET", Path: "/v1/_state"},
	{ID: "lanes", Method: "GET", Path: "/v1/_concurrency/stats"},
	{ID: "maintenance", Method: "POST", Path: "/v1/_memory/repair-tenant"},
	// Health reads the open /healthz, which no gate governs; the tab is part
	// of the operator's runtime panel, so it is anchored on the runtime
	// state route rather than shown to every principal.
	{ID: "health", Method: "GET", Path: "/v1/_state"},
	// The account menu's links.
	{ID: "my-credentials", Method: "POST", Path: "/v1/_credentialdef"},
}

// whoamiEffective is GET /v1/_me's capability summary: bounded by the
// table above, no per-instance data, nothing secret.
type whoamiEffective struct {
	Surfaces map[string]bool `json:"surfaces"`
}

// openModeEffective is the summary with no principal: every page.
func openModeEffective() whoamiEffective {
	out := make(map[string]bool, len(navSurfaces))
	for _, ns := range navSurfaces {
		out[ns.ID] = true
	}
	return whoamiEffective{Surfaces: out}
}

// navEffective answers, per page, whether p may reach it. ctx is the
// /v1/_me request's, which authMiddleware stamped with the rollout mode and
// transport, so the ACL is asked under the mode a request would face.
func navEffective(ctx context.Context, p auth.Principal) whoamiEffective {
	operator := p.Legacy || auth.HasScope(p.Scopes, auth.ScopeTenant)
	subject := p.Subject
	if subject == "" {
		subject = "_"
	}
	out := make(map[string]bool, len(navSurfaces))
	for _, ns := range navSurfaces {
		path := strings.ReplaceAll(ns.Path, "{subject}", subject)
		out[ns.ID] = (!ns.Operator || operator) && routeGatesAdmit(ctx, p, ns.Method, path)
	}
	return whoamiEffective{Surfaces: out}
}

// routeGatesAdmit is authMiddleware's sequence of gates for a resolved
// principal — scope, run plane, the route's type-level ACL question —
// asked without serving or refusing anything. Unlike the middleware it
// records nothing: a whoami is not a request to those routes, so a "no"
// here must not write an acl_denied audit row. An ACL set enforce mode
// could not evaluate is a "no", as the request would be refused.
func routeGatesAdmit(ctx context.Context, p auth.Principal, method, path string) bool {
	if _, ok := scopeGate(p, method, path); !ok {
		return false
	}
	if p.Restrictions.RunPlaneOnly() && !runPlaneRoute(method, path) {
		return false
	}
	surface, governed := authz.HTTPSurface(method, path)
	if !governed {
		return true
	}
	res := authz.EvaluateSurface(ctx, p.Effective, surface, true)
	return res.Allowed && !res.Unavailable
}
