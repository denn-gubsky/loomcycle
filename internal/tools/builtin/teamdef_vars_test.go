package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// varsTeam declares two variables. Its entry agent reads them, a vars state
// overwrites one, and a second agent reads them again.
const varsTeam = `{
  "entry":"first",
  "vars":{"tone":"formal","lang":"en"},
  "states":[
    {"state":"first","handler":{"kind":"agent","agent":"writer","input_template":"${var.tone}/${var.lang}"}},
    {"state":"stamp","handler":{"kind":"vars","set":{"tone":"set-by-state"}}},
    {"state":"second","handler":{"kind":"agent","agent":"writer","input_template":"${var.tone}/${var.lang}"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"first","to":"stamp","on":"success"},
    {"from":"stamp","to":"second","on":"success"},
    {"from":"second","to":"done","on":"success"}
  ]}`

func definitionVars(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	def, ok := row["definition"].(map[string]any)
	if !ok {
		t.Fatalf("definition = %T, want an object", row["definition"])
	}
	vars, _ := def["vars"].(map[string]any)
	return vars
}

func getTeam(t *testing.T, tool *TeamDef, ctx context.Context, defID string) map[string]any {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get %s: %s", defID, res.Text)
	}
	return decodeResult(t, res.Text)
}

func forkTeam(t *testing.T, tool *TeamDef, ctx context.Context, name, overlay string) map[string]any {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","overlay":`+overlay+`}`))
	if res.IsError {
		t.Fatalf("fork %q: %s", name, res.Text)
	}
	return decodeResult(t, res.Text)
}

func TestTeamDefTool_VarsRoundTripThroughCreateGetForkGet(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	declared := map[string]any{"tone": "formal", "lang": "en"}

	created := createTeam(t, tool, ctx, "vars-rt", varsTeam)
	if got := definitionVars(t, getTeam(t, tool, ctx, created["def_id"].(string))); !reflect.DeepEqual(got, declared) {
		t.Fatalf("after create, get returns vars %v, want %v", got, declared)
	}

	// A fork that does not mention vars keeps the parent's.
	kept := forkTeam(t, tool, ctx, "vars-rt", `{"max_iterations":4}`)
	if got := definitionVars(t, getTeam(t, tool, ctx, kept["def_id"].(string))); !reflect.DeepEqual(got, declared) {
		t.Errorf("a fork that does not send vars returns %v, want the parent's %v", got, declared)
	}

	// A fork that sends vars replaces the list whole — `lang` is gone, not merged.
	replaced := forkTeam(t, tool, ctx, "vars-rt", `{"vars":{"tone":"casual"}}`)
	if got := definitionVars(t, getTeam(t, tool, ctx, replaced["def_id"].(string))); !reflect.DeepEqual(got, map[string]any{"tone": "casual"}) {
		t.Errorf("a fork that sends vars returns %v, want exactly {tone: casual}", got)
	}
	if replaced["content_sha256"] == created["content_sha256"] {
		t.Error("a fork that changed a default kept the parent's content hash")
	}

	// {} declares none, and is the definition a team without the block has.
	cleared := forkTeam(t, tool, ctx, "vars-rt", `{"vars":{}}`)
	if got := definitionVars(t, getTeam(t, tool, ctx, cleared["def_id"].(string))); len(got) != 0 {
		t.Errorf("a fork that sends vars:{} returns %v, want none", got)
	}
}

func TestTeamDefTool_CreateRefusesABadVarsBlockAndPersistsNothing(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	bad := strings.Replace(varsTeam, `"tone":"formal"`, `"tone":"{{document:/secret}}"`, 1)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"vars-bad","overlay":`+bad+`}`))
	if !res.IsError || !strings.Contains(res.Text, `vars "tone"`) {
		t.Fatalf("create = %q (isErr=%v), want a refusal naming vars \"tone\"", res.Text, res.IsError)
	}
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"vars-bad"}`)); !res.IsError {
		if n := len(decodeResult(t, res.Text)["versions"].([]any)); n != 0 {
			t.Errorf("the refused create persisted %d version(s)", n)
		}
	}
}
