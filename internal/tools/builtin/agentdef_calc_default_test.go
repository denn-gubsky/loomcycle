package builtin

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GY P1 PR 9a: Calc is a default tool every agent holds at run time unless
// its tools remove it. It is never written into a stored list, so a tools
// ceiling (a fork's root, a creator's tools, a member's ACL) never names it:
// each treats it as held, and "-Calc" is a removal authoring accepts.

func TestAgentDefFork_ListingCalcUnderARootWithoutItIsAccepted(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	got := storedTools(t, agentDefOp(t, tool, ctx, "fork", "researcher", `{"tools":["Read","Calc"]}`))
	if !slices.Equal(got, []string{"Read", "Calc"}) {
		t.Errorf("stored tools = %v, want [Read Calc]", got)
	}
}

// The root's -Calc still holds over a fork that lists Calc: listing it widens
// nothing, and the fork keeps its root's removal.
func TestAgentDefFork_ListingCalcUnderARootThatRemovesItKeepsTheRemoval(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Agents["quiet"] = config.AgentDef{Provider: "anthropic", Model: "claude-haiku-4-5", SystemPrompt: "q", Tools: []string{"Read", "-Calc"}}
	got := storedTools(t, agentDefOp(t, tool, ctx, "fork", "quiet", `{"tools":["Read","Calc"]}`))
	if slices.Contains(config.EffectiveTools(got), "Calc") && !slices.Contains(got, "-Calc") {
		t.Errorf("stored tools = %v: the fork dropped its root's -Calc", got)
	}
	if _, removed := config.SplitTools(got); !config.ToolRemoved("Calc", removed) {
		t.Errorf("stored tools = %v: want the root's -Calc kept", got)
	}
}

func TestAgentDefCreateAndFork_AcceptACalcRemoval(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"Read", "Calc"} })
	if got := storedTools(t, agentDefOp(t, tool, ctx, "create", "lean", `{"provider":"anthropic","system_prompt":"x","tools":["Read","-Calc"]}`)); !slices.Equal(got, []string{"Read", "-Calc"}) {
		t.Errorf("create: stored tools = %v, want [Read -Calc]", got)
	}
	if got := storedTools(t, agentDefOp(t, tool, ctx, "fork", "researcher", `{"tools":["Read","-Calc"]}`)); !slices.Equal(got, []string{"Read", "-Calc"}) {
		t.Errorf("fork: stored tools = %v, want [Read -Calc]", got)
	}
}

// A creator whose own run removed Calc may still list it: the new agent would
// hold Calc by default whether or not it is listed.
func TestAgentDefCreate_CalcIsWithinTheCallersCeilingThoughItsToolsLackIt(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"Read"} })
	if got := storedTools(t, agentDefOp(t, tool, ctx, "create", "calcy", `{"provider":"anthropic","system_prompt":"x","tools":["Read","Calc"]}`)); !slices.Equal(got, []string{"Read", "Calc"}) {
		t.Errorf("stored tools = %v, want [Read Calc]", got)
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "wide", `{"provider":"anthropic","system_prompt":"x","tools":["Read","Write"]}`),
		"create Write", "cannot widen")
}

// A member's off-run create is held to the tools its ACL lets it invoke; Calc,
// held by every agent, is not asked about.
func TestAgentDefCreate_MemberCeilingTreatsCalcAsHeld(t *testing.T) {
	tool, _, cleanup := agentDefFixture(t)
	defer cleanup()
	member := tools.WithCallerTier(memberPrincipalCtx(t, "acme"), tools.TierMember)
	member = tools.WithRunIdentity(member, tools.RunIdentityValue{TenantID: "acme", UserID: "bob"})
	member = tools.UpdateRunPolicy(member, func(rp *tools.RunPolicy) {
		rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}}
		rp.Skill = tools.SkillPolicyValue{Granted: true}
		rp.Tools = []string{"*"}
		rp.ACL = narrowMemberACL(t) // invoke narrowed to Read and Context
	})
	const rest = `"provider":"anthropic","system_prompt":"x","memory_scopes":["-*"],"sql_scopes":["-*"],"history_scope":["-*"],"skills":["-*"]`
	if res := agentDefOp(t, tool, member, "create", "wide", `{`+rest+`,"tools":["Write"]}`); !res.IsError {
		t.Fatalf("fixture: the member's narrowed ACL let it grant Write: %s", res.Text)
	}
	if res := agentDefOp(t, tool, member, "create", "calcy", `{`+rest+`,"tools":["Read","Calc"]}`); res.IsError {
		t.Errorf("the member ceiling refused Calc: %s", res.Text)
	}
	if res := agentDefOp(t, tool, member, "create", "lean", `{`+rest+`,"tools":["Read","-Calc"]}`); res.IsError {
		t.Errorf("the member ceiling refused -Calc: %s", res.Text)
	}
}

func TestCheckRegisteredAgent_MemberMayListOrRemoveCalc(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	member := tools.UpdateRunPolicy(memberPrincipalCtx(t, "acme"), func(rp *tools.RunPolicy) { rp.ACL = narrowMemberACL(t) })
	for _, list := range [][]string{{"Read", "Calc"}, {"Read", "-Calc"}} {
		req := connector.RegisterAgentRequest{Name: "calcy", SystemPrompt: "x", Tools: list}
		if err := CheckRegisteredAgent(member, &config.Config{}, st, req, req.Tools); err != nil {
			t.Errorf("a member's registration with %v was refused: %v", list, err)
		}
	}
}

// The static agent's bootstrapped lineage root, which a fork's ceiling and
// content hash come from, carries the yaml's list and no Calc.
func TestAgentDefBootstrapStatic_RowDoesNotCarryCalc(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	row, err := tool.bootstrapStatic(ctx, "researcher", tool.Cfg.Agents["researcher"])
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(row.Definition, &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.Tools, []string{"Read", "WebFetch", "AgentDef"}) || strings.Contains(string(row.Definition), "Calc") {
		t.Errorf("bootstrapped definition %s; want the yaml's tools and no Calc", row.Definition)
	}
}

// A skill needing Calc does not widen an agent that holds it by default; it
// does widen one that removes it.
func TestSkillToolsExceedingAgent_CalcIsTheAgentsUnlessRemoved(t *testing.T) {
	if got := skillToolsExceedingAgent([]string{"Calc"}, nil, config.EffectiveTools([]string{"Read"})); len(got) != 0 {
		t.Errorf("widening = %v, want none", got)
	}
	if got := skillToolsExceedingAgent([]string{"Calc"}, nil, config.EffectiveTools([]string{"Read", "-Calc"})); !slices.Equal(got, []string{"Calc"}) {
		t.Errorf("widening = %v, want [Calc]", got)
	}
}
