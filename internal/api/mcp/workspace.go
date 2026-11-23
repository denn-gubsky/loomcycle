package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The `workspace` meta-tool is the Workspace tool outside a run (RFC GW
// D35): the workspaces belong to the session's token identity, its tenant
// and subject. connector.Workspace owns every rule (the operator's switch
// and ops, the principal, the run rate); the tool holds the pin. This file
// only advertises it.

func workspaceToolDescriptor() loommcp.ToolDescriptor {
	schema, ok := builtin.WorkspaceMCPSchema()
	if !ok {
		schema = rawJSON(`{"type": "object", "properties": {"op": {"type": "string"}, "name": {"type": "string"}}, "required": ["op"]}`)
	}
	return loommcp.ToolDescriptor{Name: "workspace", Description: workspaceMCPDescription, InputSchema: schema}
}

// workspaceMCPDescription is what an MCP client reads. Keep its op list in
// step with builtin.WorkspaceMCPOps: TestWorkspaceMCP_DescriptionListsEveryOp
// checks it.
const workspaceMCPDescription = "Open a workspace — a directory on a workspace worker, away from the runtime — and run shell commands and file operations in it, from outside any run. " +
	"Ops: open (`name`, optional `toolchain`), list, status, release; exec (`command`, optional `cwd`, `timeout_s`); " +
	"read, write, edit, glob, grep with the same arguments as the Read, Write, Edit, Glob and Grep tools (no `volume`), paths relative to the workspace. " +
	"A workspace opened here belongs to the token you signed in with — its tenant and subject — not to a run: every session of that subject reaches it, nobody else does, and no run sees it. " +
	"It is deleted when you release it, after an hour without an op, a day after it opened, or when the token that opened it is retired or expires (the operator sets the two times): copy out what you need to keep with read. " +
	"Off unless the operator enables it (workspace.mcp); which of read, write and exec you may use is the operator's; you need the runs:create scope, and each call takes one unit of your token's run rate. " +
	"Sharing, interpreters and servers are done inside a run. " +
	"Do NOT use it as storage: it is a scratch directory that goes away."

// handleWorkspace forwards to connector.Workspace under the session's
// principal, which owns what it opens.
func handleWorkspace(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("workspace: no connector wired")
	}
	res, err := env.connector.Workspace(mcpPrincipalCtx(ctx), args)
	if err != nil {
		if errors.Is(err, runner.ErrRateLimited) {
			return runRateSpent("workspace", err), nil
		}
		return toolErrFrom("workspace", err), nil
	}
	return toolResultFromConnector(res), nil
}
