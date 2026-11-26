package authz

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// The token scopes a route, RPC or tool may require. They are the catalog
// internal/auth documents and validates (auth.Scope* is defined from these);
// they live here because the route table names them and auth imports this
// package.
const (
	ScopeAdmin          = "substrate:admin"
	ScopeTenant         = "substrate:tenant"
	ScopeRunsCreate     = "runs:create"
	ScopeRunsRead       = "runs:read"
	ScopeRunsDelete     = "runs:delete"
	ScopeChannelPublish = "channel:publish"
	ScopeChannelRead    = "channel:read"
	ScopeLLMGateway     = "llm:gateway"
	ScopeSCIMProvision  = "scim:provision"
)

// Route is everything the HTTP gates ask of one registered route (RFC EZ P6):
// the token scope a caller must hold (the ceiling every rollout mode keeps),
// whether the route is on the run plane (the whole reach of a credential
// limited to certain agents), and the ACL question. Every route the server
// registers has exactly one row in httpRoutes, keyed by the pattern it is
// registered under, so the three answers cannot drift apart or miss a route.
//
// Scope "" admits any authenticated principal. The member, self-service and
// gateway admission paths that let a principal past a scope it lacks are the
// HTTP gate's (scopeGate), not the table's.
type Route struct {
	// Pattern is the mux pattern the route is registered under.
	Pattern  string
	Scope    string
	RunPlane bool
	// ask is the ACL question; askFn when the question names something the
	// path carries (a channel, a user). Neither: no ACL governs the route.
	ask   *Surface
	askFn func(method string, rest []string) Surface
}

// Question is the route's ACL question for a request to path, and false for a
// route no ACL governs.
func (r Route) Question(method, path string) (Surface, bool) {
	switch {
	case r.ask != nil:
		return *r.ask, true
	case r.askFn != nil:
		return r.askFn(gateMethod(method), pathRest(path)), true
	}
	return Surface{}, false
}

// Governed reports whether an ACL question governs the route.
func (r Route) Governed() bool { return r.ask != nil || r.askFn != nil }

func row(pattern, scope string, runPlane bool, q Surface) Route {
	return Route{Pattern: pattern, Scope: scope, RunPlane: runPlane, ask: &q}
}

// rowOpen is a route no ACL governs: it serves no principal (a sign-in, the UI
// shell, the docs) or answers only about the caller itself (whoami, the
// authz questions, the caller's own account).
func rowOpen(pattern, scope string, runPlane bool) Route {
	return Route{Pattern: pattern, Scope: scope, RunPlane: runPlane}
}

func rowFn(pattern, scope string, runPlane bool, f func(method string, rest []string) Surface) Route {
	return Route{Pattern: pattern, Scope: scope, RunPlane: runPlane, askFn: f}
}

func q(action, typ, name string) Surface { return instance(action, typ, name) }

// gateMethod is the method a request is gated as. The mux serves HEAD with
// the GET handler, so HEAD is asked what GET is asked.
func gateMethod(method string) string {
	if method == http.MethodHead {
		return http.MethodGet
	}
	return method
}

// pathRest is the path's segments after /v1, as the per-row questions read
// them.
func pathRest(path string) []string {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	if len(seg) > 0 && seg[0] == "v1" {
		return seg[1:]
	}
	return seg
}

// channelsAsk is the question of a /v1/_channels/{name}... route: the channel
// the path names, with the verb its operation segment (or method) implies.
func channelsAsk(method string, rest []string) Surface {
	get := method == http.MethodGet
	switch {
	case len(rest) < 2:
		if get {
			return typeLevel("subscribe", "channel")
		}
		return typeLevel("manage", "channel")
	case rest[1] == "_await":
		return typeLevel("await", "channel")
	case rest[1] == "_broadcast":
		return typeLevel("broadcast", "channel")
	case len(rest) == 2:
		if get {
			return instance("subscribe", "channel", rest[1])
		}
		if method == http.MethodPatch || method == http.MethodDelete {
			return instance("manage", "channel", rest[1])
		}
		return instance("publish", "channel", rest[1])
	}
	return instance(channelVerb(rest[2]), "channel", rest[1])
}

// userAsk asks verb of the user /v1/_users/{subject} names.
func userAsk(verb string) func(string, []string) Surface {
	return func(_ string, rest []string) Surface {
		name := ""
		if len(rest) >= 2 {
			name = rest[1]
		}
		return instance(verb, "principal", "user:"+name)
	}
}

// userChannelAsk asks verb of the channel /v1/users/{user_id}/channels/{name}
// names.
func userChannelAsk(verb string) func(string, []string) Surface {
	return func(_ string, rest []string) Surface {
		name := ""
		if len(rest) >= 4 {
			name = rest[3]
		}
		return instance(verb, "channel", name)
	}
}

// HTTPRouteByPattern returns the row of the pattern a request was routed
// under (http.Request.Pattern). The HTTP gate asks it this way, so it gates
// the route the mux actually chose, whatever the path's spelling.
func HTTPRouteByPattern(pattern string) (Route, bool) {
	i, ok := httpRouteIndex()[pattern]
	if !ok {
		return Route{}, false
	}
	return httpRoutes[i], true
}

// HTTPRoute returns the row of the route a request for method and path is
// served by, matched as the server's mux matches it. A caller that names a
// route by its method and path (a twin, the whoami page summary) asks it
// this way. The second result is false for a path no route serves.
func HTTPRoute(method, path string) (Route, bool) {
	_, mux := httpRouteTables()
	req := &http.Request{Method: method, URL: &url.URL{Path: path}, Host: "loomcycle"}
	_, pattern := mux.Handler(req)
	if pattern == "" {
		return Route{}, false
	}
	return HTTPRouteByPattern(pattern)
}

// UnroutedScope is the scope a request no route serves is held to: a write
// needs substrate:admin, a read nothing. The mux answers such a request 404
// before any gate; this is the answer for a caller that asks anyway.
func UnroutedScope(method string) string {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return ScopeAdmin
	}
	return ""
}

// HTTPRoutes returns every row, for the census tests.
func HTTPRoutes() []Route { return append([]Route(nil), httpRoutes...) }

var httpRouteTables = sync.OnceValues(func() (map[string]int, *http.ServeMux) {
	idx := make(map[string]int, len(httpRoutes))
	mux := http.NewServeMux()
	for i, rt := range httpRoutes {
		if _, dup := idx[rt.Pattern]; dup {
			panic("authz: route " + rt.Pattern + " has two rows")
		}
		idx[rt.Pattern] = i
		mux.Handle(rt.Pattern, http.NotFoundHandler())
	}
	return idx, mux
})

func httpRouteIndex() map[string]int {
	idx, _ := httpRouteTables()
	return idx
}

// httpRoutes is every route the HTTP server registers (server.go and the
// files beside it, and the webhook receiver's routes it mounts). The A2A
// transport's own routes are not here: it is mounted under its own
// authentication, outside the HTTP gates, and asks no route question.
var httpRoutes = []Route{
	rowOpen("GET /.well-known/oauth-authorization-server", "", false),
	// The A2A extended card, registered by a2a.Server.Mount behind the HTTP
	// gates: any authenticated principal, no ACL question.
	rowOpen("GET /.well-known/agent-card.json/extended", "", false),
	rowOpen("/healthz", "", false),
	// The Prometheus scrape: the operator's, and a metrics:read credential's
	// (admitted by the HTTP gate before the scope).
	row("GET /metrics", ScopeAdmin, false, q("read", "runtime", "metrics")),
	// The SCIM service (RFC FA P6), reserved by the HTTP gate to a
	// scim:provision credential.
	row("GET /scim/v2/Groups", ScopeSCIMProvision, false, q("read", "principal", "scim:groups")),
	row("POST /scim/v2/Groups", ScopeSCIMProvision, false, q("manage", "principal", "scim:groups")),
	row("DELETE /scim/v2/Groups/{id}", ScopeSCIMProvision, false, q("manage", "principal", "scim:groups")),
	row("GET /scim/v2/Groups/{id}", ScopeSCIMProvision, false, q("read", "principal", "scim:groups")),
	row("PATCH /scim/v2/Groups/{id}", ScopeSCIMProvision, false, q("manage", "principal", "scim:groups")),
	row("PUT /scim/v2/Groups/{id}", ScopeSCIMProvision, false, q("manage", "principal", "scim:groups")),
	row("GET /scim/v2/ResourceTypes", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("GET /scim/v2/ResourceTypes/{id}", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("GET /scim/v2/Schemas", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("GET /scim/v2/Schemas/{id}", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("GET /scim/v2/ServiceProviderConfig", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("GET /scim/v2/Users", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("POST /scim/v2/Users", ScopeSCIMProvision, false, q("manage", "principal", "scim:users")),
	row("DELETE /scim/v2/Users/{id}", ScopeSCIMProvision, false, q("manage", "principal", "scim:users")),
	row("GET /scim/v2/Users/{id}", ScopeSCIMProvision, false, q("read", "principal", "scim:users")),
	row("PATCH /scim/v2/Users/{id}", ScopeSCIMProvision, false, q("manage", "principal", "scim:users")),
	row("PUT /scim/v2/Users/{id}", ScopeSCIMProvision, false, q("manage", "principal", "scim:users")),
	// The Web UI shell and its sign-in, before any principal exists.
	rowOpen("GET /ui", "", false),
	rowOpen("GET /ui/", "", false),
	rowOpen("POST /ui/session", ScopeAdmin, false),
	// The definition families: the op is in the body, so a route asks the
	// family's weakest verb and the handler asks each op its own. Tenant
	// operators author their own tenant's; a member reaches them on the
	// member path.
	row("POST /v1/_a2aagentdef", ScopeTenant, false, q("read", "def", "a2a/*")),
	row("GET /v1/_a2aagentdef/names", ScopeTenant, false, q("read", "def", "a2a/*")),
	row("POST /v1/_a2aservercarddef", ScopeTenant, false, q("read", "def", "a2aserver/*")),
	row("GET /v1/_a2aservercarddef/names", ScopeTenant, false, q("read", "def", "a2aserver/*")),
	row("GET /v1/_aclbinding", ScopeTenant, false, q("read", "def", "acl/*")),
	row("POST /v1/_aclbinding", ScopeTenant, false, q("read", "def", "acl/*")),
	row("DELETE /v1/_aclbinding/{id}", ScopeTenant, false, q("read", "def", "acl/*")),
	row("POST /v1/_acldef", ScopeTenant, false, q("read", "def", "acl/*")),
	row("GET /v1/_acldef/names", ScopeTenant, false, q("read", "def", "acl/*")),
	row("POST /v1/_agentdef", ScopeTenant, false, q("read", "def", "agent/*")),
	row("GET /v1/_agentdef/names", ScopeTenant, false, q("read", "def", "agent/*")),
	row("GET /v1/_agentdef/{name}/overridable", ScopeTenant, true, q("read", "agent", "*")),
	row("GET /v1/_audit", ScopeTenant, false, q("read", "runtime", "audit")),
	// /v1/_auth/*: sign-in, invites, resets, an external gate's signed
	// assertions, logout and the caller's own account run before any
	// principal exists or act on the caller only. A tenant's gate policy is
	// the deployment admin's; approving a device login mints a token for the
	// signed-in person; a bearer may retire itself, even one limited to certain
	// agents.
	rowOpen("GET /v1/_auth/account", "", false),
	rowOpen("POST /v1/_auth/assert", "", false),
	rowOpen("GET /v1/_auth/complete/{gate}", "", false),
	rowOpen("POST /v1/_auth/complete/{gate}", "", false),
	rowOpen("POST /v1/_auth/device", "", false),
	row("POST /v1/_auth/device/approve", "", false, q("mint", "principal", "token:self")),
	row("POST /v1/_auth/device/info", "", false, q("mint", "principal", "token:self")),
	rowOpen("POST /v1/_auth/device/token", "", false),
	rowOpen("POST /v1/_auth/invite", "", false),
	rowOpen("POST /v1/_auth/invite/info", "", false),
	rowOpen("GET /v1/_auth/login/{gate}", "", false),
	rowOpen("POST /v1/_auth/logout", "", false),
	rowOpen("POST /v1/_auth/passkey/{gate}/begin", "", false),
	rowOpen("GET /v1/_auth/passkeys", "", false),
	rowOpen("POST /v1/_auth/passkeys/register/begin", "", false),
	rowOpen("POST /v1/_auth/passkeys/register/finish", "", false),
	rowOpen("DELETE /v1/_auth/passkeys/{id}", "", false),
	rowOpen("PATCH /v1/_auth/passkeys/{id}", "", false),
	rowOpen("POST /v1/_auth/password", "", false),
	rowOpen("POST /v1/_auth/reset", "", false),
	rowOpen("POST /v1/_auth/reset/info", "", false),
	rowOpen("POST /v1/_auth/revoke", "", false),
	rowOpen("POST /v1/_auth/sync", "", false),
	row("GET /v1/_auth/tenant-policies", ScopeAdmin, false, q("read", "runtime", "tenants")),
	row("DELETE /v1/_auth/tenant-policies/{tenant}", ScopeAdmin, false, q("manage", "runtime", "tenants")),
	row("PUT /v1/_auth/tenant-policies/{tenant}", ScopeAdmin, false, q("manage", "runtime", "tenants")),
	row("POST /v1/_auth/token/revoke", "", true, q("manage", "principal", "token:self")),
	rowOpen("DELETE /v1/_auth/totp", "", false),
	rowOpen("POST /v1/_auth/totp/begin", "", false),
	rowOpen("POST /v1/_auth/totp/confirm", "", false),
	// The password gate's management API: the tenant operator's, and an
	// auth:manage credential's (admitted by the HTTP gate). Every route asks
	// manage, reads included.
	row("GET /v1/_authgate", ScopeAdmin, false, q("read", "runtime", "config")),
	row("DELETE /v1/_authgate/{gate}/credentials/{subject}", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("GET /v1/_authgate/{gate}/identities", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("GET /v1/_authgate/{gate}/invites", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("POST /v1/_authgate/{gate}/invites", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("DELETE /v1/_authgate/{gate}/invites/{id}", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("PATCH /v1/_authgate/{gate}/logins/{subject}", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("POST /v1/_authgate/{gate}/resets", ScopeTenant, false, q("manage", "principal", "user:*")),
	row("GET /v1/_authgate/{gate}/tenant-status", ScopeTenant, false, q("manage", "principal", "user:*")),
	// The AuthZEN-shaped questions, open to any principal for itself and
	// confined to it by the handler.
	rowOpen("POST /v1/_authz/evaluation", "", false),
	rowOpen("POST /v1/_authz/evaluations", "", false),
	rowOpen("POST /v1/_authz/explain", "", false),
	rowOpen("POST /v1/_authz/simulate", "", false),
	// The Calc tool outside a run: a pure function of its arguments that
	// reaches no stored data, so any principal; off the run plane.
	rowOpen("POST /v1/_calc", "", false),
	// The channel admin family, tenant-confined by its handlers.
	row("GET /v1/_channels", ScopeTenant, false, q("subscribe", "channel", "*")),
	row("POST /v1/_channels", ScopeTenant, false, q("manage", "channel", "*")),
	row("POST /v1/_channels/_await", ScopeTenant, false, q("await", "channel", "*")),
	row("POST /v1/_channels/_broadcast", ScopeTenant, false, q("broadcast", "channel", "*")),
	rowFn("POST /v1/_channels/{name...}", ScopeTenant, false, channelsAsk),
	rowFn("DELETE /v1/_channels/{name}", ScopeTenant, false, channelsAsk),
	rowFn("PATCH /v1/_channels/{name}", ScopeTenant, false, channelsAsk),
	rowFn("POST /v1/_channels/{name}/ack", ScopeTenant, false, channelsAsk),
	rowFn("GET /v1/_channels/{name}/peek", ScopeTenant, false, channelsAsk),
	rowFn("POST /v1/_channels/{name}/publish", ScopeTenant, false, channelsAsk),
	rowFn("POST /v1/_channels/{name}/purge", ScopeTenant, false, channelsAsk),
	rowFn("POST /v1/_channels/{name}/release", ScopeTenant, false, channelsAsk),
	rowFn("POST /v1/_channels/{name}/subscribe", ScopeTenant, false, channelsAsk),
	row("POST /v1/_clientdef", ScopeTenant, false, q("read", "def", "client/*")),
	row("GET /v1/_clientdef/names", ScopeTenant, false, q("read", "def", "client/*")),
	row("GET /v1/_concurrency/stats", ScopeAdmin, false, q("read", "runtime", "metrics")),
	row("POST /v1/_config/reload", ScopeAdmin, false, q("manage", "runtime", "config")),
	row("GET /v1/_config/teams", ScopeAdmin, false, q("read", "runtime", "config")),
	row("POST /v1/_credentialdef", ScopeTenant, false, q("use", "credential", "*")),
	row("POST /v1/_crypto", ScopeTenant, false, q("use", "crypto", "*")),
	// A decision outside a run spends what a run would: the scope that
	// creates one.
	row("POST /v1/_decide", ScopeRunsCreate, false, q("use", "gateway", "decide")),
	row("GET /v1/_decide/models", ScopeRunsCreate, false, q("use", "gateway", "decide")),
	// Documents and the Path tree. The maintenance passes ask write and are
	// carved out of the member path; the projection stays the admin's (it is
	// not carved out, so opening it would widen a member's reach).
	row("POST /v1/_document", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_document/asset/{chunk_id}", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_document/changes", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_document/derive_units", ScopeTenant, false, q("write", "document", "*")),
	row("POST /v1/_document/describe_images", ScopeTenant, false, q("write", "document", "*")),
	row("GET /v1/_document/export/{document_id}", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_document/feed", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_document/project", ScopeAdmin, false, q("write", "document", "*")),
	row("POST /v1/_document/reindex", ScopeTenant, false, q("write", "document", "*")),
	row("POST /v1/_documentsourcedef", ScopeTenant, false, q("read", "def", "documentsource/*")),
	row("GET /v1/_documentsourcedef/names", ScopeTenant, false, q("read", "def", "documentsource/*")),
	row("POST /v1/_egressroutedef", ScopeTenant, false, q("read", "def", "egressroute/*")),
	row("GET /v1/_env_template", ScopeAdmin, false, q("read", "runtime", "config")),
	row("GET /v1/_erasure", ScopeTenant, false, q("read", "runtime", "erasure")),
	row("POST /v1/_erasure", ScopeTenant, false, q("manage", "runtime", "erasure")),
	row("POST /v1/_evaluation", ScopeRunsRead, false, q("read", "evaluation", "*")),
	row("GET /v1/_events", ScopeTenant, false, q("read", "runtime", "events")),
	row("GET /v1/_groups", ScopeTenant, false, q("read", "principal", "group:*")),
	row("POST /v1/_groups", ScopeTenant, false, q("read", "principal", "group:*")),
	row("DELETE /v1/_groups/{name}", ScopeTenant, false, q("read", "principal", "group:*")),
	row("GET /v1/_groups/{name}", ScopeTenant, false, q("read", "principal", "group:*")),
	row("GET /v1/_groups/{name}/effective", ScopeTenant, false, q("read", "principal", "group:*")),
	row("POST /v1/_groups/{name}/members", ScopeTenant, false, q("read", "principal", "group:*")),
	row("DELETE /v1/_groups/{name}/members/{kind}/{id}", ScopeTenant, false, q("read", "principal", "group:*")),
	row("POST /v1/_history", ScopeRunsRead, false, q("read", "history", "*")),
	row("POST /v1/_hookdef", ScopeTenant, false, q("read", "def", "hook/*")),
	row("GET /v1/_hookdef/names", ScopeTenant, false, q("read", "def", "hook/*")),
	row("GET /v1/_library/agents", ScopeTenant, false, q("read", "agent", "*")),
	row("GET /v1/_library/mcp-servers", ScopeTenant, false, q("read", "def", "mcpserver/*")),
	row("GET /v1/_library/skills", ScopeTenant, false, q("read", "skill", "*")),
	row("DELETE /v1/_limits", ScopeTenant, false, q("manage", "runtime", "limits")),
	row("GET /v1/_limits", ScopeTenant, false, q("read", "runtime", "limits")),
	row("PUT /v1/_limits", ScopeTenant, false, q("manage", "runtime", "limits")),
	// The raw model endpoint: llm:gateway, which a non-isolated member is
	// admitted without (the HTTP gate's gateway member path).
	row("POST /v1/_llm/chat", ScopeLLMGateway, false, q("use", "gateway", "chat")),
	// The loomcycle-as-MCP-server transport: its per-tool gate applies the
	// scope and run plane of each tool (authz.MCPToolRoute).
	row("DELETE /v1/_mcp", ScopeTenant, true, q("use", "transport", "mcp")),
	row("POST /v1/_mcp", ScopeTenant, true, q("use", "transport", "mcp")),
	row("POST /v1/_mcpserverdef", ScopeTenant, false, q("read", "def", "mcpserver/*")),
	row("GET /v1/_mcpserverdef/names", ScopeTenant, false, q("read", "def", "mcpserver/*")),
	// Whoami: any principal, and a credential limited to certain agents.
	rowOpen("GET /v1/_me", "", true),
	rowOpen("GET /v1/_me/app-sign-ins", "", false),
	rowOpen("DELETE /v1/_me/app-sign-ins/{family_id}", "", false),
	// Memory browse, search and maintenance, tenant-confined. repair-tenant
	// rewrites rows across every scope in one statement: the operator's.
	row("POST /v1/_memory/backfill_embeddings", ScopeTenant, false, q("write", "memory", "*")),
	row("POST /v1/_memory/backfill_traces", ScopeTenant, false, q("write", "memory", "*")),
	row("GET /v1/_memory/changes", ScopeTenant, false, q("read", "memory", "*")),
	row("POST /v1/_memory/collapse_facts", ScopeTenant, false, q("write", "memory", "*")),
	row("GET /v1/_memory/embed_stats", ScopeTenant, false, q("read", "memory", "*")),
	row("POST /v1/_memory/home_facts", ScopeTenant, false, q("write", "memory", "*")),
	row("POST /v1/_memory/purge_stale_embeddings", ScopeTenant, false, q("write", "memory", "*")),
	row("POST /v1/_memory/reembed", ScopeTenant, false, q("write", "memory", "*")),
	row("POST /v1/_memory/repair-tenant", ScopeAdmin, false, q("manage", "runtime", "tenants")),
	row("GET /v1/_memory/scopes", ScopeTenant, false, q("read", "memory", "*")),
	row("GET /v1/_memory/scopes/{scope}", ScopeTenant, false, q("read", "memory", "*")),
	row("GET /v1/_memory/scopes/{scope}/{scope_id}/keys", ScopeTenant, false, q("read", "memory", "*")),
	row("DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", ScopeTenant, false, q("write", "memory", "*")),
	row("GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", ScopeTenant, false, q("read", "memory", "*")),
	row("PUT /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", ScopeTenant, false, q("write", "memory", "*")),
	row("POST /v1/_memory/search", ScopeTenant, false, q("read", "memory", "*")),
	row("POST /v1/_memorybackenddef", ScopeTenant, false, q("read", "def", "memorybackend/*")),
	row("GET /v1/_memorybackenddef/names", ScopeTenant, false, q("read", "def", "memorybackend/*")),
	row("GET /v1/_metrics/runs/{run_id}", ScopeAdmin, false, q("read", "runtime", "metrics")),
	row("GET /v1/_metrics/samples", ScopeAdmin, false, q("read", "runtime", "metrics")),
	row("GET /v1/_metrics/summary", ScopeAdmin, false, q("read", "runtime", "metrics")),
	row("GET /v1/_models", ScopeTenant, false, q("read", "runtime", "routing")),
	// The app sign-in routes (RFC FL), authenticated by the code or the
	// refresh token, not a bearer; never behind the HTTP gates.
	rowOpen("GET /v1/_oauth/authorize", ScopeAdmin, false),
	rowOpen("OPTIONS /v1/_oauth/revoke", ScopeAdmin, false),
	rowOpen("POST /v1/_oauth/revoke", ScopeAdmin, false),
	rowOpen("OPTIONS /v1/_oauth/token", ScopeAdmin, false),
	rowOpen("POST /v1/_oauth/token", ScopeAdmin, false),
	// The tenant ontology. Its writes, the plan and its releases are carved
	// out of the member path.
	row("GET /v1/_ontology", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_ontology", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_ontology/adopt", ScopeTenant, false, q("write", "document", "*")),
	row("POST /v1/_ontology/plan", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_ontology/proposals", ScopeTenant, false, q("write", "document", "*")),
	row("GET /v1/_ontology/releases", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_ontology/releases/diff", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_ontology/releases/{seq}", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_ontology/rollback", ScopeTenant, false, q("write", "document", "*")),
	row("POST /v1/_ontology/subjects/merge", ScopeTenant, false, q("write", "document", "*")),
	row("POST /v1/_ontology/subjects/principal", ScopeTenant, false, q("write", "document", "*")),
	// Token minting: a tenant operator only for its own tenant's scrape
	// tokens (the tool's grant); the names listing stays the admin's.
	row("POST /v1/_operatortokendef", ScopeTenant, false, q("read", "principal", "token:*")),
	row("GET /v1/_operatortokendef/names", ScopeAdmin, false, q("read", "principal", "token:*")),
	row("POST /v1/_path", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_pause", ScopeAdmin, false, q("manage", "runtime", "pause")),
	row("GET /v1/_presets", ScopeAdmin, false, q("read", "runtime", "config")),
	row("GET /v1/_presets/{name}", ScopeAdmin, false, q("read", "runtime", "config")),
	row("GET /v1/_providers/{id}/models", ScopeAdmin, false, q("read", "runtime", "config")),
	row("GET /v1/_resident", ScopeTenant, false, q("read", "run", "*")),
	row("POST /v1/_resident/{run_id}/cancel", ScopeTenant, false, q("cancel", "run", "*")),
	row("POST /v1/_resident/{run_id}/close", ScopeTenant, false, q("cancel", "run", "*")),
	row("POST /v1/_resolve/probe", ScopeAdmin, false, q("manage", "runtime", "config")),
	row("GET /v1/_resolver", ScopeAdmin, false, q("read", "runtime", "config")),
	row("POST /v1/_resume", ScopeAdmin, false, q("manage", "runtime", "pause")),
	row("GET /v1/_retention", ScopeTenant, false, q("read", "runtime", "retention")),
	row("DELETE /v1/_retention/overrides", ScopeTenant, false, q("manage", "runtime", "retention")),
	row("GET /v1/_retention/overrides", ScopeTenant, false, q("read", "runtime", "retention")),
	row("PUT /v1/_retention/overrides", ScopeTenant, false, q("manage", "runtime", "retention")),
	row("GET /v1/_routing", ScopeTenant, false, q("read", "runtime", "routing")),
	// Which agents the caller may run: part of starting a run.
	row("GET /v1/_runnable-agents", ScopeRunsRead, true, q("read", "agent", "*")),
	row("POST /v1/_scheduledef", ScopeTenant, false, q("read", "def", "schedule/*")),
	row("GET /v1/_scheduledef/names", ScopeTenant, false, q("read", "def", "schedule/*")),
	row("GET /v1/_schedules/list-all", ScopeTenant, false, q("read", "def", "schedule/*")),
	row("POST /v1/_schedules/{def_id}/pause", ScopeTenant, false, q("manage", "def", "schedule/*")),
	row("POST /v1/_schedules/{def_id}/resume", ScopeTenant, false, q("manage", "def", "schedule/*")),
	row("POST /v1/_schedules/{def_id}/run-now", ScopeTenant, false, q("manage", "def", "schedule/*")),
	row("GET /v1/_schedules/{def_id}/state", ScopeTenant, false, q("read", "def", "schedule/*")),
	row("POST /v1/_skilldef", ScopeTenant, false, q("read", "def", "skill/*")),
	row("GET /v1/_skilldef/names", ScopeTenant, false, q("read", "def", "skill/*")),
	row("GET /v1/_snapshots", ScopeAdmin, false, q("read", "runtime", "snapshot")),
	row("POST /v1/_snapshots", ScopeAdmin, false, q("manage", "runtime", "snapshot")),
	row("DELETE /v1/_snapshots/{id}", ScopeAdmin, false, q("manage", "runtime", "snapshot")),
	row("GET /v1/_snapshots/{id}", ScopeAdmin, false, q("read", "runtime", "snapshot")),
	row("GET /v1/_snapshots/{id}/export", ScopeAdmin, false, q("read", "runtime", "snapshot")),
	row("POST /v1/_snapshots/{id}/restore", ScopeAdmin, false, q("manage", "runtime", "snapshot")),
	rowOpen("GET /v1/_speech", ScopeRunsCreate, false),
	row("GET /v1/_state", ScopeAdmin, false, q("read", "runtime", "pause")),
	// The statement review queue, rulings and their undo ask read; the
	// connector asks review or write of each statement.
	row("GET /v1/_statements/agreement", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_statements/apply_policy", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_statements/revert", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_statements/review", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_statements/rule", ScopeTenant, false, q("read", "document", "*")),
	row("GET /v1/_statements/rulings", ScopeTenant, false, q("read", "document", "*")),
	row("POST /v1/_teamdef", ScopeTenant, false, q("read", "def", "team/*")),
	row("GET /v1/_teamdef/names", ScopeTenant, false, q("read", "def", "team/*")),
	row("GET /v1/_teamdef/{team}/channels", ScopeTenant, false, q("subscribe", "channel", "*")),
	row("GET /v1/_teamdef/{team}/channels/{name}/peek", ScopeTenant, false, q("subscribe", "channel", "*")),
	row("POST /v1/_teamdef/{team}/channels/{name}/release", ScopeTenant, false, q("manage", "channel", "*")),
	// Webhook receivers authenticate each delivery by its definition's
	// signature, not a bearer: never behind the HTTP gates.
	row("POST /v1/_teams/{team}/webhooks/{name}", ScopeAdmin, false, q("run", "team", "*")),
	row("POST /v1/_teams/{tenant}/{team}/webhooks/{name}", ScopeAdmin, false, q("run", "team", "*")),
	row("DELETE /v1/_telemetry", ScopeTenant, false, q("manage", "runtime", "telemetry")),
	row("GET /v1/_telemetry", ScopeTenant, false, q("read", "runtime", "telemetry")),
	row("PUT /v1/_telemetry", ScopeTenant, false, q("manage", "runtime", "telemetry")),
	row("POST /v1/_telemetry:test", ScopeTenant, false, q("manage", "runtime", "telemetry")),
	row("GET /v1/_tenants", ScopeAdmin, false, q("read", "runtime", "tenants")),
	row("GET /v1/_usage", ScopeTenant, false, q("read", "runtime", "usage")),
	row("GET /v1/_usage/speed", ScopeTenant, false, q("read", "runtime", "usage")),
	// The user roster. Listing it is any principal's (the handler
	// tenant-scopes it); the rest is the tenant operator's.
	row("GET /v1/_users", "", false, q("read", "principal", "user:*")),
	row("POST /v1/_users", ScopeTenant, false, q("manage", "principal", "user:*")),
	rowFn("DELETE /v1/_users/{subject}", ScopeTenant, false, userAsk("manage")),
	rowFn("GET /v1/_users/{subject}", ScopeTenant, false, userAsk("read")),
	rowFn("PATCH /v1/_users/{subject}", ScopeTenant, false, userAsk("manage")),
	rowFn("GET /v1/_users/{subject}/app-sign-ins", ScopeTenant, false, userAsk("read")),
	rowFn("DELETE /v1/_users/{subject}/app-sign-ins/{family_id}", ScopeTenant, false, userAsk("manage")),
	rowFn("POST /v1/_users/{subject}/approve", ScopeTenant, false, userAsk("manage")),
	rowFn("GET /v1/_users/{subject}/identities", ScopeTenant, false, userAsk("read")),
	rowFn("POST /v1/_users/{subject}/identities", ScopeTenant, false, userAsk("manage")),
	rowFn("DELETE /v1/_users/{subject}/identities/{gate}", ScopeTenant, false, userAsk("manage")),
	rowFn("GET /v1/_users/{subject}/tokens", ScopeTenant, false, userAsk("mint")),
	rowFn("POST /v1/_users/{subject}/tokens", ScopeTenant, false, userAsk("mint")),
	rowFn("DELETE /v1/_users/{subject}/tokens/{def_id}", ScopeTenant, false, userAsk("mint")),
	row("POST /v1/_volumedef", ScopeTenant, false, q("read", "def", "volume/*")),
	row("GET /v1/_volumes", ScopeTenant, false, q("read", "volume", "*")),
	row("GET /v1/_volumes/ephemeral", ScopeTenant, false, q("read", "volume", "*")),
	row("POST /v1/_webhookdef", ScopeTenant, false, q("read", "def", "webhook/*")),
	row("GET /v1/_webhookdef/names", ScopeTenant, false, q("read", "def", "webhook/*")),
	row("POST /v1/_webhooks/{name}", ScopeAdmin, false, q("manage", "def", "webhook/*")),
	row("GET /v1/_webhooks/{name}/delivery-counts", ScopeAdmin, false, q("manage", "def", "webhook/*")),
	row("GET /v1/_webhooks/{name}/recent-deliveries", ScopeAdmin, false, q("manage", "def", "webhook/*")),
	row("POST /v1/_webhooks/{name}/test", ScopeAdmin, false, q("manage", "def", "webhook/*")),
	row("POST /v1/_webhooks/{tenant}/{name}", ScopeAdmin, false, q("manage", "def", "webhook/*")),
	// Runs and sessions: create and control with runs:create, read with
	// runs:read, delete a tree with runs:delete (off the run plane: a token
	// limited to certain agents does not erase records).
	row("GET /v1/agents/{agent_id}", ScopeRunsRead, true, q("read", "run", "*")),
	row("POST /v1/agents/{agent_id}/cancel", ScopeRunsCreate, true, q("cancel", "run", "*")),
	row("GET /v1/agents/{agent_name}/channels", ScopeRunsRead, false, q("subscribe", "channel", "*")),
	// Read-aloud and transcription: a chat user's call, so runs:create and not
	// llm:gateway (the engine and model are the operator's); off the run plane.
	row("POST /v1/audio/speech", ScopeRunsCreate, false, q("use", "gateway", "speech")),
	row("POST /v1/audio/transcriptions", ScopeRunsCreate, false, q("use", "gateway", "transcription")),
	row("POST /v1/chat/completions", ScopeLLMGateway, false, q("use", "gateway", "chat")),
	// The client-tool socket and the ticket that opens it: one gate.
	row("GET /v1/client-tools", ScopeRunsCreate, false, q("run", "agent", "*")),
	row("POST /v1/client-tools/ticket", ScopeRunsCreate, false, q("run", "agent", "*")),
	rowOpen("GET /v1/config", "", true),
	rowOpen("GET /v1/docs", "", false),
	rowOpen("GET /v1/docs/{file}", "", false),
	row("POST /v1/embeddings", ScopeLLMGateway, false, q("use", "gateway", "embeddings")),
	rowOpen("GET /v1/openapi.json", "", false),
	rowOpen("GET /v1/openapi.yaml", "", false),
	row("GET /v1/runs", ScopeRunsRead, true, q("read", "run", "*")),
	row("POST /v1/runs", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("DELETE /v1/runs/{run_id}", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("GET /v1/runs/{run_id}", ScopeRunsRead, true, q("read", "run", "*")),
	row("PATCH /v1/runs/{run_id}", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("GET /v1/runs/{run_id}/breakpoints", ScopeRunsRead, true, q("read", "run", "*")),
	row("PUT /v1/runs/{run_id}/breakpoints", ScopeRunsCreate, true, q("steer", "run", "*")),
	row("POST /v1/runs/{run_id}/cancel", ScopeRunsCreate, true, q("cancel", "run", "*")),
	row("POST /v1/runs/{run_id}/compact", ScopeRunsCreate, true, q("compact", "run", "*")),
	row("GET /v1/runs/{run_id}/config", ScopeRunsRead, true, q("read", "run", "*")),
	row("GET /v1/runs/{run_id}/effective-config", ScopeRunsRead, true, q("read", "run", "*")),
	row("POST /v1/runs/{run_id}/file", ScopeRunsRead, true, q("read", "run", "*")),
	row("GET /v1/runs/{run_id}/inbox/{path...}", ScopeRunsRead, true, q("read", "run", "*")),
	// A run owner's file put (RFC HE F1): off the run plane, so a token
	// limited to certain agents needs the inbox_put surface granted; asked as
	// that surface's gateway kind, as the audio surfaces are.
	row("PUT /v1/runs/{run_id}/inbox/{name}", ScopeRunsCreate, false, q("use", "gateway", "inbox_put")),
	row("POST /v1/runs/{run_id}/input", ScopeRunsCreate, true, q("steer", "run", "*")),
	row("GET /v1/runs/{run_id}/interrupts", ScopeRunsRead, true, q("read", "run", "*")),
	row("POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve", ScopeRunsCreate, true, q("steer", "run", "*")),
	row("GET /v1/runs/{run_id}/prompt", ScopeRunsRead, true, q("read", "run", "*")),
	row("POST /v1/runs/{run_id}/retune", ScopeRunsCreate, true, q("retune", "run", "*")),
	row("POST /v1/runs/{run_id}/review", ScopeRunsCreate, true, q("review", "run", "*")),
	row("POST /v1/runs/{run_id}/start", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("GET /v1/runs/{run_id}/stream", ScopeRunsRead, true, q("read", "run", "*")),
	row("DELETE /v1/runs/{run_id}/tree", ScopeRunsDelete, false, q("delete", "run", "*")),
	row("POST /v1/runs:batch", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("DELETE /v1/sessions/{id}", ScopeRunsDelete, false, q("delete", "run", "*")),
	row("POST /v1/sessions/{id}/messages", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("POST /v1/sessions/{id}/replay", ScopeRunsCreate, true, q("run", "agent", "*")),
	row("GET /v1/sessions/{id}/transcript", ScopeRunsRead, true, q("read", "run", "*")),
	row("GET /v1/users/{user_id}/agents", ScopeRunsRead, true, q("read", "run", "*")),
	row("GET /v1/users/{user_id}/agents/stream", ScopeRunsRead, true, q("read", "run", "*")),
	rowFn("POST /v1/users/{user_id}/channels/{name}/ack", ScopeChannelPublish, false, userChannelAsk("ack")),
	rowFn("GET /v1/users/{user_id}/channels/{name}/peek", ScopeChannelRead, false, userChannelAsk("subscribe")),
	rowFn("POST /v1/users/{user_id}/channels/{name}/publish", ScopeChannelPublish, false, userChannelAsk("publish")),
	rowFn("POST /v1/users/{user_id}/channels/{name}/subscribe", ScopeChannelPublish, false, userChannelAsk("subscribe")),
	row("GET /v1/users/{user_id}/interrupts", ScopeRunsRead, true, q("read", "run", "*")),
	// A workspace's exposed port (RFC GW §5.3), every method: reading what a
	// run made. The handler admits the workspace's own user alone (D22).
	// Off the run plane, so a token limited to certain agents reaches no
	// page (P9 plan D-P9-26).
	row("/v1/workspaces/exposures/{id}/{path...}", ScopeRunsRead, false, q("read", "run", "*")),
	// Its socket's one-time ticket (D-P9-42 as ruled): gated as the page.
	row("POST /v1/workspaces/exposure-tickets/{id}", ScopeRunsRead, false, q("read", "run", "*")),
}
