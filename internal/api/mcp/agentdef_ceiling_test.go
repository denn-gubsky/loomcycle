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

// F11: operatorCtx must attach the tools-ceiling wildcard ceiling so an MCP
// `agentdef`/`skilldef` create with a tool-bearing tools overlay
// validates instead of refusing "caller's effective tools not on ctx
// (runtime misconfiguration)". The builtin agentdef test pins that a ["*"]
// ceiling actually accepts a tool-bearing create; this pins that operatorCtx
// provides it (mirroring HTTP substrateAdminCtx). Fails on the pre-fix code,
// where PolicyOf(ctx).Tools was nil.
func TestOperatorCtx_GrantsAgentToolsWildcard(t *testing.T) {
	got := tools.PolicyOf(operatorCtx(context.Background())).Tools
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("operatorCtx Tools = %v, want [*] (F11)", got)
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

// A member's session authors definitions as RFC CB lets it: the capability
// ceiling, which no substrate plane stamps a member's policies for, does not
// apply off-run — but what it writes is not the operator's. Before the split, a
// member's create naming a volume, or any create where the operator configured
// a default volume, was refused against an empty ceiling.
func TestMCPMemberSession_AuthorsDefinitionsButNotAsTheOperator(t *testing.T) {
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	member := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes}))
	op := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}))
	for name, vol := range map[string]config.Volume{
		"named volume":   {Path: t.TempDir(), Mode: "rw"},
		"default volume": {Path: t.TempDir(), Mode: "rw", Default: true},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := sqlite.Open(":memory:")
			if err != nil {
				t.Fatalf("sqlite.Open: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			// The operator's default volume is the one NAMED default: an agent that
			// declares no volumes binds it.
			volName := "work"
			if vol.Default {
				volName = "default"
			}
			cfg := &config.Config{Volumes: map[string]config.Volume{volName: vol}}
			tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
			overlay := `{"tools":["Read"]}`
			if !vol.Default {
				overlay = `{"tools":["Read"],"volumes":["work"]}`
			}
			for who, ctx := range map[string]context.Context{"member": member, "operator": op} {
				res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"by-`+who+`","overlay":`+overlay+`}`))
				if err != nil || res.IsError {
					t.Fatalf("the %s's create was refused: %v %s", who, err, res.Text)
				}
			}
			for who, want := range map[string]bool{"member": false, "operator": true} {
				row, err := st.AgentDefGetActive(context.Background(), "acme", "by-"+who)
				if err != nil {
					t.Fatalf("read by-%s: %v", who, err)
				}
				if row.OperatorAuthored != want {
					t.Errorf("by-%s operator_authored = %v, want %v", who, row.OperatorAuthored, want)
				}
			}
		})
	}
}
