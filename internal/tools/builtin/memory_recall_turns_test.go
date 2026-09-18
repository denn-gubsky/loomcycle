package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRecallTurns_SchemaAdvertisesTheParameter. The parser census that caught the
// `sources`/`traces` drift applies here too: a parameter in the struct but absent
// from the schema is one a caller has no way to learn about, and a parameter in the
// schema but absent from the struct decodes to nothing and is silently ignored —
// which is the exact shape of the last three bugs in this area.
func TestRecallTurns_SchemaAdvertisesTheParameter(t *testing.T) {
	if !strings.Contains(memoryInputSchema, `"include_turns"`) {
		t.Error("include_turns is not in the input schema — a caller cannot discover it")
	}
	var in memoryInput
	if err := json.Unmarshal([]byte(`{"op":"recall","scope":"user","query":"x","include_turns":true}`), &in); err != nil {
		t.Fatalf("include_turns did not decode: %v", err)
	}
	if !in.IncludeTurns {
		t.Error("include_turns decoded to false — accepted and dropped is the failure " +
			"mode this asserts against")
	}
}

// TestRecallTurns_DefaultsOff. Turns are large; every existing caller would pay for
// them silently if this defaulted on, and RFC DF's first no-regression rule is that
// an unset parameter leaves the response byte-shaped as it is today.
func TestRecallTurns_DefaultsOff(t *testing.T) {
	var in memoryInput
	if err := json.Unmarshal([]byte(`{"op":"recall","scope":"user","query":"x"}`), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if in.IncludeTurns {
		t.Error("include_turns defaulted to TRUE — every existing caller would silently " +
			"start paying for transcript it never asked for")
	}
}

// TestRecallTurns_TurnTextIsCapped. A turn is raw conversation and can be arbitrarily
// long; the fact it supports is one sentence.
func TestRecallTurns_TurnTextIsCapped(t *testing.T) {
	long := strings.Repeat("x", recallTurnMaxChars*3)
	if got := trimTurnText(long); len(got) > recallTurnMaxChars {
		t.Errorf("turn text = %d chars, want <= %d", len(got), recallTurnMaxChars)
	}
	short := "a short turn"
	if got := trimTurnText(short); got != short {
		t.Errorf("a turn under the cap was altered: %q", got)
	}
}
