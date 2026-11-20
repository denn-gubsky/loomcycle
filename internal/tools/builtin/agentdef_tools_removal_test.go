package builtin

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GY: a "-Name" entry in tools removes a default tool (Context). These
// pin how the AgentDef tool and register_agent author it.

func storedTools(t *testing.T, res tools.Result) []string {
	t.Helper()
	if res.IsError {
		t.Fatalf("refused: %s", res.Text)
	}
	var out struct {
		Definition struct {
			Tools []string `json:"tools"`
		} `json:"definition"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %s: %v", res.Text, err)
	}
	return out.Definition.Tools
}

// grantsContextAtResolve is whether a definition holding tools resolves to a
// Context grant, by the rule every resolver applies.
func grantsContextAtResolve(list []string) bool {
	def := config.AgentDef{Tools: list}
	config.AddContextToolDefault(&def)
	grants, removed := config.SplitTools(def.Tools)
	return slices.Contains(grants, "Context") && !config.ToolRemoved("Context", removed)
}

func TestAgentDefFork_AddingARemovalUnderARootWithoutItIsAccepted(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	got := storedTools(t, agentDefOp(t, tool, ctx, "fork", "researcher", `{"tools":["Read","-Context"]}`))
	if !slices.Equal(got, []string{"Read", "-Context"}) {
		t.Errorf("stored tools = %v, want [Read -Context]", got)
	}
}

func TestAgentDefFork_KeepsTheRootsRemovalWhenTheOverlayOmitsIt(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Agents["quiet"] = config.AgentDef{Provider: "anthropic", Model: "claude-haiku-4-5", SystemPrompt: "q", Tools: []string{"*", "-Context"}}
	got := storedTools(t, agentDefOp(t, tool, ctx, "fork", "quiet", `{"tools":["Read"]}`))
	if !slices.Contains(got, "-Context") || grantsContextAtResolve(got) {
		t.Errorf("stored tools = %v: the fork dropped its root's -Context", got)
	}
	// A fork that restates Context explicitly under "*" still does not get it.
	got = storedTools(t, agentDefOp(t, tool, ctx, "fork", "quiet", `{"tools":["Read","Context"]}`))
	if grantsContextAtResolve(got) {
		t.Errorf("stored tools = %v: an explicit Context undid the root's -Context", got)
	}
}

func TestAgentDefCreate_RemovalIsNotHeldToTheCallersTools(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"Read"} })
	got := storedTools(t, agentDefOp(t, tool, ctx, "create", "lean", `{"provider":"anthropic","system_prompt":"x","tools":["Read","-Context"]}`))
	if !slices.Equal(got, []string{"Read", "-Context"}) || grantsContextAtResolve(got) {
		t.Errorf("stored tools = %v, want [Read -Context] without Context", got)
	}
}

func TestAgentDefCreateAndFork_RefuseARemovalOfANonDefaultTool(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"Read", "Skill"} })
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "lean", `{"provider":"anthropic","system_prompt":"x","tools":["Read","-Read"]}`),
		"create -Read", "`-Read`", "only a default tool")
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "lean", `{"provider":"anthropic","system_prompt":"x","tools":["Read","-Skill"]}`),
		"create -Skill", "`-Skill`", "skills: [-*]")
	wantRefused(t, agentDefOp(t, tool, ctx, "fork", "researcher", `{"tools":["Read","-WebFetch"]}`),
		"fork -WebFetch", "`-WebFetch`", "only a default tool")
	wantRefused(t, agentDefOp(t, tool, ctx, "fork", "researcher", `{"tools":["-skill"]}`),
		"fork -skill", "skills: [-*]")
}

// narrowMemberACL is the baseline member set with tool invocation narrowed to
// Read and Context: a member whose ceiling names its tools, so an entry the
// ceiling asks about by mistake is refused rather than matched by tool:*.
func narrowMemberACL(t *testing.T) *authz.Effective {
	t.Helper()
	acl := authz.Baselines()[authz.BaselineMember]
	rules := make([]authz.Rule, 0, len(acl.Rules))
	for _, r := range acl.Rules {
		r.Resources = slices.Clone(r.Resources)
		if i := slices.Index(r.Resources, "tool:*"); i >= 0 && slices.Contains(r.Actions, "invoke") {
			r.Resources[i] = "tool:Read"
			r.Resources = append(r.Resources, "tool:Context")
		}
		rules = append(rules, r)
	}
	acl.Rules = rules
	eff, err := authz.Compile(authz.Input{
		Subject:  authz.Subject{TenantID: "acme", ID: "bob"},
		Bindings: []authz.Binding{{ACL: "narrow", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"narrow": {ACL: acl}},
		Now:      time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// A member's off-run create is held to the tools the member may invoke, asked
// of the ACL engine per entry; a removal is no tool to ask about.
func TestAgentDefCreate_MemberCeilingDoesNotJudgeARemoval(t *testing.T) {
	tool, _, cleanup := agentDefFixture(t)
	defer cleanup()
	member := tools.WithCallerTier(memberPrincipalCtx(t, "acme"), tools.TierMember)
	member = tools.WithRunIdentity(member, tools.RunIdentityValue{TenantID: "acme", UserID: "bob"})
	member = tools.UpdateRunPolicy(member, func(rp *tools.RunPolicy) {
		rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}}
		rp.Skill = tools.SkillPolicyValue{Granted: true}
		rp.Tools = []string{"*"}
		rp.ACL = narrowMemberACL(t)
	})
	if res := agentDefOp(t, tool, member, "create", "wide", `{"provider":"anthropic","system_prompt":"x","tools":["Write"]}`); !res.IsError {
		t.Fatalf("fixture: the member's narrowed ACL let it grant Write: %s", res.Text)
	}
	const rest = `"provider":"anthropic","system_prompt":"x","memory_scopes":["-*"],"sql_scopes":["-*"],"history_scope":["-*"],"skills":["-*"]`
	if res := agentDefOp(t, tool, member, "create", "plain", `{`+rest+`,"tools":["Read"]}`); res.IsError {
		t.Fatalf("fixture: the member cannot create an agent with Read: %s", res.Text)
	}
	if res := agentDefOp(t, tool, member, "create", "lean", `{`+rest+`,"tools":["Read","-Context"]}`); res.IsError {
		t.Errorf("the member ceiling refused -Context: %s", res.Text)
	}
}

func TestCheckRegisteredAgent_MemberMayRegisterARemoval(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	req := connector.RegisterAgentRequest{Name: "lean", SystemPrompt: "x", Tools: []string{"Read", "-Context"}}
	member := tools.UpdateRunPolicy(memberPrincipalCtx(t, "acme"), func(rp *tools.RunPolicy) { rp.ACL = narrowMemberACL(t) })
	if err := CheckRegisteredAgent(member, &config.Config{}, st, req, req.Tools); err != nil {
		t.Errorf("a member's registration with -Context was refused: %v", err)
	}
}

// A skill needing a tool the agent's removal takes away widens the agent,
// though a glob in its tools would cover the name.
func TestSkillToolsExceedingAgent_RemovedToolIsNotTheAgents(t *testing.T) {
	if got := skillToolsExceedingAgent([]string{"Read", "Context"}, nil, []string{"*", "-Context"}); !slices.Equal(got, []string{"Context"}) {
		t.Errorf("widening = %v, want [Context]", got)
	}
}
