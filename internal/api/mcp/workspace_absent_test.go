package mcp

import (
	"strings"
	"testing"
)

// The Workspace tool is deliberately NOT on loomcycle's MCP server (RFC GW
// P1, ruling Q4): a workspace is pinned to the run that opened it and
// released when that run ends, and an MCP session is not a run. Who would
// own a workspace opened from a session, and when it would go, is settled
// before it is offered here. This keeps a later change from adding it by
// accident.
func TestMCPTools_WorkspaceIsDeliberatelyAbsent(t *testing.T) {
	for name := range handlersByName {
		if strings.Contains(strings.ToLower(name), "workspace") {
			t.Errorf("MCP tool %q exposes workspaces; settle who owns a workspace opened outside a run first", name)
		}
	}
	for _, d := range toolDescriptors() {
		if strings.Contains(strings.ToLower(d.Name), "workspace") {
			t.Errorf("MCP descriptor %q exposes workspaces", d.Name)
		}
	}
}
