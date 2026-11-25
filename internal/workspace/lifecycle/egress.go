package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// egress.go — the core's gate on a workspace's requests out (RFC GW §6.3, P4).
// The dispatcher hands Egress the workspace's row, read from the store, and
// the request the worker described. The grant is built here, from that row
// and the operator's routes, never from anything the worker sent.

// OperatorRoutes turns the operator's workspace.egress.routes into the
// proxy's routes, sorted by name. A credential is "$cred:<name>" in config
// and the bare name in a route.
func OperatorRoutes(routes map[string]config.EgressRoute) []egress.Route {
	out := make([]egress.Route, 0, len(routes))
	for name, r := range routes {
		r = r.Resolved()
		rt := egress.Route{Name: name, URL: r.URL, Methods: r.Methods, Auth: r.Auth, Header: r.Header}
		if r.Credential != "" {
			cred, ok := credential.ParseRef(r.Credential)
			if !ok {
				// Load refuses such a route; were one to get here, a name
				// that no credential has makes the proxy refuse the route.
				cred = r.Credential
			}
			rt.Credential = cred
		}
		out = append(out, rt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetEgress gives the manager the proxy its workspaces send through and the
// operator's routes. Called once at boot, before any run; without it every
// request out is refused.
func (m *Manager) SetEgress(p *egress.Proxy, routes []egress.Route) {
	m.proxy = p
	m.routes = make(map[string]egress.Route, len(routes))
	for _, r := range routes {
		m.routes[r.Name] = r
	}
}

// TenantRouteStore reads the routes a tenant wrote (EgressRouteDef).
// store.Store satisfies it.
type TenantRouteStore interface {
	EgressRouteDefList(ctx context.Context, tenantID string) ([]store.EgressRouteDefRow, error)
}

// SetTenantRoutes turns tenant routes on (workspace.egress.tenant_routes),
// bounded by hosts (workspace.egress.tenant_hosts; empty for any public
// host). Called at boot, after SetEgress; without it a tenant's route is
// not one any workspace holds.
func (m *Manager) SetTenantRoutes(st TenantRouteStore, hosts []string) {
	m.tenantRoutes = st
	m.tenantHosts = slices.Clone(hosts)
}

// Egress answers one request a ready workspace's session sent out: the
// dispatcher's EgressFunc. The grant is the workspace's own: its tenant,
// user and owner run for the credential, and the routes named on its row.
// A name is the operator's route when there is one (plan O14: it wins a
// name a tenant also took), else, with tenant routes on, the workspace's
// own tenant's route of that name, checked again here as at its creation.
// A name that is neither is left out, so the proxy refuses it as a route the
// workspace does not hold.
func (m *Manager) Egress(ctx context.Context, row store.WorkspaceRow, req egress.Request) (*egress.Response, error) {
	if m.proxy == nil {
		return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: "this runtime sends no workspace request out (no egress is configured)"}
	}
	if row.OwnerKind == store.WorkspaceOwnerPrincipal {
		// A workspace opened over the MCP server (RFC GW D35) is granted no
		// route at open; refused here too, so a row that somehow names one
		// still sends nothing.
		return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: "a workspace opened over the MCP server has no egress routes"}
	}
	g := egress.Grant{TenantID: row.TenantID, UserID: row.UserID, RunID: row.OwnerRunID, WorkspaceID: row.ID}
	var tenantNames []string
	for _, name := range row.Egress {
		if rt, ok := m.routes[name]; ok {
			g.Routes = append(g.Routes, rt)
		} else {
			tenantNames = append(tenantNames, name)
		}
	}
	if len(tenantNames) > 0 && m.tenantRoutes != nil {
		routes, ref := m.tenantRoutesFor(ctx, row.TenantID, tenantNames, req.Route)
		if ref != nil {
			return nil, ref
		}
		g.Routes = append(g.Routes, routes...)
	}
	credentialed := false
	for _, rt := range g.Routes {
		credentialed = credentialed || rt.Credential != ""
	}
	if credentialed {
		// An agent-scoped credential is the agent's: the owner run's agent
		// name decides it. One that cannot be read refuses the request
		// rather than resolving without it, which could fall back to
		// another scope's credential of the same name.
		run, err := m.store.GetRun(ctx, row.OwnerRunID)
		if err != nil || run.TenantID != row.TenantID {
			return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: "the workspace's run cannot be read to resolve a route's credential; try again"}
		}
		g.AgentName = run.Agent
	}
	return m.proxy.Do(ctx, g, req)
}

// tenantRoutesFor reads the tenant's routes named on the row: only that
// tenant's, never another's of the same name. Each is held again to the
// rules its creation checked (tenant_hosts as the operator has it now, port
// 443, a credential reference); a route that fails them is refused when it
// is the one asked for, and otherwise left out (a redirect to it is then
// refused too). A store fault refuses the request: nothing goes out on a
// guess.
func (m *Manager) tenantRoutesFor(ctx context.Context, tenantID string, names []string, asked string) ([]egress.Route, *egress.Refusal) {
	rows, err := m.tenantRoutes.EgressRouteDefList(ctx, tenantID)
	if err != nil {
		m.log.Warn("cannot read the tenant's egress routes", "tenant", tenantID, "err", err)
		return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: "the tenant's egress routes cannot be read; try again"}
	}
	var out []egress.Route
	for _, r := range rows {
		if r.TenantID != tenantID || !slices.Contains(names, r.Name) {
			continue
		}
		rt, reason := m.tenantRoute(r)
		if reason != "" {
			if r.Name == asked {
				return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: fmt.Sprintf("route %q: %s", r.Name, reason)}
			}
			continue
		}
		out = append(out, rt)
	}
	return out, nil
}

// tenantRoute is a stored tenant route as the proxy takes it, or the reason
// it may not be used.
func (m *Manager) tenantRoute(r store.EgressRouteDefRow) (egress.Route, string) {
	u, err := url.Parse(r.URL)
	if err != nil || u.Hostname() == "" {
		return egress.Route{}, "the stored URL is not valid"
	}
	if p := u.Port(); p != "" && p != "443" && p != m.tenantTestPort {
		return egress.Route{}, fmt.Sprintf("port %s is not allowed; a tenant route uses https on port 443 only", p)
	}
	if len(m.tenantHosts) > 0 && !slices.Contains(m.tenantHosts, strings.ToLower(u.Hostname())) {
		return egress.Route{}, fmt.Sprintf("host %q is not on the operator's workspace.egress.tenant_hosts", strings.ToLower(u.Hostname()))
	}
	rt := egress.Route{Name: r.Name, URL: r.URL, Methods: slices.Clone(r.Methods), Auth: r.Auth, Header: r.Header, Tenant: true}
	if r.Credential != "" {
		name, ok := credential.ParseRef(r.Credential)
		if !ok {
			return egress.Route{}, "the stored credential is not a $cred: reference"
		}
		rt.Credential = name
	}
	return rt, ""
}

// RecordEgress writes the audit line of one request through a credentialed
// or tenant-written route: the proxy's Audit. It reads the sink SetAudit set.
func (m *Manager) RecordEgress(l egress.Line) {
	if m.audit == nil {
		return
	}
	_ = m.audit.Record(audit.Event{
		Action: "workspace_egress", TargetTenant: l.TenantID,
		WorkspaceID: l.WorkspaceID, WorkspaceOwnerRunID: l.RunID,
		EgressRoute: l.Route, EgressMethod: l.Method, EgressPath: l.Path,
		EgressStatus: l.Status, EgressBytes: l.Bytes, EgressRefused: l.Refused,
	}) // best effort, as for shares: a sink that fails does not refuse the request
}

// forgetEgress drops what the proxy counts for a released workspace.
func (m *Manager) forgetEgress(id string) {
	if m.proxy != nil {
		m.proxy.Forget(id)
	}
}
