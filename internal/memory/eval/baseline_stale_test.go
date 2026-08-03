package eval

import (
	"path/filepath"
	"testing"
)

// TestBaseline_StaleMatchNamesAnExpiredGate covers the difference between two
// states that EntryFor collapses into one silent "no baseline".
//
// EntryFor keys on provider+model+effort+prompt_sha+corpus_sha, and a run with no
// exact match is deliberately NOT a regression — blocking an unmeasured model would
// block every new candidate's first run. But that policy also hides the case where
// a configuration WAS measured and its gate expired because the extractor prompt
// changed: editing that prompt un-gates every model/effort at once, invisibly, and
// the next run reports a clean pass with nothing behind it.
//
// This is not hypothetical. It is the state of the recorded baseline: effort=medium
// was measured under one prompt, the prompt changed, and medium runs have been
// ungated since — reported as neither a regression nor a gap.
func TestBaseline_StaleMatchNamesAnExpiredGate(t *testing.T) {
	b := Baseline{
		RecallTolerance: 0.15,
		Entries: []BaselineEntry{{
			Provider: "ollama-local", Model: "m", Effort: "medium",
			SystemPromptSHA256: "OLDPROMPT", CorpusSHA256: "CORPUS",
			MeasuredAt: "2026-07-29T00:00:00Z",
			Recall:     map[string]float64{"extraction": 1},
			Violations: map[string]int{"extraction": 0},
		}},
	}
	// Same configuration, NEW prompt: no exact match, so nothing is compared.
	cur := ExtractionReport{
		Provider: "ollama-local", Model: "m", Effort: "medium",
		SystemPromptSHA256: "NEWPROMPT", CorpusSHA256: "CORPUS",
		Abilities: []AbilityScore{{Ability: AbilityExtraction, Recall: 0.1, Violations: 9}},
	}
	if regs := b.Regressions(cur); len(regs) != 0 {
		t.Fatalf("precondition: expected the gate to be silent, got %v", regs)
	}
	stale, had := b.StaleMatch(cur)
	if !had {
		t.Fatal("a configuration measured under a DIFFERENT prompt was not reported as " +
			"stale, so an expired gate is indistinguishable from a never-measured one")
	}
	if stale.SystemPromptSHA256 != "OLDPROMPT" {
		t.Errorf("stale entry = %q, want the previously-measured prompt", stale.SystemPromptSHA256)
	}

	// An EXACT match must not be called stale — that would cry wolf on every run.
	exact := cur
	exact.SystemPromptSHA256 = "OLDPROMPT"
	if _, had := b.StaleMatch(exact); had {
		t.Error("an exactly-matching configuration was reported stale")
	}
	// And a genuinely new model is not stale either: it was never measured, which
	// is the case the no-block policy is FOR.
	fresh := cur
	fresh.Model = "never-seen"
	if _, had := b.StaleMatch(fresh); had {
		t.Error("an unmeasured model was reported as having an expired gate")
	}
}

// TestBaseline_RecordedFileHasAnUngatedEffort documents the live state of the
// checked-in baseline, so the gap is visible rather than folklore. It is
// intentionally not a failure: re-recording is an operator decision that needs a
// model run, not something a unit test can do.
func TestBaseline_RecordedFileHasAnUngatedEffort(t *testing.T) {
	b, err := LoadBaseline(filepath.Join("extraction-baseline.json"))
	if err != nil {
		t.Skipf("baseline not readable: %v", err)
	}
	if len(b.Entries) == 0 {
		t.Skip("no baseline entries recorded")
	}
	newest := b.Entries[0]
	for _, e := range b.Entries {
		if e.MeasuredAt > newest.MeasuredAt {
			newest = e
		}
	}
	gated, measured := map[string]bool{}, map[string]bool{}
	for _, e := range b.Entries {
		measured[e.Effort] = true
		if e.SystemPromptSHA256 == newest.SystemPromptSHA256 {
			gated[e.Effort] = true
		}
	}
	for eff := range measured {
		if !gated[eff] {
			t.Logf("effort=%q has a baseline but NOT under the newest prompt (%s…) — "+
				"runs at that effort are currently ungated; re-record to restore it",
				eff, shortSHAForTest(newest.SystemPromptSHA256))
		}
	}
	t.Logf("efforts measured=%v gated-under-newest-prompt=%v", effortKeys(measured), effortKeys(gated))
}

func shortSHAForTest(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// effortKeys is local to this file; consolidation_test.go already has a keysOf.
func effortKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
