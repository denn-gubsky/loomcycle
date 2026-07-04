package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// F11: substrateGRPCCtx must attach the AgentTools wildcard ceiling (mirrors
// HTTP substrateAdminCtx + MCP operatorCtx) so a gRPC `agentdef`/`skilldef`
// create with a tool-bearing tools overlay validates instead of
// refusing "caller's effective tools not on ctx". Fails on the pre-fix
// code, where AgentTools(ctx) was nil.
func TestGrpcSubstrate_OperatorCtxGrantsAgentToolsWildcard(t *testing.T) {
	got := tools.AgentTools(substrateGRPCCtx(context.Background()))
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("substrateGRPCCtx AgentTools = %v, want [*] (F11)", got)
	}
}
