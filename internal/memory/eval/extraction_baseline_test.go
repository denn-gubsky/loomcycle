package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reportFor(promptSHA string, recall map[Ability]float64, viol map[Ability]int) ExtractionReport {
	r := ExtractionReport{
		Provider: "ollama-local", Model: "qwen3.6:30b", Effort: "medium",
		SystemPromptSHA256: promptSHA,
	}
	for _, a := range AllAbilities() {
		s := AbilityScore{Ability: a, Cases: 1, Recall: -1}
		if v, ok := recall[a]; ok {
			s.Recall = v
		}
		s.Violations = viol[a]
		r.Abilities = append(r.Abilities, s)
		r.TotalViolations += s.Violations
	}
	return r
}

func TestLoadBaseline_MissingFileIsNotAnError(t *testing.T) {
	b, err := LoadBaseline(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing baseline must not be an error — the first run against a new model has nothing to compare to: %v", err)
	}
	if b.RecallTolerance != DefaultRecallTolerance {
		t.Errorf("tolerance should default, got %v", b.RecallTolerance)
	}
	if len(b.Entries) != 0 {
		t.Errorf("expected no entries, got %d", len(b.Entries))
	}
}

func TestBaseline_NoMatchingEntryIsNotARegression(t *testing.T) {
	b := Baseline{RecallTolerance: DefaultRecallTolerance}
	rep := reportFor("sha-a", map[Ability]float64{AbilityProperty: 0.0}, nil)
	if got := b.Regressions(rep); len(got) != 0 {
		t.Fatalf("an unmeasured model is not a regression, got %v", got)
	}
}

// TestBaseline_ViolationRiseIsAlwaysARegression: tolerance exists for recall
// jitter, and there is no jitter that turns "stored no secrets" into "stored one".
func TestBaseline_ViolationRiseIsAlwaysARegression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	clean := reportFor("sha-a", map[Ability]float64{AbilityUpdate: 1.0}, nil)
	if err := SaveBaselineEntry(path, clean); err != nil {
		t.Fatalf("save: %v", err)
	}
	b, _ := LoadBaseline(path)

	leaked := reportFor("sha-a", map[Ability]float64{AbilityUpdate: 1.0}, map[Ability]int{AbilityAbstention: 1})
	got := b.Regressions(leaked)
	if len(got) == 0 {
		t.Fatal("a new violation must be reported as a regression")
	}
	if !strings.Contains(got[0], "abstention") {
		t.Errorf("the regression should name the ability: %v", got)
	}
}

func TestBaseline_RecallJitterToleratedButALostCaseIsNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	good := reportFor("sha-a", map[Ability]float64{AbilityProperty: 1.0, AbilityUpdate: 1.0}, nil)
	if err := SaveBaselineEntry(path, good); err != nil {
		t.Fatalf("save: %v", err)
	}
	b, _ := LoadBaseline(path)

	// Within tolerance — must NOT fire, or the gate fails on model noise and
	// everyone learns to pass --no-gate.
	jitter := reportFor("sha-a", map[Ability]float64{AbilityProperty: 0.9, AbilityUpdate: 1.0}, nil)
	if got := b.Regressions(jitter); len(got) != 0 {
		t.Errorf("a 0.10 drop is inside the %.2f tolerance and must not fire: %v", b.RecallTolerance, got)
	}

	// A lost case out of two is 0.5 — well beyond tolerance.
	lost := reportFor("sha-a", map[Ability]float64{AbilityProperty: 0.5, AbilityUpdate: 1.0}, nil)
	got := b.Regressions(lost)
	if len(got) == 0 {
		t.Fatal("a lost case must be reported")
	}
	if !strings.Contains(got[0], "property") {
		t.Errorf("the regression should name the ability: %v", got)
	}
}

// TestBaseline_PromptChangeIsNotARegression is the subtle one, and the reason the
// prompt digest is part of the key.
//
// Editing the extractor prompt legitimately moves every score. A baseline keyed
// only on (provider, model, effort) would compare the new prompt's numbers against
// the old prompt's and report a regression that is really just a different
// measurement — which would either block every prompt change or, worse, train
// people to ignore the gate. Keyed WITH the digest, the honest answer is "no
// baseline for this prompt yet".
func TestBaseline_PromptChangeIsNotARegression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	old := reportFor("sha-old", map[Ability]float64{AbilityProperty: 1.0, AbilityUpdate: 1.0}, nil)
	if err := SaveBaselineEntry(path, old); err != nil {
		t.Fatalf("save: %v", err)
	}
	b, _ := LoadBaseline(path)

	// Same model, DIFFERENT prompt, much worse scores.
	edited := reportFor("sha-new", map[Ability]float64{AbilityProperty: 0.0, AbilityUpdate: 0.0}, nil)
	if got := b.Regressions(edited); len(got) != 0 {
		t.Errorf("a different prompt is a different measurement, not a regression: %v", got)
	}
	if _, ok := b.EntryFor(edited); ok {
		t.Error("the edited prompt must not match the old entry")
	}
	if d := b.DeltaFor(edited, AbilityScore{Ability: AbilityProperty, Recall: 0}); !strings.Contains(d, "new") {
		t.Errorf("the table should mark it new, got %q", d)
	}
}

// TestBaseline_CorpusChangeIsNotARegression is the corpus half of the
// prompt-digest argument, and it was verified against a real near-miss: a baseline
// had just been recorded when the buried property case was added. Without the
// corpus digest in the key, the very next run reported "property recall fell
// 1.00 -> 0.50" — a spurious regression caused entirely by adding a harder case,
// which is how a gate teaches people to pass --no-gate.
func TestBaseline_CorpusChangeIsNotARegression(t *testing.T) {
	b := Baseline{RecallTolerance: DefaultRecallTolerance, Entries: []BaselineEntry{{
		Provider: "p", Model: "m", Effort: "medium",
		SystemPromptSHA256: "sha", CorpusSHA256: "corpus-old",
		Recall:     map[string]float64{"property": 1.0},
		Violations: map[string]int{"property": 0},
	}}}
	newCorpus := ExtractionReport{
		Provider: "p", Model: "m", Effort: "medium",
		SystemPromptSHA256: "sha", CorpusSHA256: "corpus-new",
		Abilities: []AbilityScore{{Ability: AbilityProperty, Recall: 0.5}},
	}
	if got := b.Regressions(newCorpus); len(got) != 0 {
		t.Fatalf("a different corpus is a different measurement, not a regression: %v", got)
	}
	if _, ok := b.EntryFor(newCorpus); ok {
		t.Error("a run against a new corpus must not match the old entry")
	}
}

// TestSaveBaselineEntry_RefusesAFaultedRun: recording scores from a run whose
// canary failed would bake 0.0 in as the number to beat, and the next run would
// then look like an improvement.
func TestSaveBaselineEntry_RefusesAFaultedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	faulted := reportFor("sha-a", map[Ability]float64{AbilityProperty: 0.0}, nil)
	faulted.HarnessFault = "canary returned no facts"
	err := SaveBaselineEntry(path, faulted)
	if err == nil {
		t.Fatal("a harness-faulted run must not be recorded as a baseline")
	}
	if !strings.Contains(err.Error(), "harness-faulted") {
		t.Errorf("the refusal should say why: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("no baseline file should have been written")
	}
}

func TestSaveBaselineEntry_RefusesAnEmptyReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := SaveBaselineEntry(path, ExtractionReport{Provider: "p", Model: "m"}); err == nil {
		t.Fatal("a report with no ability scores must not be recorded")
	}
}

func TestSaveBaselineEntry_UpsertsAndSortsStably(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	a := reportFor("sha-a", map[Ability]float64{AbilityUpdate: 1.0}, nil)
	bRep := a
	bRep.Model = "aaa-first-alphabetically"
	for _, r := range []ExtractionReport{a, bRep, a} { // `a` twice → upsert, not duplicate
		if err := SaveBaselineEntry(path, r); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	loaded, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("want 2 entries after an upsert, got %d", len(loaded.Entries))
	}
	if loaded.Entries[0].Key() > loaded.Entries[1].Key() {
		t.Error("entries must be sorted by key so the committed file diffs cleanly")
	}
	for _, e := range loaded.Entries {
		if e.MeasuredAt == "" {
			t.Error("entries should record when they were measured, so a reader knows how stale the number is")
		}
	}
}

// TestBaseline_ShippedFileParses guards the committed baseline: a malformed one
// would fail every gated run with a parse error instead of a score.
func TestBaseline_ShippedFileParses(t *testing.T) {
	b, err := LoadBaseline("extraction-baseline.json")
	if err != nil {
		t.Fatalf("the committed baseline must parse: %v", err)
	}
	if b.RecallTolerance <= 0 {
		t.Error("the committed baseline should carry a positive recall tolerance")
	}
	// Every entry must name the prompt it was measured against, or it cannot be
	// matched to a run and is dead weight.
	for i, e := range b.Entries {
		if e.SystemPromptSHA256 == "" {
			t.Errorf("entry %d has no prompt digest, so it can never match a run", i)
		}
		if e.Provider == "" || e.Model == "" {
			t.Errorf("entry %d is missing provider/model", i)
		}
	}
}

// TestBaseline_ShippedFileIsValidJSONShape catches a hand-edit that produced a
// structurally-valid but semantically-wrong file (e.g. entries as an object).
func TestBaseline_ShippedFileIsValidJSONShape(t *testing.T) {
	raw, err := os.ReadFile("extraction-baseline.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var probe struct {
		RecallTolerance *float64          `json:"recall_tolerance"`
		Entries         []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("shape: %v", err)
	}
	if probe.RecallTolerance == nil {
		t.Error("recall_tolerance should be explicit in the committed file, not left to the default")
	}
}
