package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
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

// A member may not grant what it does not hold itself: the operator-grade
// fields (statement methods, tenant crypto keys, the authoring scopes) are
// refused, and a volume its own rules do not let it mount is refused; the
// tenant operator's session writes each of them.
func TestMCPMemberSession_CannotGrantOperatorGradeCapabilities(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Volumes: map[string]config.Volume{"work": {Path: t.TempDir(), Mode: "rw"}, "vault": {Path: t.TempDir(), Mode: "rw"}}}
	tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	// The member's own rules: the member baseline, less mounting the vault.
	base := authz.Baselines()[authz.BaselineMember]
	deny := authz.ACL{Name: "no-vault", Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"mount"}, Resources: []string{"volume:vault"}}}}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: "bob"},
		Bindings: []authz.Binding{{ACL: base.Name, SubjectKind: authz.SubjectTenant}, {ACL: deny.Name, SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{base.Name: {ACL: base}, deny.Name: {ACL: deny}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	member := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes, Effective: eff}))
	op := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}))
	for i, c := range []struct{ field, overlay string }{
		{"statement_methods", `{"provider":"code-js","system_prompt":"links","tools":["Document"],"volumes":["work"],"statement_methods":["judge"]}`},
		{"crypto_scopes", `{"tools":["Read"],"volumes":["work"],"crypto_scopes":["tenant"]}`},
		{"agent_def_scopes", `{"tools":["Read"],"volumes":["work"],"agent_def_scopes":["any"]}`},
		{"volume_def_scopes", `{"tools":["Read"],"volumes":["work"],"volume_def_scopes":["any"]}`},
		{"volumes", `{"tools":["Read"],"volumes":["vault"]}`},
	} {
		create := func(ctx context.Context, who string) (string, bool) {
			res, err := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"create","name":"%s-%d","overlay":%s}`, who, i, c.overlay)))
			if err != nil {
				t.Fatal(err)
			}
			return res.Text, res.IsError
		}
		if text, isErr := create(member, "member"); !isErr || !strings.Contains(text, c.field) || !strings.Contains(text, "ask an operator to author") {
			t.Errorf("a member granted %s: %s", c.field, text)
		}
		if text, isErr := create(op, "op"); isErr {
			t.Errorf("the tenant operator was refused %s: %s", c.field, text)
		}
	}
}
