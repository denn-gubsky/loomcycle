package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// EgressRouteDef is the op handler for the routes a tenant writes (RFC GW
// §6.3, P4): flat, tenant-scoped records like VolumeDef, with the ops create,
// get, list and delete. A route names an upstream URL a workspace may reach
// as http://egress/<name>/…, the methods it may send, and optionally a stored
// credential the core adds on the way out.
//
// Unlike VolumeDef this is NOT an in-run tool (RFC GW §6.3 "Who may not write
// one", D10): a model must not widen its own way out. It is not in the boot
// catalog, so no agent's tools can name it, and it refuses any call made with
// a run on ctx, as HookDef does, so a wiring mistake cannot hand it to a run.
// The operator surfaces (HTTP, gRPC, MCP) reach it through the connector,
// gated at substrate:tenant.
//
// Tenant is tools.RunIdentity(ctx).TenantID, which the operator surfaces set
// from the authoritative principal, never from the wire.
type EgressRouteDef struct {
	// Store is the persistence backend. Required.
	Store store.Store
	// Cfg is the loaded config: workspace.egress holds the operator's ceiling
	// (tenant_routes, tenant_hosts) and the operator's own route names.
	// Required.
	Cfg *config.Config
	// Credentials answers metadata lookups for a credentialed route. Nil
	// refuses every credentialed route: the check cannot be made.
	Credentials EgressRouteCredentials
}

// EgressRouteCredentials is the one call creation makes on the credential
// engine: metadata, never the secret. *credential.Engine satisfies it.
type EgressRouteCredentials interface {
	Get(ctx context.Context, id credential.Identity) (credential.Meta, error)
}

// EgressRouteDefToolName is the handler's name on the operator surfaces.
const EgressRouteDefToolName = "EgressRouteDef"

const egressRouteDefDescription = `Write the egress routes this tenant's workspaces may reach the network through (RFC GW §6.3). ` +
	`A route is a name, an https upstream URL ending in "/", the methods a session may send (default GET, HEAD), ` +
	`and optionally a stored credential ("$cred:<name>") the core adds on the way out, sent as bearer (default), basic, ` +
	`or in a named header. A script in a workspace calls http://egress/<name>/<path>; the core maps the path under the URL. ` +
	`Operations: create, get, list, delete. Off unless the operator sets workspace.egress.tenant_routes; ` +
	`workspace.egress.tenant_hosts, when set, lists the only hosts a route may name. A route cannot take an operator route's name. ` +
	`A credential must be a tenant credential restricted (allowed_urls) to URLs covering the route's URL. ` +
	`Routes are written by people: no agent can call this. A name is replaced by delete, then create.`

const egressRouteDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["create","get","list","delete"]},
    "name":       {"type": "string", "description": "Route name (create/get/delete): a lowercase letter, then up to 62 lowercase letters, digits or -. The first path segment of http://egress/<name>/ in a session."},
    "url":        {"type": "string", "description": "create: the upstream URL, https, no user, query or fragment, in canonical form and ending in /."},
    "methods":    {"type": "array", "items": {"type": "string", "enum": ["GET","HEAD","POST","PUT","PATCH","DELETE","OPTIONS"]}, "description": "create: the methods a session may send. Default GET, HEAD. A route with another method is listed with writes: true."},
    "credential": {"type": "string", "description": "create: a stored tenant credential, \"$cred:<name>\", restricted to URLs covering url. Never a literal secret."},
    "auth":       {"type": "string", "enum": ["bearer","basic","header"], "description": "create: how the credential is sent. Default bearer with a credential."},
    "header":     {"type": "string", "description": "create with auth header: the header the credential is sent in."}
  },
  "required": ["op"]
}`

type egressRouteDefInput struct {
	Op         string   `json:"op"`
	Name       string   `json:"name,omitempty"`
	URL        string   `json:"url,omitempty"`
	Methods    []string `json:"methods,omitempty"`
	Credential string   `json:"credential,omitempty"`
	Auth       string   `json:"auth,omitempty"`
	Header     string   `json:"header,omitempty"`
}

// Name implements tools.Tool.
func (e *EgressRouteDef) Name() string { return EgressRouteDefToolName }

// Description implements tools.Tool.
func (e *EgressRouteDef) Description() string { return egressRouteDefDescription }

// InputSchema implements tools.Tool.
func (e *EgressRouteDef) InputSchema() json.RawMessage {
	return json.RawMessage(egressRouteDefInputSchema)
}

// Execute implements tools.Tool.
func (e *EgressRouteDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if e.Store == nil || e.Cfg == nil {
		return errResult("EgressRouteDef: not configured (no Store or Config)"), nil
	}
	if tools.RunID(ctx) != "" {
		return errResult("EgressRouteDef is not available inside a run; routes are written by a tenant operator through the operator surfaces"), nil
	}
	// The surfaces gate at substrate:tenant; checked here too, so a surface
	// that forgot its gate cannot let a member write where scripts send data.
	// No principal is the open-mode operator.
	if p, ok := auth.PrincipalFromContext(ctx); ok && !auth.HasScope(p.Scopes, auth.ScopeTenant) {
		return errResult("EgressRouteDef needs " + auth.ScopeTenant + ": routes are written by the tenant's operators"), nil
	}
	var in egressRouteDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	if res, refused := tools.AuthorizeDefOp(ctx, "egressroute", in.Op, in.Name); refused {
		return res, nil
	}
	tenant := tools.RunIdentity(ctx).TenantID
	switch in.Op {
	case "create":
		return e.execCreate(ctx, tenant, in)
	case "get":
		return e.execGet(ctx, tenant, in)
	case "list":
		return e.execList(ctx, tenant)
	case "delete":
		return e.execDelete(ctx, tenant, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, get, list, delete)", in.Op)), nil
	}
}

func (e *EgressRouteDef) execCreate(ctx context.Context, tenant string, in egressRouteDefInput) (tools.Result, error) {
	eg := e.Cfg.Workspace.Egress
	if !eg.TenantRoutes {
		return errResult("EgressRouteDef create: tenant routes are off; the operator allows them with workspace.egress.tenant_routes: true"), nil
	}
	if !config.ValidEgressRouteName(in.Name) {
		return errResult(fmt.Sprintf("EgressRouteDef create: %q is not a route name (a lowercase letter, then up to 62 lowercase letters, digits or -)", in.Name)), nil
	}
	// A tenant cannot bind a name an agent definition meant for the
	// operator's route to a URL of its own (RFC GW §6.3).
	if _, taken := eg.Routes[in.Name]; taken {
		return errResult(fmt.Sprintf("EgressRouteDef create: %q is the name of an operator's route; choose another", in.Name)), nil
	}
	route := config.EgressRoute{URL: in.URL, Methods: in.Methods, Credential: in.Credential, Auth: in.Auth, Header: in.Header}
	// One validator with the operator's yaml routes, so a route refused there
	// is not kept by a runtime write.
	if err := config.ValidateEgressRoute(route); err != nil {
		return errResult("EgressRouteDef create: " + err.Error()), nil
	}
	if msg := tenantHostRefusal(eg.TenantHosts, route.URL); msg != "" {
		return errResult("EgressRouteDef create: " + msg), nil
	}
	if route.Credential != "" {
		if msg := e.credentialRefusal(ctx, tenant, route); msg != "" {
			return errResult("EgressRouteDef create: " + msg), nil
		}
	}
	route = route.Resolved()
	row, err := e.Store.EgressRouteDefCreate(ctx, store.EgressRouteDefRow{
		TenantID: tenant, Name: in.Name, URL: route.URL, Methods: route.Methods,
		Credential: route.Credential, Auth: route.Auth, Header: route.Header,
		CreatedBy: routeCreator(ctx),
	})
	var conflict *store.ErrConflict
	if errors.As(err, &conflict) {
		return errResult(fmt.Sprintf("EgressRouteDef create: route %q already exists; delete it first to replace it", in.Name)), nil
	}
	if err != nil {
		return tools.Result{}, fmt.Errorf("EgressRouteDef create: %w", err)
	}
	return jsonResult(map[string]any{"route": egressRouteView(row)})
}

// tenantHostRefusal applies workspace.egress.tenant_hosts (D31): when set,
// the URL's host must be one of them exactly; unset, any host (the network
// guard refuses a private or metadata address at use).
func tenantHostRefusal(hosts []string, raw string) string {
	if len(hosts) == 0 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "url: " + err.Error()
	}
	host := strings.ToLower(u.Hostname())
	if slices.Contains(hosts, host) {
		return ""
	}
	return fmt.Sprintf("url: host %q is not on the operator's workspace.egress.tenant_hosts", host)
}

// credentialRefusal checks the route's credential at creation from metadata
// only (RFC GW §11 "A credential that does not cover the route's URL refuses
// the route's creation"; plan O3): the tenant-scope credential of that name
// must exist, be restricted to URLs, and cover the route's URL. A user- or
// agent-scope credential of the same name, which outranks it for a run, is
// checked again at use by the proxy (internal/workspace/egress), as every
// credential is.
func (e *EgressRouteDef) credentialRefusal(ctx context.Context, tenant string, route config.EgressRoute) string {
	name, _ := credential.ParseRef(route.Credential)
	if e.Credentials == nil {
		return "credential: no credential store is configured, so a credentialed route cannot be checked"
	}
	meta, err := e.Credentials.Get(ctx, credential.Identity{TenantID: tenant, Scope: "tenant", Name: name})
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return fmt.Sprintf("credential: no tenant credential %q; create it first, restricted to URLs covering the route", name)
	case err != nil:
		return fmt.Sprintf("credential: %q could not be read", name)
	case meta.AllowedURLs == nil:
		return fmt.Sprintf("credential: %q is not restricted to URLs; a route sends only a credential whose allowed_urls cover the route's URL", name)
	case !credential.URLAllowed(meta.AllowedURLs, route.URL):
		return fmt.Sprintf("credential: the allowed_urls of %q do not cover %s", name, route.URL)
	}
	return ""
}

// routeCreator names who wrote the route: the principal's subject, or the
// run identity's user on a surface without one.
func routeCreator(ctx context.Context) string {
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.Subject != "" {
		return p.Subject
	}
	return tools.RunIdentity(ctx).UserID
}

func (e *EgressRouteDef) execGet(ctx context.Context, tenant string, in egressRouteDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("EgressRouteDef get: name is required"), nil
	}
	row, err := e.Store.EgressRouteDefGet(ctx, tenant, in.Name)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		// The same answer for another tenant's route as for none.
		return errResult(fmt.Sprintf("EgressRouteDef get: route %q not found", in.Name)), nil
	}
	if err != nil {
		return tools.Result{}, fmt.Errorf("EgressRouteDef get: %w", err)
	}
	return jsonResult(map[string]any{"route": egressRouteView(row)})
}

func (e *EgressRouteDef) execList(ctx context.Context, tenant string) (tools.Result, error) {
	rows, err := e.Store.EgressRouteDefList(ctx, tenant)
	if err != nil {
		return tools.Result{}, fmt.Errorf("EgressRouteDef list: %w", err)
	}
	routes := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		routes = append(routes, egressRouteView(r))
	}
	return jsonResult(map[string]any{"routes": routes, "count": len(routes)})
}

func (e *EgressRouteDef) execDelete(ctx context.Context, tenant string, in egressRouteDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("EgressRouteDef delete: name is required"), nil
	}
	found, err := e.Store.EgressRouteDefDelete(ctx, tenant, in.Name)
	if err != nil {
		return tools.Result{}, fmt.Errorf("EgressRouteDef delete: %w", err)
	}
	if !found {
		return errResult(fmt.Sprintf("EgressRouteDef delete: route %q not found", in.Name)), nil
	}
	return jsonResult(map[string]any{"deleted": true, "name": in.Name})
}

// egressRouteView is a route as get and list report it. writes marks a route
// that may change something upstream, for the listings (RFC GW §3.1).
func egressRouteView(r store.EgressRouteDefRow) map[string]any {
	route := config.EgressRoute{URL: r.URL, Methods: r.Methods, Credential: r.Credential, Auth: r.Auth, Header: r.Header}
	out := map[string]any{
		"name":       r.Name,
		"url":        r.URL,
		"methods":    r.Methods,
		"writes":     route.Writes(),
		"created_by": r.CreatedBy,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.Credential != "" {
		out["credential"] = r.Credential
		out["auth"] = r.Auth
	}
	if r.Header != "" {
		out["header"] = r.Header
	}
	return out
}
