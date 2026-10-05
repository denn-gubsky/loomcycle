package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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

// The capability ceiling recognises an MCP operator session — stdio/open mode
// and an authenticated tenant operator alike — as the operator plane, so its
// authoring is not narrowed by an agent's policies.
func TestMCPOperatorCtx_IsNotNarrowedByTheCapabilityCeiling(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Volumes: map[string]config.Volume{"work": {Path: t.TempDir(), Mode: "rw"}}}
	tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	for name, ctx := range map[string]context.Context{
		"stdio":           operatorCtx(context.Background()),
		"tenant-operator": mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})),
	} {
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"wide-`+name+`","overlay":{"volumes":["work"],"interruption":{"enabled":true},"sql_scopes":["tenant"]}}`))
		if err != nil || res.IsError {
			t.Errorf("%s: the operator was refused: %v %s", name, err, res.Text)
		}
	}
}
