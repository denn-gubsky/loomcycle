package builtin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
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

// TestRecallTraces_SuppressedWhenTheCallerAlreadyAskedForTraces.
//
// A trace-only search returns the turns as its ENTRIES. Appending them again under
// `source_turns` hands the model the same rows twice in two shapes — the duplication
// the dedup path exists to prevent. ErrTracesNotCombinable already refuses mixing
// traces with other sources, so "asked for traces" is exactly a traces-only query.
func TestRecallTraces_SuppressedWhenTheCallerAlreadyAskedForTraces(t *testing.T) {
	if !sourcesIncludeTraces([]memrank.Source{memrank.SourceTraces}) {
		t.Error("a traces-only selector was not recognised — the block would be appended " +
			"on top of a result that already IS the turns")
	}
	for _, sel := range [][]memrank.Source{
		nil,
		{memrank.SourceFacts},
		{memrank.SourceFacts, memrank.SourceNotes},
		{memrank.SourceDocuments},
	} {
		if sourcesIncludeTraces(sel) {
			t.Errorf("selector %v was treated as a trace search — the grant would be "+
				"suppressed on exactly the calls it exists to serve", sel)
		}
	}
}

// TestRecallTraces_GrantCoversSearchNotJustRecall.
//
// ⚠️ THE REGRESSION THIS FILE'S SIBLING DID NOT HAVE. The grant first shipped on
// `recall` alone, and the OP IS A DECISION THE MODEL MAKES: on one LoCoMo conversation
// the reader chose `recall` 136 of 150 times and the grant fired on 91% of calls; on
// another it chose `search` 49 of 85 and fired on 42%. Of 81 questions there, the 36
// that used `recall` moved +27.8pp and the 45 that used `search` moved +0.0pp — the
// lever was worth the same, only coverage differed.
//
// Driven off the source text rather than a live store, because the point is
// structural: both dispatch paths must consult the grant.
func TestRecallTraces_GrantCoversSearchNotJustRecall(t *testing.T) {
	b, err := os.ReadFile("memory.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "RecallAttachTraces"); n < 2 {
		t.Errorf("the grant is consulted %d time(s) in memory.go — it must gate BOTH the "+
			"recall path and the search path, or the op the model happens to pick decides "+
			"whether an operator's grant applies", n)
	}
	if n := strings.Count(src, "attachQuestionTurns"); n < 2 {
		t.Errorf("attachQuestionTurns is called %d time(s) — expected both recall and search", n)
	}
	if !strings.Contains(src, "sourcesIncludeTraces") {
		t.Error("the search path does not suppress the block on a traces-only search, so a " +
			"trace search would return the same rows twice")
	}
}

// TestRecallTraces_SkipsTurnsFromTheCallersOwnRun.
//
// The live indexer files a user turn as it arrives, so by the time the attached
// search runs, the question being asked IS in the index — and it matches itself
// better than anything else, taking the top slot and handing the reader its own
// question back as evidence. On a per-instance benchmark where each run asks one
// question, that is a guaranteed wasted slot in every block.
//
// Fails OPEN: a row with no run_id is KEPT, because dropping real evidence is worse
// than keeping one echo.
func TestRecallTraces_SkipsTurnsFromTheCallersOwnRun(t *testing.T) {
	own := `{"text":"my own question","run_id":"r_self"}`
	other := `{"text":"something said earlier","run_id":"r_earlier"}`
	legacy := `{"text":"a row from before the field existed"}`
	if got := traceTurnRunID(json.RawMessage(own)); got != "r_self" {
		t.Errorf("traceTurnRunID = %q, want r_self", got)
	}
	if got := traceTurnRunID(json.RawMessage(other)); got != "r_earlier" {
		t.Errorf("traceTurnRunID = %q, want r_earlier", got)
	}
	if got := traceTurnRunID(json.RawMessage(legacy)); got != "" {
		t.Errorf("a row without run_id must yield \"\" so it is KEPT, got %q", got)
	}
	if got := traceTurnRunID(json.RawMessage(`not json`)); got != "" {
		t.Errorf("an unparseable row must yield \"\" (fail open), got %q", got)
	}
	src, err := os.ReadFile("memory_recall_traces.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "ownRun != \"\" && traceTurnRunID(") {
		t.Error("the skip is not guarded on a non-empty own-run id — with no run identity " +
			"every legacy row would match \"\" and the whole block would be dropped")
	}
}
