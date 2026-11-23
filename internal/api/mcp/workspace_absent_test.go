package mcp

import (
	"strings"
	"testing"
)

// The Workspace tool was kept off loomcycle's MCP server until it was
// settled who owns a workspace opened outside a run (RFC GW P1, ruling Q4).
// D35 settled it: the session's token identity. The one tool that exposes
// workspaces is `workspace`, whose connector owns that rule; this keeps a
// later change from adding another way in by accident.
func TestMCPTools_OnlyTheD35WorkspaceToolExposesWorkspaces(t *testing.T) {
	for name := range handlersByName {
		if strings.Contains(strings.ToLower(name), "workspace") && name != "workspace" {
			t.Errorf("MCP tool %q exposes workspaces outside the workspace tool", name)
		}
	}
	for _, d := range toolDescriptors() {
		if strings.Contains(strings.ToLower(d.Name), "workspace") && d.Name != "workspace" {
			t.Errorf("MCP descriptor %q exposes workspaces outside the workspace tool", d.Name)
		}
	}
}
