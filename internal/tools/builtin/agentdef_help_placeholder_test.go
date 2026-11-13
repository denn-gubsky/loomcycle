package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A runtime agent's prompt is held to the closed set of {{help:<topic>}}
// placeholders static agents are (RFC GL P4): an unknown topic, a topic with
// no call examples and a malformed placeholder are refused at create AND at
// fork, naming the placeholder, rather than stored to render nothing.
func TestAgentDefTool_RefusesAnUnusableHelpPlaceholder(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithCallerTier(tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"*"} }), tools.TierOperatorPlane)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"carder","promote":true,"overlay":{
		"system_prompt":"Search like this:\n{{help:Document/search}}","tools":["Document"]}}`))
	if res.IsError {
		t.Fatalf("create with a known topic: %s", res.Text)
	}

	for _, tc := range []struct{ prompt, want string }{
		{`{{help:Document/no_such_op}}`, "Document/no_such_op"},
		{`{{help:loomcycle}}`, "no call examples"},
		{`{{help:Document/search/extra}}`, "Document/search/extra"},
	} {
		overlay, _ := json.Marshal(map[string]any{"system_prompt": tc.prompt})
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"carder2","overlay":`+string(overlay)+`}`))
		wantRefused(t, res, "create "+tc.prompt, tc.want)
		res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"carder","overlay":`+string(overlay)+`}`))
		wantRefused(t, res, "fork "+tc.prompt, tc.want)
	}
}

// A STORED body is read, not authored: a team-local agent whose prompt names a
// help topic this binary no longer has, or whose examples outgrew the budget,
// must still resolve and restore. The placeholder then renders nothing (or is
// trimmed) at run time; refusing here would make the stored team unrunnable
// and unrestorable after an upgrade.
func TestStoredLocalAgentBody_StaleHelpPlaceholderStillResolves(t *testing.T) {
	for what, prompt := range map[string]string{
		"an unknown topic":    `{{help:Document/no_such_op}}`,
		"an over-budget body": strings.Repeat(`{{help:knowledge-base}} `, 4),
	} {
		body, _ := json.Marshal(map[string]any{"tier": "middle", "tools": []string{"Read"}, "system_prompt": prompt})
		if _, err := LocalAgentDefinition(body); err != nil {
			t.Errorf("%s: LocalAgentDefinition refused a stored body: %v", what, err)
		}
		if err := ValidateTeamDefBody(json.RawMessage(localTeam(string(body)))); err != nil {
			t.Errorf("%s: ValidateTeamDefBody refused a stored team: %v", what, err)
		}
	}
}

// Authoring a team is still an authoring gate: a team-local agent whose prompt
// names an unknown topic is refused at TeamDef create.
func TestTeamDefCreate_RefusesLocalAgentWithUnknownHelpTopic(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	body, _ := json.Marshal(map[string]any{"tier": "middle", "tools": []string{"Read"}, "system_prompt": `{{help:Document/no_such_op}}`})
	res := teamOp(t, tool, ctx, "create", "sdlc", localTeam(string(body)))
	wantRefused(t, res, "team create", "Document/no_such_op")
}
