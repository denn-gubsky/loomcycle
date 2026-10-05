package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamSkillTool is a Skill tool over the static doc/* set whose caller's team
// declares "style" (needs Read) — inTeam says whether the caller is one of
// that team's own agents.
func teamSkillTool(t *testing.T, inTeam bool) *SkillTool {
	t.Helper()
	return &SkillTool{Set: groupedSet(t), TeamSkills: func(context.Context) (map[string]teamgraph.LocalSkill, bool, error) {
		if !inTeam {
			return nil, false, nil
		}
		return map[string]teamgraph.LocalSkill{
			"style": {Body: "TEAM STYLE", Description: "House style", Tools: []string{"Read"}},
		}, true, nil
	}}
}

func teamSkillCtx(skills ...string) context.Context {
	ctx := tools.WithAgentTools(context.Background(), []string{"Read", "Skill"})
	ctx = tools.WithAgentToolPatterns(ctx, []string{"Read", "Skill"})
	return tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: skills})
}

func TestSkillTool_TeamAgentLoadsAndListsItsGrantedTeamSkill(t *testing.T) {
	tool := teamSkillTool(t, true)
	ctx := teamSkillCtx("./style", "doc/*")
	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"./style"}`))
	if res.IsError || res.Text != "TEAM STYLE" {
		t.Fatalf("invoke ./style = %q (error %v), want the team skill's body", res.Text, res.IsError)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list"}`))
	if got := strings.Join(listNames(t, res), ","); got != "./style,doc/redactor,doc/summarizer" {
		t.Errorf("list = %q, want the team skill as ./style next to the doc/* skills", got)
	}
	if !strings.Contains(res.Text, "House style") {
		t.Errorf("list should carry the team skill's description: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","pattern":"doc/*"}`))
	if got := strings.Join(listNames(t, res), ","); got != "doc/redactor,doc/summarizer" {
		t.Errorf("list pattern=doc/* = %q, want the team skill filtered out", got)
	}
}

// Patterns govern global skills. None of them — not even allow-all — grants
// a team's own skill, and a negative entry still denies one that is named.
func TestSkillTool_PatternsNeverGrantATeamSkill(t *testing.T) {
	tool := teamSkillTool(t, true)
	for _, skills := range [][]string{nil, {"*"}, {"**"}, {"+*"}, {"./*"}, {"sdlc/*"}, {"style"}, {"./style", "-*"}} {
		ctx := teamSkillCtx(skills...)
		res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"./style"}`))
		if !res.IsError || strings.Contains(res.Text, "TEAM STYLE") {
			t.Errorf("skills %q: invoke ./style = %q, want refused", skills, res.Text)
		}
		res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list"}`))
		for _, n := range listNames(t, res) {
			if strings.HasPrefix(n, "./") {
				t.Errorf("skills %q: list offers %q", skills, n)
			}
		}
	}
}

// A global agent — even one inside the team's walk, even one whose skills name
// "./style" — is not the team's own agent and reaches no team skill.
func TestSkillTool_TeamSkillIsUnreachableToAnyOtherCaller(t *testing.T) {
	ctx := teamSkillCtx("./style")
	for name, tool := range map[string]*SkillTool{
		"not a team's own agent": teamSkillTool(t, false),
		"no team skills wired":   {Set: groupedSet(t)},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"./style"}`))
		if !res.IsError || strings.Contains(res.Text, "TEAM STYLE") {
			t.Errorf("%s: invoke ./style = %q, want refused", name, res.Text)
		}
		res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list"}`))
		if got := listNames(t, res); len(got) != 0 {
			t.Errorf("%s: list = %v, want nothing", name, got)
		}
	}
}

func TestSkillTool_TeamSkillCannotWidenTheAgentsTools(t *testing.T) {
	tool := teamSkillTool(t, true)
	ctx := tools.WithAgentTools(context.Background(), []string{"Skill"})
	ctx = tools.WithAgentToolPatterns(ctx, []string{"Skill"})
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"./style"}})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"name":"./style"}`))
	if !res.IsError || !strings.Contains(res.Text, "Read") || strings.Contains(res.Text, "TEAM STYLE") {
		t.Errorf("invoke ./style without Read = %q, want refused naming Read", res.Text)
	}
}

// A failure to read the team is not "no such skill": it is reported, on both
// ops, so the model does not conclude the agent has none.
func TestSkillTool_TeamSkillReadFailureIsReported(t *testing.T) {
	tool := &SkillTool{Set: groupedSet(t), TeamSkills: func(context.Context) (map[string]teamgraph.LocalSkill, bool, error) {
		return nil, false, errors.New("database unavailable")
	}}
	ctx := teamSkillCtx("./style")
	for _, in := range []string{`{"name":"./style"}`, `{"op":"list"}`} {
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		if !res.IsError || !strings.Contains(res.Text, "database unavailable") {
			t.Errorf("%s = %q, want the read failure reported", in, res.Text)
		}
	}
}
