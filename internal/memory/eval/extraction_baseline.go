package eval

// The extraction eval's committed baseline.
//
// WHY A FILE AND NOT A JUDGEMENT CALL. A score on its own says nothing: "property
// recall 0.5" is only information next to what it was yesterday. Every tuning
// session before this one compared against memory — "I think abstention was fine
// last time" — which is how a regression survives a review. The baseline turns
// each ability into a number a run can be measured against, and a drop into a
// gate failure that names the ability and both values.
//
// KEYED BY (provider, model, effort, PROMPT DIGEST, CORPUS DIGEST). The two
// digests are the part that is easy to leave out and expensive to omit: change the
// extractor prompt, or add a case to the corpus, and every score legitimately
// moves. A baseline that ignored either would block the change as a regression, or
// worse, silently compare across incomparable runs. With both in the key, editing
// a prompt or a fixture produces "no baseline for this yet" — which is the truth.
//
// The corpus digest was added after the first live run, when adding a realistic
// case would have silently invalidated a stored figure with nothing to signal it.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Measured is a run a baseline can key on and compare: the five key fields plus the
// per-ability numbers. Both the extraction and the judge reports implement it.
//
// AN INTERFACE RATHER THAN A SECOND COPY of this file, because nothing here is specific
// to extraction — and the refusals below (a harness-faulted run, an incomplete run, a run
// that emitted forbidden material) are exactly the rules that would drift apart if the
// judge had its own baseline writer. They were each added after a real incident; having
// them in one place is the point.
type Measured interface {
	// baselineKeyFields returns (provider, model, effort, promptSHA, corpusSHA).
	baselineKeyFields() (string, string, string, string, string)
	// AbilityScores are the per-ability numbers to record and compare.
	AbilityScores() []AbilityScore
	// Incomplete reports (errors, harnessFault) — why a run's numbers may not be
	// recordable.
	Incomplete() (int, string)
}

// BaselineEntry is one measured run's scores.
type BaselineEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	// SystemPromptSHA256 is the extractor prompt these numbers were measured
	// against. See the file header for why it is part of the key.
	SystemPromptSHA256 string `json:"system_prompt_sha256"`
	// CorpusSHA256 is the fixture set these numbers were measured against.
	CorpusSHA256 string `json:"corpus_sha256"`
	// MeasuredAt is informational — it tells a reader how stale the number is. It
	// is NOT part of the key.
	MeasuredAt string `json:"measured_at,omitempty"`
	// Recall per ability. Absent = the ability asserts no positive expectations.
	Recall map[string]float64 `json:"recall,omitempty"`
	// Violations per ability. Present even at 0, because 0 is the interesting value.
	Violations map[string]int `json:"violations"`
}

// Key identifies the run this entry describes.
func (e BaselineEntry) Key() string {
	return e.Provider + "|" + e.Model + "|" + e.Effort + "|" + e.SystemPromptSHA256 + "|" + e.CorpusSHA256
}

// Baseline is the committed set of measured runs.
type Baseline struct {
	// RecallTolerance is how far recall may fall below the baseline before it
	// counts as a regression. Non-zero because a live model is not deterministic:
	// the same model on the same corpus will disagree with itself at the margin,
	// and a zero-tolerance gate would fail on noise and teach everyone to pass
	// --no-gate. A drop larger than this is a signal, not jitter.
	RecallTolerance float64         `json:"recall_tolerance"`
	Entries         []BaselineEntry `json:"entries"`
}

// DefaultRecallTolerance is the shipped allowance. One case out of the smallest
// ability (a single-case ability moves in steps of 1.0, a two-case one in 0.5), so
// this admits sub-case jitter without admitting a lost case.
const DefaultRecallTolerance = 0.15

// LoadBaseline reads a baseline file. A MISSING file is not an error — the first
// run against a new model legitimately has nothing to compare to, and forcing the
// operator to create an empty file first would just be friction.
func LoadBaseline(path string) (Baseline, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Baseline{RecallTolerance: DefaultRecallTolerance}, nil
	}
	if err != nil {
		return Baseline{}, fmt.Errorf("read %s: %w", path, err)
	}
	var out Baseline
	if err := json.Unmarshal(b, &out); err != nil {
		return Baseline{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if out.RecallTolerance <= 0 {
		out.RecallTolerance = DefaultRecallTolerance
	}
	return out, nil
}

// EntryFor returns the baseline entry matching a report's exact key.
func (b Baseline) EntryFor(r Measured) (BaselineEntry, bool) {
	provider, model, effort, promptSHA, corpusSHA := r.baselineKeyFields()
	want := BaselineEntry{
		Provider: provider, Model: model, Effort: effort,
		SystemPromptSHA256: promptSHA, CorpusSHA256: corpusSHA,
	}.Key()
	for _, e := range b.Entries {
		if e.Key() == want {
			return e, true
		}
	}
	return BaselineEntry{}, false
}

// StaleMatch reports a baseline entry recorded for this SAME provider/model/effort
// but under a different system prompt or corpus, when there is no exact match.
//
// EntryFor requires all five fields, and a run with no exact match is deliberately
// not a regression — an unmeasured model must not be blocked. But that policy hides
// a second, very different situation: a configuration that WAS measured and whose
// gate silently expired because the extractor prompt changed. Editing that prompt
// un-gates every model/effort combination at once, invisibly, and the next run
// reports a clean pass because there is nothing to compare against.
//
// Observed in the recorded baseline: effort=medium was measured under prompt
// 7104816985db9c7c, the prompt then changed to cf2736fd62c80291, and a medium run
// has been ungated since — reported as neither a regression nor a gap.
//
// This does not block. It exists so the difference between "never measured" and
// "no longer gated" is visible at the point the gate would have spoken.
func (b Baseline) StaleMatch(r Measured) (BaselineEntry, bool) {
	if _, exact := b.EntryFor(r); exact {
		return BaselineEntry{}, false
	}
	provider, model, effort, _, _ := r.baselineKeyFields()
	for _, e := range b.Entries {
		if e.Provider == provider && e.Model == model && e.Effort == effort {
			return e, true
		}
	}
	return BaselineEntry{}, false
}

// Regressions reports every ability that got WORSE than the baseline, beyond
// tolerance. A run with no matching baseline entry reports nothing: an unmeasured
// model is not a regression, and pretending otherwise would block the first run
// against every new candidate.
//
// A new violation is ALWAYS a regression regardless of tolerance — tolerance
// exists for recall jitter, and there is no jitter that turns "stored no secrets"
// into "stored one".
func (b Baseline) Regressions(r Measured) []string {
	base, ok := b.EntryFor(r)
	if !ok {
		return nil
	}
	var out []string
	for _, s := range r.AbilityScores() {
		name := string(s.Ability)
		if was, had := base.Violations[name]; had && s.Violations > was {
			out = append(out, fmt.Sprintf("%s violations rose %d → %d against the baseline", name, was, s.Violations))
		}
		if was, had := base.Recall[name]; had && s.Recall >= 0 && s.Recall < was-b.RecallTolerance {
			out = append(out, fmt.Sprintf("%s recall fell %.2f → %.2f against the baseline (tolerance %.2f)",
				name, was, s.Recall, b.RecallTolerance))
		}
	}
	sort.Strings(out)
	return out
}

// DeltaFor renders an ability's change against the baseline for the report table,
// or "" when there is nothing to compare.
func (b Baseline) DeltaFor(r Measured, s AbilityScore) string {
	base, ok := b.EntryFor(r)
	if !ok {
		return "   (new)"
	}
	name := string(s.Ability)
	if was, had := base.Recall[name]; had && s.Recall >= 0 {
		d := s.Recall - was
		switch {
		case d > 0.001:
			return fmt.Sprintf("  +%.2f", d)
		case d < -0.001:
			return fmt.Sprintf("  %.2f", d)
		default:
			return "   ="
		}
	}
	if was, had := base.Violations[name]; had {
		if s.Violations != was {
			return fmt.Sprintf("  %+d viol", s.Violations-was)
		}
		return "   ="
	}
	return ""
}

// SaveBaselineEntry upserts this report's scores into the baseline at path and
// writes it back, entries sorted by key so the committed file diffs cleanly.
//
// It REFUSES a harness-faulted report. Recording scores from a run whose canary
// failed would bake "0.0 recall" in as the number to beat, and the next run would
// then look like an improvement.
func SaveBaselineEntry(path string, r Measured) error {
	totalErrors, harnessFault := r.Incomplete()
	abilities := r.AbilityScores()
	if harnessFault != "" {
		return fmt.Errorf("refusing to record a baseline from a harness-faulted run: %s", harnessFault)
	}
	if len(abilities) == 0 {
		return fmt.Errorf("refusing to record a baseline with no ability scores")
	}
	// Same argument as the harness-fault refusal above, for the case that actually
	// happened: a slow local model exhausted the run budget, five cases never ran,
	// and the resulting `update 0.00` was written in as the number to beat. A
	// partial run's figures describe a subset of the corpus, so recording them
	// makes the next full run look like an improvement.
	if totalErrors > 0 {
		return fmt.Errorf("refusing to record a baseline from an INCOMPLETE run: %d case(s) never produced an answer, so these numbers describe only part of the corpus", totalErrors)
	}
	// A run that emitted forbidden material is not a reference point. Recording it
	// makes the violations the accepted norm: Regressions() only fires when a count
	// RISES above the baseline, so a model that leaked 4 things would thereafter
	// leak 4 things without comment.
	//
	// This is not hypothetical — a model measured for comparison stored a
	// credential mention, chatter, and a prompt-injection attempt as a durable
	// user PREFERENCE, and all four violations were written in as its baseline.
	// The gate still refuses such a run; the baseline must not quietly accept it.
	violations := 0
	for _, s := range abilities {
		violations += s.Violations
	}
	if violations > 0 {
		return fmt.Errorf("refusing to record a baseline from a run with %d violation(s): a baseline is a reference for how a healthy run looks, and recording forbidden emissions makes them the norm (Regressions only fires when a count RISES)", violations)
	}
	b, err := LoadBaseline(path)
	if err != nil {
		return err
	}
	provider, model, effort, promptSHA, corpusSHA := r.baselineKeyFields()
	entry := BaselineEntry{
		Provider: provider, Model: model, Effort: effort,
		SystemPromptSHA256: promptSHA,
		CorpusSHA256:       corpusSHA,
		MeasuredAt:         time.Now().UTC().Format(time.RFC3339),
		Recall:             map[string]float64{},
		Violations:         map[string]int{},
	}
	for _, s := range abilities {
		if s.Recall >= 0 {
			entry.Recall[string(s.Ability)] = s.Recall
		}
		entry.Violations[string(s.Ability)] = s.Violations
	}

	replaced := false
	for i := range b.Entries {
		if b.Entries[i].Key() == entry.Key() {
			// Preserve nothing from the old entry: a re-measurement replaces it
			// wholesale, including dropping an ability that no longer scores.
			b.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		b.Entries = append(b.Entries, entry)
	}
	sort.Slice(b.Entries, func(i, j int) bool { return b.Entries[i].Key() < b.Entries[j].Key() })
	if b.RecallTolerance <= 0 {
		b.RecallTolerance = DefaultRecallTolerance
	}

	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
