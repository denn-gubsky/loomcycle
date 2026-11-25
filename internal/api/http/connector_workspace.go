package http

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// Workspace runs the Workspace tool outside any run, for the principal of a
// session of loomcycle's MCP server (RFC GW D35). The owner of what it opens
// is that principal's token identity, its tenant and subject, taken from the
// authenticated principal on ctx and never from the input. What it may do is
// the operator's workspace.mcp.ops; the tool itself holds the pin and the
// ops it takes over MCP.
func (s *Server) Workspace(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	wcfg := s.cfg().Workspace.Resolved()
	if !wcfg.MCP.Enabled {
		return workspaceTwinRefusal("workspaces_off", "this runtime offers no workspaces over its MCP server (workspace.mcp.enabled is off)"), nil
	}
	p, ok := auth.PrincipalFromContext(ctx)
	// A workspace is the subject's: a caller nothing identifies (open mode,
	// the shared legacy token) has no subject to own one.
	if !ok || p.Legacy || p.TenantID == "" || p.Subject == "" {
		return workspaceTwinRefusal("no_principal", "a workspace opened over the MCP server belongs to the signed-in token's subject; this session has none"), nil
	}
	if !auth.HasScope(p.Scopes, auth.ScopeRunsCreate) {
		return workspaceTwinRefusal("not_granted", "workspaces over the MCP server need the runs:create scope"), nil
	}
	if err := s.AdmitRunRate(ctx, p); err != nil {
		return connector.ToolResult{}, err
	}
	pol := tools.PolicyOf(ctx)
	pol.Workspace = workspaceMCPGrant(wcfg.MCP)
	ctx = tools.WithRunPolicy(ctx, pol)
	ctx = lifecycle.WithPrincipalOwner(ctx, lifecycle.PrincipalOwner(p.TenantID, p.Subject, p.TokenDefID))
	return s.dispatchBuiltin(ctx, "Workspace", input)
}

// workspaceMCPGrant is the grant a session holds: open, the operator's ops,
// and the operator's routes of workspace.mcp.egress, which open may narrow
// (P4 plan O28), and templates. Never share, never an engine floor beyond the
// worker's own.
func workspaceMCPGrant(c config.WorkspaceMCPConfig) tools.WorkspacePolicyValue {
	// Templates (RFC GW D-P6-24, ruled 2026-11-25): the session saves and
	// deletes its own user templates; a tenant one needs a tenant-operator
	// token, checked by the tool.
	g := tools.WorkspacePolicyValue{Open: true, Templates: true, Egress: slices.Clone(c.Egress)}
	for _, op := range c.Ops {
		switch op {
		case config.WorkspaceOpRead:
			g.Read = true
		case config.WorkspaceOpWrite:
			g.Write = true
		case config.WorkspaceOpExec:
			g.Exec = true
		}
	}
	return g
}

// workspaceTwinRefusal is a refusal in the tool's own shape.
func workspaceTwinRefusal(class, detail string) connector.ToolResult {
	b, _ := json.Marshal(map[string]string{"error": class, "detail": detail})
	return connector.ToolResult{Text: string(b), IsError: true}
}
