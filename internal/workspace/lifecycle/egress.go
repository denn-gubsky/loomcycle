package lifecycle

import (
	"context"
	"net/http"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
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

// Egress answers one request a ready workspace's session sent out: the
// dispatcher's EgressFunc. The grant is the workspace's own: its tenant,
// user and owner run for the credential, and of the operator's routes only
// those named on its row. A name on the row that is no operator route is
// left out, so the proxy refuses it as a route the workspace does not hold.
// Tenant routes (EgressRouteDef) are not resolved here yet.
func (m *Manager) Egress(ctx context.Context, row store.WorkspaceRow, req egress.Request) (*egress.Response, error) {
	if m.proxy == nil {
		return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: "this runtime sends no workspace request out (no egress is configured)"}
	}
	g := egress.Grant{TenantID: row.TenantID, UserID: row.UserID, RunID: row.OwnerRunID, WorkspaceID: row.ID}
	credentialed := false
	for _, name := range row.Egress {
		if rt, ok := m.routes[name]; ok {
			g.Routes = append(g.Routes, rt)
			credentialed = credentialed || rt.Credential != ""
		}
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
	if row.Engine == workspace.EngineHost {
		// The host engine's session reaches its routes at a loopback port
		// of its worker's choosing (engine.Host), which the core does not
		// know: links it rewrites and redirects it relays point at the
		// route relative to the root of whatever address the session asked.
		g.Base = "/"
	}
	return m.proxy.Do(ctx, g, req)
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
