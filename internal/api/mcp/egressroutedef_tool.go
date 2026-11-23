package mcp

import loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"

// egressRouteDefToolDescriptor is the tools/list entry of the MCP twin of
// POST /v1/_egressroutedef (RFC GW P4): a tenant operator's workspace egress
// routes. Its own file, like clientdef's, so the base table stays in its
// length budget. Model-visible text: no design-document letters.
func egressRouteDefToolDescriptor() loommcp.ToolDescriptor {
	return loommcp.ToolDescriptor{
		Name:        "egressroutedef",
		Description: "Write the EGRESS ROUTES your tenant's workspaces may reach the network through: a workspace has no network but these, and a script in it calls http://egress/<name>/<path>. Ops: create, get, list, delete. A route is a name, an https upstream URL ending in /, the methods allowed (default GET, HEAD; a route with a write method lists as writes: true) and optionally a stored credential ($cred:<name>) the runtime adds on the way out, so no secret enters the workspace. Off unless the operator turns tenant routes on, and bounded by the operator's host list. A credential must be a tenant credential whose allowed_urls cover the route's URL. A route cannot take the name of an operator's route. Flat, like volumedef: no fork or promote; replace a route by delete then create. Confined to your own tenant, and to tenant operators: a member token will not see this tool, and no agent can write a route. Do NOT use it to give an agent a route: that is the agent definition's workspace.egress grant (agentdef), which names routes written here or by the operator; and do NOT use it to store the secret itself — that is credentialdef.",
		InputSchema: builtinSchema("egressroutedef"),
	}
}
