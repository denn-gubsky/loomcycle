package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// inputPublishGraph is an input state that publishes the walk's input to
// pcparts-in, then ends.
const inputPublishGraph = `{"entry":"form","states":[` +
	`{"state":"form","handler":{"kind":"input","publish":{"channel":"pcparts-in"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"form","to":"done","on":"success"}]`

// TestTeamDefPreflight_RefusesAnInputPublishWithNoGrant: an input state's
// publish goes through the team's ACL like a channel state's, so a definition
// that does not grant it could never run and is refused at create.
func TestTeamDefPreflight_RefusesAnInputPublishWithNoGrant(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	ctx := authoringCtx([]string{"pcparts-in", "other"}, nil)

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"intake","overlay":`+inputPublishGraph+`,"channels":{"publish":["other"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, "does not grant publish") {
		t.Fatalf("an input publish with no grant was accepted: IsError=%v %s", res.IsError, res.Text)
	}
	for _, want := range []string{`state "form"`, `channel "pcparts-in"`, "publish"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("refusal is missing %q:\n%s", want, res.Text)
		}
	}
}

func TestTeamDefPreflight_AcceptsAnInputPublishWithTheGrant(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = declared("pcparts-in")
	ctx := authoringCtx([]string{"pcparts-in"}, nil)

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"intake","overlay":`+inputPublishGraph+`,"channels":{"publish":["pcparts-in"]}}}`))
	if res.IsError {
		t.Fatalf("a granted input publish was refused: %s", res.Text)
	}
}
