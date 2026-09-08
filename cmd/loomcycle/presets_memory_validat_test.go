package main

import (
	"strings"
	"testing"
)

// RFC CW Probe 1c — valid_at from a relative phrase, as a FALLBACK.
//
// Measured before building this, on 86 facts: the extractor fills valid_at on
// ~19% and its resolutions are GOOD when it makes them. The other 81% have the
// time DELETED rather than resolved — "Melanie bought figurines." was stamped one
// day before it was said, so the model worked out "yesterday" then dropped the
// word. So the model keeps the resolver's job and this catches what it drops.
//
// The prompt now asks it to KEEP the wording when it cannot resolve the date,
// because before that change only 2 of 86 fact texts still carried a relative
// phrase and ZERO carried one while lacking valid_at — a resolver alone would
// have had no input at all.

// TestConsolidator_RelativePhraseResolvesAgainstObservedAt is the regression.
func TestConsolidator_RelativePhraseResolvesAgainstObservedAt(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I moved to Berlin.\nassistant: ok"
	// The model kept the wording and did NOT resolve it — the case this covers.
	f.factsJSON = `[{"text":"Dave moved to Berlin last week.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastCall(t, f, "Memory.set")
	if got, _ := set.Input["observed_at"].(string); got != "2023-07-07T19:56:00Z" {
		t.Fatalf("observed_at = %q, want the turn's stamp as the anchor", got)
	}
	if got, _ := set.Input["valid_at"].(string); got != "2023-06-30T19:56:00Z" {
		t.Errorf("valid_at = %q, want 2023-06-30T19:56:00Z (one week before it was said) — "+
			"the anchor and the phrase are both present, so the fact is datable", got)
	}
}

// TestConsolidator_ModelValidAtWinsOverTheFallback. The model read the sentence and
// may have resolved something the parser cannot; it is the better resolver.
func TestConsolidator_ModelValidAtWinsOverTheFallback(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I moved.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved to Berlin last week.","class":"fact","valid_at":"2023-07-01T00:00:00Z"}]`

	runConsolidator(t, f)

	set := lastCall(t, f, "Memory.set")
	if got, _ := set.Input["valid_at"].(string); got != "2023-07-01T00:00:00Z" {
		t.Errorf("valid_at = %q, want the model's own value kept", got)
	}
}

// TestConsolidator_AnAbsoluteDateInTheSentenceIsNotComputedOver. If the sentence
// already names a date, the model either resolved it or the date IS the claim.
// Computing over the top would replace a stated fact with an inference.
func TestConsolidator_AnAbsoluteDateInTheSentenceIsNotComputedOver(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I moved.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved to Berlin in June 2023, a week before he said so.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastCall(t, f, "Memory.set")
	if v, ok := set.Input["valid_at"]; ok {
		t.Errorf("valid_at = %v, want NO computed value: the sentence names June 2023, so the "+
			"date is stated rather than inferable", v)
	}
}

// TestConsolidator_TwoDifferentRelativePhrasesRefuse — ambiguity omits.
func TestConsolidator_TwoDifferentRelativePhrasesRefuse(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I moved.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved last week and started the job two months ago.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastCall(t, f, "Memory.set")
	if v, ok := set.Input["valid_at"]; ok {
		t.Errorf("valid_at = %v, want the field OMITTED — two different references in one "+
			"sentence cannot both be the world-time", v)
	}
}

// TestConsolidator_MonthArithmeticCrossesTheBoundaryCorrectly. "last month" from
// 31 March is 28 February; 30 days back is 1 March — a whole month wrong at
// exactly the boundary a temporal question is most likely to probe.
func TestConsolidator_MonthArithmeticCrossesTheBoundaryCorrectly(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [2023-03-31T12:00:00Z] Dave: I moved.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave changed jobs last month.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastCall(t, f, "Memory.set")
	got, _ := set.Input["valid_at"].(string)
	if got != "2023-02-28T12:00:00Z" && got != "2023-03-03T12:00:00Z" {
		t.Errorf("valid_at = %q — want a real month subtraction from 31 March", got)
	}
	if got == "2023-03-01T12:00:00Z" {
		t.Errorf("valid_at = %q looks like 30-day arithmetic, which is a month wrong here", got)
	}
}

// TestConsolidator_ForwardReferenceIsNotDated. A plan's world-time is when it will
// happen, which a transcript rarely pins down; guessing dates a fact to an event
// that may never occur.
func TestConsolidator_ForwardReferenceIsNotDated(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: [7:56 pm on 7 July, 2023] Dave: I'm moving.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave is moving to Berlin next week.","class":"fact"}]`

	runConsolidator(t, f)

	set := lastCall(t, f, "Memory.set")
	if v, ok := set.Input["valid_at"]; ok {
		t.Errorf("valid_at = %v, want NO value for a forward reference", v)
	}
}

// TestConsolidator_ThePromptAsksToKeepUnresolvedWording — the resolver's INPUT.
// Without this instruction only 2 of 86 fact texts kept a relative phrase, so the
// resolver would have had nothing to act on.
func TestConsolidator_ThePromptAsksToKeepUnresolvedWording(t *testing.T) {
	f := newFakeToolset()
	f.sessions = nil
	f.pending = []map[string]any{{
		"id": "pend-1",
		"payload": map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "[7:56 pm on 7 July, 2023] Dave: I moved last week."},
		}},
	}}
	f.factsJSON = `[{"text":"Dave moved last week.","class":"fact"}]`

	runConsolidator(t, f)

	spawn := lastCall(t, f, "Agent")
	prompt, _ := spawn.Input["prompt"].(string)
	if !strings.Contains(prompt, "KEEP THE WORDING") {
		t.Errorf("the prompt does not ask the extractor to keep an unresolved time reference, "+
			"so the fallback has no input; prompt = %q", prompt)
	}
}
