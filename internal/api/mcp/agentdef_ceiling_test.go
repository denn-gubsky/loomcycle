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

// fyFixture is an AgentDef tool over a store with a "work" volume, plus a
// tenant operator's and a member's MCP session of tenant acme.
func fyFixture(t *testing.T, memberEff *authz.Effective) (*builtin.AgentDef, *sqlite.Store, context.Context, context.Context) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Volumes: map[string]config.Volume{"work": {Path: t.TempDir(), Mode: "rw"}}}
	tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	op := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}))
	member := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes, Effective: memberEff}))
	return tool, st, op, member
}

func agentDefCall(t *testing.T, tool *builtin.AgentDef, ctx context.Context, body string) (map[string]any, string, bool) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out, res.Text, res.IsError
}

// The reviewer's probe: a member forked the tenant operator's linker (code-js,
// statement methods, tenant crypto, authoring scopes, tenant memory) with its
// own prompt and promote:true, and the active linker became the member's
// version holding every grant. A member's fork may not carry the operator's
// grants, nor become the active version of an operator's agent; a member may
// still fork and promote its own agent.
func TestMCPMemberSession_CannotForkAnOperatorsAgentIntoForce(t *testing.T) {
	tool, st, op, member := fyFixture(t, nil)
	if _, text, isErr := agentDefCall(t, tool, op, `{"op":"create","name":"linker","overlay":{"provider":"code-js","system_prompt":"links","tools":["Document"],
		"statement_methods":["rule"],"crypto_scopes":["tenant"],"agent_def_scopes":["any"],"memory_scopes":["tenant"],"sql_scopes":["tenant"]}}`); isErr {
		t.Fatalf("operator create: %s", text)
	}
	before, err := st.AgentDefGetActive(context.Background(), "acme", "linker")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"op":"fork","name":"linker","promote":true,"overlay":{"system_prompt":"mine now"}}`,
		`{"op":"fork","name":"linker","overlay":{"system_prompt":"mine now"}}`,
	} {
		if _, text, isErr := agentDefCall(t, tool, member, body); !isErr {
			t.Errorf("a member's fork carrying the operator's grants was accepted (%s): %s", body, text)
		}
	}
	// Stripped of the grants it may fork, but not make its fork the linker.
	out, text, isErr := agentDefCall(t, tool, member, `{"op":"fork","name":"linker","overlay":{"system_prompt":"mine now","statement_methods":[],"crypto_scopes":[],"agent_def_scopes":[],"memory_scopes":["user"],"sql_scopes":["user"]}}`)
	if isErr {
		t.Fatalf("a member's stripped, inactive fork was refused: %s", text)
	}
	defID, _ := out["def_id"].(string)
	if _, text, isErr := agentDefCall(t, tool, member, `{"op":"promote","def_id":"`+defID+`"}`); !isErr {
		t.Errorf("a member promoted its fork over the operator's linker: %s", text)
	}
	if _, text, isErr := agentDefCall(t, tool, member, `{"op":"fork","name":"linker","promote":true,"overlay":{"system_prompt":"mine now","statement_methods":[],"crypto_scopes":[],"agent_def_scopes":[],"memory_scopes":["user"],"sql_scopes":["user"]}}`); !isErr {
		t.Errorf("a member's fork promoted over the operator's linker: %s", text)
	}
	if _, text, isErr := agentDefCall(t, tool, member, `{"op":"create","name":"linker","overlay":{"system_prompt":"mine now","tools":["Read"]}}`); !isErr {
		t.Errorf("a member's create took over the operator's linker: %s", text)
	}
	// Retiring the operator's active version clears the pointer for a create to
	// take: refused too.
	if _, text, isErr := agentDefCall(t, tool, member, `{"op":"retire","def_id":"`+before.DefID+`","retired":true}`); !isErr {
		t.Errorf("a member retired the operator's linker: %s", text)
	}
	if after, err := st.AgentDefGetActive(context.Background(), "acme", "linker"); err != nil || after.DefID != before.DefID {
		t.Errorf("the active linker moved: %s → %s (%v)", before.DefID, after.DefID, err)
	}
	// The member's own agent: fork and promote as before.
	if _, text, isErr := agentDefCall(t, tool, member, `{"op":"create","name":"helper","overlay":{"system_prompt":"help","tools":["Read"]}}`); isErr {
		t.Fatalf("member create: %s", text)
	}
	if _, text, isErr := agentDefCall(t, tool, member, `{"op":"fork","name":"helper","promote":true,"overlay":{"system_prompt":"help more"}}`); isErr {
		t.Errorf("a member could not fork and promote its own agent: %s", text)
	}
}

// shell_credentials left unset is EVERY credential the operator lists, so a
// member's shell-holding agent must say which it gets; with no rule naming the
// operator's host credentials, a member may grant none ([]).
func TestMCPMemberSession_ShellCredentialsAreExplicitForAShellAgent(t *testing.T) {
	tool, _, op, member := fyFixture(t, nil)
	for i, c := range []struct {
		overlay string
		ok      bool
	}{
		{`{"system_prompt":"s","tools":["Bash"],"volumes":["work"]}`, false},
		{`{"system_prompt":"s","tools":["Bash"],"volumes":["work"],"shell_credentials":["GITHUB_TOKEN"]}`, false},
		{`{"system_prompt":"s","tools":["Bash"],"volumes":["work"],"shell_credentials":[]}`, true},
		// No shell: nothing to judge, unset stays the norm.
		{`{"system_prompt":"s","tools":["Read"],"volumes":["work"]}`, true},
	} {
		_, text, isErr := agentDefCall(t, tool, member, fmt.Sprintf(`{"op":"create","name":"sh-%d","overlay":%s}`, i, c.overlay))
		if isErr == c.ok {
			t.Errorf("member %s: accepted=%v, want %v (%s)", c.overlay, !isErr, c.ok, text)
		}
		if _, text, isErr := agentDefCall(t, tool, op, fmt.Sprintf(`{"op":"create","name":"op-sh-%d","overlay":%s}`, i, c.overlay)); isErr {
			t.Errorf("the operator was refused %s: %s", c.overlay, text)
		}
	}
}

// When the member's rule set could not be read, its grants are refused rather
// than judged against the class baseline: a store fault must not stand in for
// the member's own denies. Without a resolver (no set, no fault) the baseline
// applies.
func TestMCPMemberSession_AnUnreadableRuleSetGrantsNothing(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Volumes: map[string]config.Volume{"work": {Path: t.TempDir(), Mode: "rw"}}}
	tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	faulted := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes, ACLFault: true}))
	if _, text, isErr := agentDefCall(t, tool, faulted, `{"op":"create","name":"f1","overlay":{"system_prompt":"s","tools":["Read"],"volumes":["work"]}}`); !isErr {
		t.Errorf("a member whose rules could not be read granted a volume: %s", text)
	}
	plain := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes}))
	if _, text, isErr := agentDefCall(t, tool, plain, `{"op":"create","name":"f2","overlay":{"system_prompt":"s","tools":["Read"],"volumes":["work"]}}`); isErr {
		t.Errorf("a member with no resolver was refused its baseline: %s", text)
	}
}

// The other fields a member may not grant: git remotes, schedule and a2a
// authoring scopes, and a history or SQL scope its own rules deny.
func TestMCPMemberSession_RefusedFieldsItsRulesDoNotCover(t *testing.T) {
	base := authz.Baselines()[authz.BaselineMember]
	deny := authz.ACL{Name: "no-tenant-sql", Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"sqlmem:tenant"}}}}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: "bob"},
		Bindings: []authz.Binding{{ACL: base.Name, SubjectKind: authz.SubjectTenant}, {ACL: deny.Name, SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{base.Name: {ACL: base}, deny.Name: {ACL: deny}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	tool, _, _, member := fyFixture(t, eff)
	for i, c := range []struct{ field, overlay string }{
		{"git_remote", `"git_remote":["fetch"]`},
		{"schedule_def_scopes", `"schedule_def_scopes":["any"]`},
		{"a2a_agent_def_scopes", `"a2a_agent_def_scopes":["any"]`},
		{"history_scope", `"history_scope":["tenant"]`},
		{"sql_scopes", `"sql_scopes":["tenant"]`},
	} {
		if _, text, isErr := agentDefCall(t, tool, member, fmt.Sprintf(`{"op":"create","name":"x-%d","overlay":{"system_prompt":"s","tools":["Read"],"volumes":["work"],%s}}`, i, c.overlay)); !isErr || !strings.Contains(text, c.field) {
			t.Errorf("a member granted %s: %s", c.field, text)
		}
	}
}
