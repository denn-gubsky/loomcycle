package main

import (
	"testing"
)

// RFC CW Probe 1b — observed_at parsed from the turn as a FALLBACK.
//
// ⚠️ Corrected 2026-09-08. These tests were written believing the extractor never
// emits `observed_at` — an API listing reported 0 of ~240 facts carrying it. That
// was a defect in the listing, which dropped the temporal columns in both
// backends (#1144). Direct SQL afterwards showed the extractor fills the field on
// 100% of facts, with real per-turn dates, without this parser at all.
//
// What the parser is FOR, then: a weaker or non-compliant extractor. Extractor
// tier was measured not to matter otherwise, so an operator may reasonably run a
// small local model, and one that silently omits the field would leave every fact
// undated with nothing to notice it. The tests below still describe the behaviour
// correctly — they feed an extractor that emits no temporal field, which is
// exactly the deployment this covers — only the premise about how COMMON that is
// was wrong.

// TestConsolidator_ObservedAtIsParsedFromTheTurnWithoutTheModel is the
// regression. The extractor emits NO temporal field — the non-compliant case this
// parser exists for, NOT what the measured model does — and the fact must still
// land dated.
//
// FAILS on the unfixed bundle: nothing parses the turn, so observed_at is absent.
func TestConsolidator_ObservedAtIsParsedFromTheTurnWithoutTheModel(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I moved to Berlin for a new job.\nassistant: ok"
	// No observed_at, no valid_at — what qwen3.8 actually returns.
	f.factsJSON = `[{"text":"Dave moved to Berlin for a new job.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	got := set.ObservedAt
	if got != "2023-07-07T19:56:00Z" {
		t.Errorf("observed_at = %q, want 2023-07-07T19:56:00Z parsed from the turn's own "+
			"bracketed stamp — the model emits nothing, so asking it cannot be the mechanism "+
			"(write keys: %v)", got, keysOf(set.Input))
	}
}

// TestConsolidator_AModelSuppliedTimeIsNotOverwritten. If a capable extractor DOES
// emit the field, the parser must not clobber it: the model saw the sentence and
// may have resolved a relative date ("last month") the parser cannot.
func TestConsolidator_AModelSuppliedTimeIsNotOverwritten(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I moved to Berlin.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact","observed_at":"2023-06-01T09:00:00Z"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	if got := set.ObservedAt; got != "2023-06-01T09:00:00Z" {
		t.Errorf("observed_at = %q, want the model's own value kept — the parser fills a gap, "+
			"it does not override a reading of the sentence", got)
	}
}

// TestConsolidator_AnUnparseableStampIsRefusedNotGuessed. The omit-when-unknown
// rule applies to the parser exactly as it applies to the model: a wrong date is
// worse than no date, because it hides the fact from the window it belongs in.
func TestConsolidator_AnUnparseableStampIsRefusedNotGuessed(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	// "sometime last summer" is not a timestamp. Neither is a 13 o'clock am/pm.
	f.transcript = "user: [sometime last summer] Dave: I moved to Berlin.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	if v := set.ObservedAt; v != "" {
		t.Errorf("observed_at = %v, want the field OMITTED — a guessed date files the fact "+
			"under a day it did not happen", v)
	}
	if value := set.Text; value != "Dave moved to Berlin." {
		t.Errorf("stored value = %q, want the fact kept regardless", value)
	}
}

// TestConsolidator_TwoDifferentStampsInOneBatchAreNotCollapsed. A queued batch
// spans several turns with different dates. Stamping every fact with one batch
// date would file facts under a day they did not happen, so attribution runs
// through each fact's own derived span; where that is ambiguous the field is
// omitted rather than assumed.
func TestConsolidator_TwoDifferentStampsInOneBatchAreNotCollapsed(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = nil
	f.pending = []map[string]any{{
		"id": "pend-1",
		"payload": map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "[7:56 pm on 7 July, 2023] Dave: I moved to Berlin for a new job."},
			map[string]any{"role": "user", "content": "[9:15 am on 20 August, 2023] Dave: I adopted a cat named Mochi."},
		}},
	}}
	f.factsJSON = `[{"text":"Dave adopted a cat named Mochi.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	got := set.ObservedAt
	// The cat fact belongs to the SECOND turn. The first turn's date would be wrong.
	if got != "2023-08-20T09:15:00Z" {
		t.Errorf("observed_at = %q, want the August turn's stamp (2023-08-20T09:15:00Z) — "+
			"attribution is per fact via its own span, not one date for the whole batch", got)
	}
}

// TestConsolidator_RFC3339AndISOStampsAreAccepted. LongMemEval carries
// `haystack_dates` like "2023-07-07 19:56"; the LoCoMo shape is prose. Both reach
// the turn text through the same bracket, so both must parse.
func TestConsolidator_RFC3339AndISOStampsAreAccepted(t *testing.T) {
	for _, tc := range []struct{ stamp, want string }{
		{"2023-07-07 19:56", "2023-07-07T19:56:00Z"},
		{"2023-07-07T19:56:00Z", "2023-07-07T19:56:00Z"},
		{"2023-07-07", "2023-07-07T00:00:00Z"},
		{"7 July, 2023", "2023-07-07T00:00:00Z"},
	} {
		f := newFakeToolset()
		f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
		f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
		f.transcript = "user: [" + tc.stamp + "] Dave: I moved to Berlin.\nassistant: ok"
		f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact"}]`

		runConsolidator(t, f)

		set := lastFactWrite(t, f)
		if got := set.ObservedAt; got != tc.want {
			t.Errorf("stamp %q -> observed_at %q, want %q", tc.stamp, got, tc.want)
		}
	}
}
