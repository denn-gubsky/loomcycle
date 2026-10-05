package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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

// The capability ceiling recognises the gRPC substrate plane as the operator,
// so authoring over it is not narrowed by an agent's policies — including for
// a tenant operator within its own tenant.
func TestSubstrateGRPCCtx_IsNotNarrowedByTheCapabilityCeiling(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Volumes: map[string]config.Volume{"work": {Path: t.TempDir(), Mode: "rw"}}}
	tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	for name, ctx := range map[string]context.Context{
		"open-mode":       substrateGRPCCtx(context.Background()),
		"tenant-operator": substrateGRPCCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})),
	} {
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"wide-`+name+`","overlay":{"volumes":["work"],"interruption":{"enabled":true},"sql_scopes":["tenant"]}}`))
		if err != nil || res.IsError {
			t.Errorf("%s: the operator was refused: %v %s", name, err, res.Text)
		}
	}
}
