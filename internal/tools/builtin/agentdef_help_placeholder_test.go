package builtin

import (
	"encoding/json"
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
		{`{{help:Document search}}`, "Document search"},
	} {
		overlay, _ := json.Marshal(map[string]any{"system_prompt": tc.prompt})
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"carder2","overlay":`+string(overlay)+`}`))
		wantRefused(t, res, "create "+tc.prompt, tc.want)
		res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"carder","overlay":`+string(overlay)+`}`))
		wantRefused(t, res, "fork "+tc.prompt, tc.want)
	}
}
