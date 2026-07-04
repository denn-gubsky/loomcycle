package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// F11: operatorCtx must attach the AgentTools wildcard ceiling so an MCP
// `agentdef`/`skilldef` create with a tool-bearing tools overlay
// validates instead of refusing "caller's effective tools not on ctx
// (runtime misconfiguration)". The builtin agentdef test pins that a ["*"]
// ceiling actually accepts a tool-bearing create; this pins that operatorCtx
// provides it (mirroring HTTP substrateAdminCtx). Fails on the pre-fix code,
// where AgentTools(ctx) was nil.
func TestOperatorCtx_GrantsAgentToolsWildcard(t *testing.T) {
	got := tools.AgentTools(operatorCtx(context.Background()))
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("operatorCtx AgentTools = %v, want [*] (F11)", got)
	}
}
