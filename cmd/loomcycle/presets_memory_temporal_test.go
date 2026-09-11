package main

import (
	"strings"
	"testing"
)

// The temporal regressions for RFC CW Probe 1.
//
// The bi-temporal columns (`observed_at`, `valid_at`, `invalid_at`) existed and were
// EMPTY in every stored fact — 0 of 217 across four different extractor models. The
// cause was not the model and not the schema: the per-call temporal rule that ASKS for
// those fields was gated on a conversation-level `when`, and the queued path passes "".
// So on the path that `Memory op=add` feeds — the benchmark's and the product's — the
// rule was never emitted, while the date sat in the turn text the whole time.

// TestConsolidator_QueuedBatchStillAsksForTheTime is the regression. A queued batch has
// no conversation-level date, which is exactly when the rule used to be withheld.
//
// FAILS on the unfixed bundle: the extractor prompt for the queued batch carries no
// temporal instruction at all.
func TestConsolidator_QueuedBatchStillAsksForTheTime(t *testing.T) {
	f := newFakeToolset()
	f.sessions = nil // no chats — the queued batch is the only extraction this pass
	f.pending = []map[string]any{{
		"id": "pend-1",
		"payload": map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "[7:56 pm on 7 July, 2023] Dave: I moved to Berlin last month."},
		}},
	}}
	f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact"}]`

	runConsolidator(t, f)

	spawn := lastCall(t, f, "Agent")
	prompt, _ := spawn.Input["prompt"].(string)
	if !strings.Contains(prompt, "observed_at") {
		t.Errorf("the queued-batch extraction prompt never asks for observed_at, so no fact from "+
			"`Memory op=add` can ever be dated; prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "[7:56 pm on 7 July, 2023]") && !strings.Contains(prompt, "leading timestamp") {
		t.Errorf("the prompt does not tell the extractor to read the turn's own timestamp, which is "+
			"the only date available on this path; prompt = %q", prompt)
	}
}

// TestConsolidator_ObservedAtReachesTheWrite — the field has to survive the validator
// and land on the write, or asking for it changes nothing.
//
// FAILS on the unfixed bundle: validateFacts builds its output object without
// observed_at, so the extractor's value is silently dropped before the write.
func TestConsolidator_ObservedAtReachesTheWrite(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: I moved to Berlin.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact","observed_at":"2023-07-07T19:56:00Z"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	got := set.ObservedAt
	if got != "2023-07-07T19:56:00Z" {
		t.Errorf("Memory.set observed_at = %q, want the extractor's value to reach the write "+
			"(input keys: %v)", got, keysOf(set.Input))
	}
}

// TestConsolidator_ObservedAtIsIndependentOfTheValidityInterval. A reversed interval is
// dropped while the FACT is kept — the standing rule. observed_at must not be collateral:
// "when it was said" is a different claim from "when it was true", and it is what a
// `Memory when=` window matches on, so losing it would make the fact untimed.
func TestConsolidator_ObservedAtIsIndependentOfTheValidityInterval(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: I moved to Berlin.\nassistant: ok"
	// invalid_at <= valid_at: the interval is refused, the fact survives.
	f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact",` +
		`"observed_at":"2023-07-07T19:56:00Z",` +
		`"valid_at":"2023-06-01T00:00:00Z","invalid_at":"2023-05-01T00:00:00Z"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	if got := set.ObservedAt; got != "2023-07-07T19:56:00Z" {
		t.Errorf("observed_at = %q, want it to survive a refused validity interval", got)
	}
	if set.ValidAt != "" {
		t.Errorf("valid_at was written despite being a reversed interval: %v", set.ValidAt)
	}
}

// TestConsolidator_ANonRFC3339TimeIsRefusedNotCoerced. tsOrEmpty accepts only a full
// instant, and observed_at joins that rule: half a timestamp guessed into a whole one is
// the wrong-date failure the field exists to avoid.
func TestConsolidator_ANonRFC3339TimeIsRefusedNotCoerced(t *testing.T) {
	f := newFakeToolset()
	f.bands = map[string]any{"merge_threshold": 0.90, "related_threshold": 0.50}
	f.sessions = []map[string]any{scanRow("sess-a", "2026-07-01T10:00:00Z")}
	f.transcript = "user: I moved to Berlin.\nassistant: ok"
	f.factsJSON = `[{"text":"Dave moved to Berlin.","class":"fact","observed_at":"July 2023"}]`

	runConsolidator(t, f)

	set := lastFactWrite(t, f)
	if set.ObservedAt != "" {
		t.Errorf("observed_at = %v, want the prose date REFUSED and the field omitted entirely", set.ObservedAt)
	}
	if value := set.Text; value != "Dave moved to Berlin." {
		t.Errorf("stored value = %q, want the fact kept — a good fact with a bad date is still a good fact", value)
	}
}

// keysOf is a failure-message helper: naming the keys that ARE present turns "want
// observed_at" into a diagnosis of what the write actually carried.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
