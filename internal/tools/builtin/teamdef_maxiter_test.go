package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// A fork can put a capped team back on the default cap. 0 is "use the
// default", so a merge that applied the field only when non-zero kept the
// parent's cap whether the editor sent 0, null, or dropped the key — and said
// nothing. Presence decides: sent (0 or null) clears, absent keeps.
func TestTeamDefFork_MaxIterationsClearedWhenSentAndKeptWhenAbsent(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()

	capped := strings.Replace(validTeamGraph, `"entry"`, `"max_iterations": 12, "entry"`, 1)
	createTeam(t, tool, ctx, "capped", capped)

	forkMax := func(overlay string) float64 {
		t.Helper()
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"capped","overlay":`+overlay+`}`))
		if res.IsError {
			t.Fatalf("fork %s: %s", overlay, res.Text)
		}
		def, _ := decodeResult(t, res.Text)["definition"].(map[string]any)
		got, _ := def["max_iterations"].(float64) // absent (omitempty) reads as 0
		return got
	}

	if got := forkMax(`{"colors":{"states":{"review":"#eeeeee"}}}`); got != 12 {
		t.Errorf("a fork that does not send max_iterations must keep the parent's 12; got %v", got)
	}
	if got := forkMax(`{"max_iterations":0}`); got != 0 {
		t.Errorf("a fork that sends max_iterations: 0 must clear the cap; got %v", got)
	}
	// Each fork forks the ACTIVE version (forks default to promote:false), so
	// every case here starts from the parent's 12.
	if got := forkMax(`{"max_iterations":7}`); got != 7 {
		t.Errorf("a fork that sends max_iterations: 7 must set it; got %v", got)
	}
	if got := forkMax(`{"max_iterations":null}`); got != 0 {
		t.Errorf("a fork that sends max_iterations: null must clear the cap; got %v", got)
	}
}
