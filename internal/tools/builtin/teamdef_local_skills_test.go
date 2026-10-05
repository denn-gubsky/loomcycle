package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// skillTeamFixture is localTeamFixture with the SkillDef tool wired too, as
// main.go wires it, and a caller who may also use the Skill tool.
func skillTeamFixture(t *testing.T) (*TeamDef, context.Context) {
	t.Helper()
	tool, _ := localTeamFixture(t)
	tool.Skills = &SkillDef{Store: tool.Store, MaxBodyBytes: 4096}
	return tool, localAuthorCtx("", []string{"any"}, []string{"Read", "Grep", "Agent", "Skill"})
}

// skillTeam is a one-state team whose own agent "reviewer" (tools agentTools)
// is granted the team's skill "style" (skill).
func skillTeam(agentTools, skill string) string {
	return `{"entry":"review",
	  "local":{"agents":{"reviewer":{"tier":"middle","tools":` + agentTools + `,"skills":["./style"]}},
	           "skills":{"style":` + skill + `}},
	  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},
	            {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"review","to":"done","on":"success"}]}`
}

func TestTeamDefCreate_StoresLocalSkillOnlyInTheTeam(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	res := teamOp(t, tool, ctx, "create", "sdlc", skillTeam(`["Read","Skill"]`, `{"body":"Prefer short functions.","tools":["Read"]}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	row, err := tool.Store.TeamDefGetActive(ctx, "", "sdlc")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		t.Fatalf("stored definition: %v", err)
	}
	if sk, ok := def.LocalSkill("style"); !ok || sk.Body != "Prefer short functions." {
		t.Fatalf("the stored definition lost its local skill: %s", row.Definition)
	}
	for _, name := range []string{"style", "sdlc/style"} {
		if _, err := tool.Store.SkillDefGetActive(ctx, "", name); err == nil {
			t.Errorf("a team's own skill was written to skill_defs as %q", name)
		}
	}
}

// One refused skill per gate SkillDef create applies. Each must be refused for
// a team's own skill exactly as for a global one — the two share gateNewSkill —
// so every row is run through both tools.
func TestTeamDefCreate_LocalSkillPassesEverySkillDefCreateGate(t *testing.T) {
	for _, tc := range []struct {
		gate    string
		ctx     func(context.Context) context.Context
		skill   string
		mention string
	}{
		{"tools ceiling", nil, `{"body":"b","tools":["Bash"]}`, "Tools cannot widen"},
		{"blank body", nil, `{"body":"  "}`, "overlay.body is required"},
		{"body size cap", nil, `{"body":"` + strings.Repeat("x", 5000) + `"}`, "LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES"},
		{"skills allowlist: name outside it",
			func(c context.Context) context.Context {
				return tools.WithSkillPolicy(c, tools.SkillPolicyValue{Patterns: []string{"other/*"}})
			},
			`{"body":"b"}`, "allowlist"},
		{"skills allowlist: deny all",
			func(c context.Context) context.Context {
				return tools.WithSkillPolicy(c, tools.SkillPolicyValue{Patterns: []string{"-*"}})
			},
			`{"body":"b"}`, "allowlist"},
		{"tools ceiling unknown",
			func(c context.Context) context.Context { return tools.WithAgentTools(c, nil) },
			`{"body":"b","tools":["Read"]}`, "effective tools not on ctx"},
	} {
		t.Run(tc.gate, func(t *testing.T) {
			tool, ctx := skillTeamFixture(t)
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			// The gate exists for a global skill of that name...
			global, _ := tool.Skills.Execute(ctx, json.RawMessage(`{"op":"create","name":"sdlc/style","overlay":`+tc.skill+`}`))
			wantRefused(t, global, "SkillDef create", tc.mention)
			// ...and a team's own skill does not get past it. No agent is
			// granted it, so this is the skill's own gate and nothing else.
			team := strings.Replace(validTeamGraph, `"entry"`, `"local":{"skills":{"style":`+tc.skill+`}},"entry"`, 1)
			wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", team), "TeamDef create", tc.mention, `local.skills["style"]`)
			if _, err := tool.Store.TeamDefGetActive(ctx, "", "sdlc"); err == nil {
				t.Error("a refused team was stored")
			}
		})
	}
}

// The allowlist is judged under the skill's FULL name, so an author allowed a
// team's subtree may write that team's skills and no other team's.
func TestTeamDefCreate_SkillAllowlistIsJudgedUnderTheTeamQualifiedName(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"sdlc/*"}})
	team := skillTeam(`["Read","Skill"]`, `{"body":"b"}`)
	if res := teamOp(t, tool, ctx, "create", "sdlc", team); res.IsError {
		t.Fatalf("sdlc/* covers sdlc/style, but create was refused: %s", res.Text)
	}
	wantRefused(t, teamOp(t, tool, ctx, "create", "other", team), "team outside the allowlist", "other/style")
}

// A skill may not need a tool the agent granted it lacks: the Skill tool would
// refuse to load it, so the team is refused when it is written — at create,
// and at a fork that narrows either side (below).
func TestTeamDefCreate_RefusesLocalSkillWiderThanAnAgentGrantedIt(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", skillTeam(`["Read","Skill"]`, `{"body":"b","tools":["Grep"]}`)),
		"skill wider than its agent", `local.agents["reviewer"]`, `"./style"`, "Grep")

	// Covered by an agent glob, as at invoke (by an author whose own tools
	// cover that glob).
	wide := localAuthorCtx("", []string{"any"}, []string{"*"})
	if res := teamOp(t, tool, wide, "create", "globbed", skillTeam(`["Read","Skill","G*"]`, `{"body":"b","tools":["Grep"]}`)); res.IsError {
		t.Fatalf("an agent tool glob covering the skill's tool was refused: %s", res.Text)
	}
}

func TestTeamDefFork_RefusesLocalSkillWiderThanAnAgentGrantedIt(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	if res := teamOp(t, tool, ctx, "create", "sdlc", skillTeam(`["Read","Grep","Skill"]`, `{"body":"b","tools":["Grep"]}`)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	narrowAgent := `{"local":{"agents":{"reviewer":{"tier":"middle","tools":["Read","Skill"],"skills":["./style"]}}}}`
	wantRefused(t, teamOp(t, tool, ctx, "fork", "sdlc", narrowAgent), "fork narrowing the agent", "Grep")
	widenSkill := `{"local":{"skills":{"style":{"body":"b","tools":["Grep","Read","Agent"]}}}}`
	if res := teamOp(t, tool, ctx, "fork", "sdlc", widenSkill); !res.IsError || !strings.Contains(res.Text, "Agent") {
		t.Errorf("fork widening the skill past its agent: %s, want refused naming Agent", res.Text)
	}
}

// A fork is an authoring act by whoever forks, so the skills it carries over
// are judged under the FORKER's allowlist and tools, as its agents are.
func TestTeamDefFork_InheritedLocalSkillsAreGatedAgainstTheForker(t *testing.T) {
	tool, operator := skillTeamFixture(t)
	team := strings.Replace(validTeamGraph, `"entry"`, `"local":{"skills":{"style":{"body":"b","tools":["Grep"]}}},"entry"`, 1)
	if res := teamOp(t, tool, operator, "create", "sdlc", team); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	const noLocal = `{"max_iterations":3}`

	narrow := localAuthorCtx("", []string{"any"}, []string{"Read"})
	wantRefused(t, teamOp(t, tool, narrow, "fork", "sdlc", noLocal), "fork by a caller without Grep", "Tools cannot widen", "Grep")

	outside := tools.WithSkillPolicy(operator, tools.SkillPolicyValue{Patterns: []string{"other/*"}})
	wantRefused(t, teamOp(t, tool, outside, "fork", "sdlc", noLocal), "fork by a caller whose allowlist misses the skill", "allowlist", "sdlc/style")

	rows, err := tool.Store.TeamDefListByName(operator, "sdlc")
	if err != nil || len(rows) != 1 {
		t.Fatalf("a refused fork must write nothing; versions = %d (err %v)", len(rows), err)
	}
	if res := teamOp(t, tool, operator, "fork", "sdlc", noLocal); res.IsError {
		t.Fatalf("fork by the original author: %s", res.Text)
	}
}

func TestTeamDefCreate_RefusesUndeclaredLocalSkillGrant(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	team := strings.Replace(skillTeam(`["Read","Skill"]`, `{"body":"b"}`), `"skills":["./style"]`, `"skills":["./ghost"]`, 1)
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", team), "grant of an undeclared skill", `"./ghost"`, "does not declare")
}

func TestTeamDefCreate_RefusesLocalSkillsWhenNoSkillDefToolIsWired(t *testing.T) {
	tool, ctx := skillTeamFixture(t)
	tool.Skills = nil
	wantRefused(t, teamOp(t, tool, ctx, "create", "sdlc", skillTeam(`["Read","Skill"]`, `{"body":"b"}`)), "no gates wired", "skill-definition tool")
}

// A restore re-runs only the body's own checks — those a restored skill def
// gets.
func TestValidateTeamDefBody_ChecksLocalSkillBodies(t *testing.T) {
	if err := ValidateTeamDefBody(json.RawMessage(skillTeam(`["Read","Skill"]`, `{"body":"b","tools":["Read"]}`))); err != nil {
		t.Fatalf("a well-formed local skill: %v", err)
	}
	err := ValidateTeamDefBody(json.RawMessage(skillTeam(`["Read","Skill"]`, `{"body":"   "}`)))
	if err == nil || !strings.Contains(err.Error(), `local.skills["style"]`) {
		t.Errorf("a blank local skill body at restore: %v, want refused naming the skill", err)
	}
}
