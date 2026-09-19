package builtin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestRecallTraces_IsGrantOnlyWithNoToolParameter.
//
// ⚠️ THE ABSENCE IS THE FEATURE. Its sibling shipped with a tool parameter as well as
// a grant, and the measurement was that the parameter does not work: told to pass
// `include_turns` on every call, qwen3.6 passed it on 51 of 128. A parameter is a
// decision the model makes, so this lever deliberately has none — if one is ever
// added, this test should fail and the person adding it should have to say why.
func TestRecallTraces_IsGrantOnlyWithNoToolParameter(t *testing.T) {
	if strings.Contains(memoryInputSchema, `"attach_traces"`) {
		t.Error("attach_traces is advertised as a tool parameter — this lever is " +
			"operator-granted precisely because a model does not reliably pass one")
	}
	var in memoryInput
	if err := json.Unmarshal([]byte(`{"op":"recall","scope":"user","query":"x","attach_traces":true}`), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Unknown fields are ignored by the decoder; the assertion is that nothing in the
	// input struct picked it up and quietly turned the grant on.
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "attach_traces") {
		t.Error("attach_traces reached the input struct")
	}
}

// TestRecallTraces_TurnTextIsCapped. A raw turn can be arbitrarily long and this
// block is not the one the reader asked for, so its cost has to be bounded.
func TestRecallTraces_TurnTextIsCapped(t *testing.T) {
	long := strings.Repeat("x", recallTraceMaxChars*3)
	if got := trimTraceText(long); len(got) > recallTraceMaxChars {
		t.Errorf("trace text = %d chars, want <= %d", len(got), recallTraceMaxChars)
	}
	short := "a short turn"
	if got := trimTraceText(short); got != short {
		t.Errorf("a turn under the cap was altered: %q", got)
	}
}

// TestRecallTraces_BudgetIsSEPARATEFromTheFactAnchoredOne.
//
// Sharing one budget would let whichever retrieval ran first starve the other, and
// which one that is would be an accident of call ordering rather than a decision.
func TestRecallTraces_BudgetIsSeparateFromTheFactAnchoredOne(t *testing.T) {
	// Two named constants, not one shared — the assertion is structural.
	if recallTracesBudget == 0 {
		t.Fatal("the question-anchored block has no budget of its own")
	}
	src, err := os.ReadFile("memory.go")
	if err != nil {
		t.Fatal(err)
	}
	// The two attach calls must not be summing into one running total.
	if strings.Contains(string(src), "recallTurnsBudget+recallTracesBudget") {
		t.Error("the two blocks share one budget; the later retrieval can be starved " +
			"by the earlier one for no reason a caller could predict")
	}
}

// TestRecallTraces_ParsesTheStoredRowRatherThanHandingOverJSON.
//
// A trace row is the index's own JSON object. Handed to a model raw, a small model
// answers with a serialised object — the same failure signature the coerced-prompt
// arm produced, where tool-call JSON was emitted into the answer field.
func TestRecallTraces_ParsesTheStoredRowRatherThanHandingOverJSON(t *testing.T) {
	row := json.RawMessage(`{"at":"2026-09-18T16:57:08Z","text":"[9:55 am on 22 October, 2023] Caroline: I passed the interviews.","run_id":"","speaker":"user","session_id":"s_abc"}`)
	got := TraceTurnText(row)
	if got != "[9:55 am on 22 October, 2023] Caroline: I passed the interviews." {
		t.Errorf("traceTurnText = %q", got)
	}
	// The DATE has to survive: a distilled fact is tenseless and the turn's stamp is
	// the half that answers "when".
	if !strings.Contains(got, "22 October, 2023") {
		t.Error("the turn's own timestamp was dropped — temporal questions become " +
			"unanswerable for a reason that is not the reader")
	}
	for _, bad := range []string{`"speaker"`, `"session_id"`, `"run_id"`} {
		if strings.Contains(got, bad) {
			t.Errorf("raw JSON field %s reached the rendered turn", bad)
		}
	}
	if TraceTurnText(json.RawMessage(`not json`)) != "" {
		t.Error("an unparseable row must yield nothing rather than raw bytes")
	}
	if TraceTurnText(nil) != "" {
		t.Error("an empty row must yield nothing")
	}
}

// TestRecallTraces_GrantIsCarriedByEveryPolicyConstructionSite.
//
// ⚠️ A STRUCT-LITERAL SWEEP, not a name search — the same guard its sibling carries,
// for the same reason. MemoryPolicyValue is assembled at several call sites and a
// field missed at one is silently zero there, which for a grant means DENIED on
// exactly the path nobody tested.
//
// Driven off the sibling's own count rather than a hand-written list of sites,
// because a hand-written list is the second copy that drifts.
func TestRecallTraces_GrantIsCarriedByEveryPolicyConstructionSite(t *testing.T) {
	for _, f := range []string{
		"../../api/http/server.go",
		"../../api/http/resume.go",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		sibling := strings.Count(src, "RecallIncludeTurns: ")
		mine := strings.Count(src, "RecallAttachTraces: ")
		if sibling == 0 {
			t.Fatalf("%s carries no RecallIncludeTurns at all — this guard derives its "+
				"expectation from that count and would assert nothing", f)
		}
		if sibling != mine {
			t.Errorf("%s builds MemoryPolicyValue %d times carrying RecallIncludeTurns but "+
				"only %d carrying RecallAttachTraces — a missed site resolves to false, "+
				"which for a grant means DENIED on that path alone", f, sibling, mine)
		}
	}
}

// TestRecallTraces_GrantRoundTripsTheAgentDefinition. The grant is useless if it does
// not survive the def it is declared on: an agent authored with it and reloaded must
// still hold it. This is the closure the *_def_scopes overlay bug broke once.
func TestRecallTraces_GrantRoundTripsTheAgentDefinition(t *testing.T) {
	for _, f := range []string{
		"../../agents/loader.go",
		"../../agents/sign.go",
		"../../lookup/agent.go",
		"agentdef.go",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		sib := strings.Count(src, "RecallIncludeTurns")
		mine := strings.Count(src, "RecallAttachTraces")
		if sib == 0 {
			t.Fatalf("%s mentions RecallIncludeTurns nowhere — the guard is vacuous", f)
		}
		if mine < sib {
			t.Errorf("%s carries RecallIncludeTurns %d times but RecallAttachTraces only "+
				"%d — the grant does not round-trip the definition everywhere its "+
				"sibling does", f, sib, mine)
		}
	}
}
