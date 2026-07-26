package eval

// Threshold calibration for the consolidation similarity bands.
//
// WHY THIS EXISTS. `memory.consolidation.merge_threshold` decides whether the
// consolidator treats two facts as one fact reworded. Cosine scale is a
// property of the EMBEDDING MODEL, not a universal, so any constant is right
// for at most one model — and a wrong one fails silently in both directions: a
// band nothing reaches never merges (duplicates accumulate forever, with no
// error anywhere), and a band everything reaches merges distinct facts (data
// loss). Neither shows up in a log line. This measures the bands against a
// labelled corpus on the operator's OWN embedder, so the number is attributable
// to a model instead of inherited from someone else's.
//
// The measurement is deliberately small and boring: embed a labelled corpus,
// take every probe-vs-base cosine, and look at where the three classes land.
// No ranker, no store, no backend — a threshold question is answered by the
// raw similarity, and putting retrieval machinery in the path would only add
// ways for the answer to be wrong.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// builtinCalibrationCorpus is the corpus the reference measurement used, so
// `loomcycle memory-calibrate` runs with zero setup and the documented
// embeddinggemma numbers are reproducible.
//
//go:embed calibration.json
var builtinCalibrationCorpus []byte

// clusteredCalibrationCorpus is the region the builtin corpus does not sample.
//
// WHY A SECOND CORPUS RATHER THAN MORE FACTS IN THE FIRST. The builtin corpus is
// twelve MUTUALLY UNRELATED subjects, so every pair it labels UNRELATED is also
// cross-TOPIC. A real memory scope is not shaped like that: it is a handful of
// facts about the same few things, and the pairs a merge threshold actually has
// to clear are same-topic, different-subject ones the builtin corpus contains
// none of. A threshold measured only on it is therefore unvalidated exactly
// where it does damage — which is how a live store came to hold
//
//	memory/fact/user-downloaded-qwen3-6-27b-q4
//	  -> "The user's model is gemma-4-12b-it-UD-Q4_K_XL.gguf."
//
// under a band that made zero false merges on the builtin corpus.
//
// The two stay separate so the published reference measurement keeps meaning
// what it says: those numbers were measured against the twelve-fact corpus, and
// silently changing what that name refers to would make the documented table
// describe a corpus nobody ran. Run both; the recommendation an operator should
// act on is the more conservative of the two.
//
//go:embed calibration-cluster.json
var clusteredCalibrationCorpus []byte

// The three labels a probe-vs-base pair can carry.
const (
	ClassDuplicate = "duplicate"
	ClassRelated   = "related"
	ClassUnrelated = "unrelated"
)

// relatedRecallTarget is the fraction of the RELATED class the recommended
// related_threshold must still catch.
//
// Why a recall target rather than an accuracy optimum: for the related band the
// two error directions are close to symmetric in cost (a missed relation loses
// a link; a spurious one adds noise) and — on every model measured so far — the
// RELATED and UNRELATED classes OVERLAP, so no threshold is clean and an
// "optimal" point is really an artefact of wherever the unrelated mass happens
// to sit. A stated recall target is a decision an operator can read, argue
// with, and change; an argmax over a synthetic corpus is not.
const relatedRecallTarget = 0.75

// CalibrationCorpus is the labelled input: base facts, plus probes labelled
// against the base they belong to.
//
// UNRELATED is DERIVED, never listed: every probe scored against a base that is
// not its own is an unrelated pair. Hand-listing them would be N² of
// error-prone bookkeeping, and the derivation is what makes an operator's own
// corpus cheap to write — add a fact and its two probes, and the negative
// samples appear on their own.
type CalibrationCorpus struct {
	Name string `json:"name"`
	// Note is free text carried through to nothing — it exists so the
	// bundled corpus can explain itself to whoever opens the file.
	Note string `json:"note,omitempty"`
	// Bases are the reference facts. Probes index into this list, 1-based.
	Bases []string `json:"bases"`
	// Duplicates restate their base fact in different words. Each one MUST
	// land above the merge threshold, or the threshold is inert.
	Duplicates []LabeledProbe `json:"duplicates"`
	// Related share their base's subject but make a DIFFERENT claim. Each one
	// must land BELOW the merge threshold, or merging destroys a fact.
	Related []LabeledProbe `json:"related"`
}

// LabeledProbe is one probe text and the 1-based index of the base it belongs
// to. 1-based because the corpus file is written and read by humans.
type LabeledProbe struct {
	Text string `json:"text"`
	Base int    `json:"base"`
}

// BuiltinCalibrationCorpus parses the embedded corpus.
func BuiltinCalibrationCorpus() (CalibrationCorpus, error) {
	return LoadCalibrationCorpus(bytes.NewReader(builtinCalibrationCorpus))
}

// ClusteredCalibrationCorpus parses the embedded dense-topic corpus — see
// clusteredCalibrationCorpus for why it exists and why it is not merged into
// the builtin one.
func ClusteredCalibrationCorpus() (CalibrationCorpus, error) {
	return LoadCalibrationCorpus(bytes.NewReader(clusteredCalibrationCorpus))
}

// LoadCalibrationCorpus reads a corpus from JSON. Single-object JSON rather
// than the eval harness's JSONL: a corpus is one object, and the JSONL split
// exists there only because a dataset has many query lines.
func LoadCalibrationCorpus(r io.Reader) (CalibrationCorpus, error) {
	var c CalibrationCorpus
	b, err := io.ReadAll(r)
	if err != nil {
		return CalibrationCorpus{}, fmt.Errorf("read corpus: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return CalibrationCorpus{}, fmt.Errorf("parse corpus: %w", err)
	}
	if err := c.Validate(); err != nil {
		return CalibrationCorpus{}, err
	}
	return c, nil
}

// Validate rejects a corpus that cannot produce a meaningful measurement. A
// mislabelled probe is indistinguishable from a bad model in the output, so
// the structural checks that CAN be made are made here rather than surfacing
// as a confusing verdict later.
func (c CalibrationCorpus) Validate() error {
	if len(c.Bases) < 2 {
		return fmt.Errorf("corpus: need at least 2 base facts (got %d) — with one base there are no unrelated pairs", len(c.Bases))
	}
	if len(c.Duplicates) == 0 {
		return fmt.Errorf("corpus: need at least one duplicate probe")
	}
	if len(c.Related) == 0 {
		return fmt.Errorf("corpus: need at least one related probe")
	}
	for _, group := range []struct {
		name   string
		probes []LabeledProbe
	}{{"duplicates", c.Duplicates}, {"related", c.Related}} {
		for i, p := range group.probes {
			if strings.TrimSpace(p.Text) == "" {
				return fmt.Errorf("corpus: %s[%d] has empty text", group.name, i)
			}
			if p.Base < 1 || p.Base > len(c.Bases) {
				return fmt.Errorf("corpus: %s[%d] base=%d out of range 1..%d", group.name, i, p.Base, len(c.Bases))
			}
		}
	}
	return nil
}

// ScoredPair is one probe-vs-base cosine and the class it was labelled with.
// Probe/Base are 1-based indices, omitted when a report is built from measured
// numbers rather than from an embedding run.
type ScoredPair struct {
	Class string  `json:"class"`
	Probe int     `json:"probe,omitempty"`
	Base  int     `json:"base,omitempty"`
	Score float64 `json:"score"`
}

// ClassStats is one class's distribution.
type ClassStats struct {
	Class  string  `json:"class"`
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	P05    float64 `json:"p05"`
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
}

// SweepRow is one candidate threshold and what it would do to the corpus.
// FalseMerges counts BOTH non-duplicate classes: a merge does not care whether
// the fact it destroyed was related or unrelated.
type SweepRow struct {
	Threshold        float64 `json:"threshold"`
	DuplicatesMerged int     `json:"duplicates_merged"`
	FalseMerges      int     `json:"false_merges"`
	RelatedCaught    int     `json:"related_caught"`
	UnrelatedFlagged int     `json:"unrelated_flagged"`
}

// EmbedderInfo attributes a result to a model. Provider/model/dimension only —
// this is the same non-secret triple `Context op=capabilities` reports, and for
// the same reason: a calibration number is meaningless without the model, and a
// base URL is the operator's network, not a capability.
type EmbedderInfo struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
}

// ConfiguredBands is what the loaded config would use right now.
type ConfiguredBands struct {
	MergeThreshold   float64 `json:"merge_threshold"`
	RelatedThreshold float64 `json:"related_threshold"`

	// Outcomes of those two thresholds ON THIS CORPUS.
	DuplicatesMerged int `json:"duplicates_merged"`
	FalseMerges      int `json:"false_merges"`
	RelatedCaught    int `json:"related_caught"`
	UnrelatedFlagged int `json:"unrelated_flagged"`

	// MergeInert: no duplicate reaches the merge band, so merging can never
	// fire and duplicates accumulate silently. This is the failure the
	// command exists to make visible.
	MergeInert bool `json:"merge_inert"`
	// MergeDestructive: some NON-duplicate reaches the merge band, so the
	// pass would merge two distinct facts. Unrecoverable, unlike inertness.
	MergeDestructive bool `json:"merge_destructive"`
	// RelatedInert: no related probe reaches the related band.
	RelatedInert bool `json:"related_inert"`
}

// CalibrationReport is the whole measurement.
type CalibrationReport struct {
	Corpus   string       `json:"corpus"`
	Embedder EmbedderInfo `json:"embedder"`
	Classes  []ClassStats `json:"classes"`

	// DuplicateVsRelatedGap is min(DUPLICATE) - max(RELATED); positive means
	// the two classes are separated by that much cosine.
	DuplicateVsRelatedGap float64 `json:"duplicate_vs_related_gap"`
	// RelatedVsUnrelatedGap is min(RELATED) - max(UNRELATED). Negative on
	// every model measured so far — see RelatedUnrelatedOverlap.
	RelatedVsUnrelatedGap float64 `json:"related_vs_unrelated_gap"`

	// Separable reports whether ANY threshold merges every duplicate and no
	// non-duplicate. This is the question the exit code answers.
	Separable bool `json:"separable"`
	// SafeWindowLow is exclusive (max non-duplicate), SafeWindowHigh
	// inclusive (min duplicate). Only meaningful when Separable.
	SafeWindowLow  float64 `json:"safe_window_low"`
	SafeWindowHigh float64 `json:"safe_window_high"`

	RecommendedMerge   float64 `json:"recommended_merge_threshold"`
	RecommendedRelated float64 `json:"recommended_related_threshold"`
	// RelatedUnrelatedOverlap: the related and unrelated classes cross, so
	// no related_threshold separates them and the recommendation is a
	// recall/false-positive trade-off, not a clean split.
	RelatedUnrelatedOverlap bool `json:"related_unrelated_overlap"`
	// RecommendedRelatedRecall / RecommendedRelatedFalsePositives are what
	// the recommended related band actually achieves, so the trade-off is
	// stated rather than implied.
	RecommendedRelatedRecall         float64 `json:"recommended_related_recall"`
	RecommendedRelatedFalsePositives int     `json:"recommended_related_false_positives"`

	Sweep      []SweepRow      `json:"sweep"`
	Configured ConfiguredBands `json:"configured"`

	// Pairs is the raw measurement, so a result can be re-analysed without
	// re-embedding (and so a reviewer can check the labels).
	Pairs []ScoredPair `json:"pairs,omitempty"`
}

// Calibrate embeds the corpus with `emb` and analyses the result.
//
// Every probe is scored against EVERY base — the full pairwise matrix, not a
// top-K neighbourhood. A truncated read is cheaper but hides the highest
// unrelated similarities, which are precisely the pairs a merge threshold has
// to clear; the full matrix can only move max(UNRELATED) up, which is the
// conservative direction for a destructive operation.
func Calibrate(ctx context.Context, corpus CalibrationCorpus, emb providers.Embedder, configured ConfiguredBands) (CalibrationReport, error) {
	if err := corpus.Validate(); err != nil {
		return CalibrationReport{}, err
	}
	if emb == nil {
		return CalibrationReport{}, fmt.Errorf("calibrate: no embedder")
	}

	texts := make([]string, 0, len(corpus.Bases)+len(corpus.Duplicates)+len(corpus.Related))
	texts = append(texts, corpus.Bases...)
	for _, p := range corpus.Duplicates {
		texts = append(texts, p.Text)
	}
	for _, p := range corpus.Related {
		texts = append(texts, p.Text)
	}

	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return CalibrationReport{}, fmt.Errorf("embed corpus: %w", err)
	}
	if len(vecs) != len(texts) {
		return CalibrationReport{}, fmt.Errorf("embed corpus: got %d vectors for %d texts", len(vecs), len(texts))
	}

	nBases := len(corpus.Bases)
	baseVecs := vecs[:nBases]
	dupVecs := vecs[nBases : nBases+len(corpus.Duplicates)]
	relVecs := vecs[nBases+len(corpus.Duplicates):]

	var pairs []ScoredPair
	collect := func(probes []LabeledProbe, probeVecs [][]float32, ownClass string) {
		for i, p := range probes {
			for b := range baseVecs {
				class := ClassUnrelated
				if b == p.Base-1 {
					class = ownClass
				}
				pairs = append(pairs, ScoredPair{
					Class: class,
					Probe: i + 1,
					Base:  b + 1,
					Score: cosine(probeVecs[i], baseVecs[b]),
				})
			}
		}
	}
	collect(corpus.Duplicates, dupVecs, ClassDuplicate)
	collect(corpus.Related, relVecs, ClassRelated)

	info := EmbedderInfo{Provider: emb.Provider(), Model: emb.Model(), Dimension: emb.Dimension()}
	// A driver that does not know its own width reports 0 (the v1.33.1
	// dimension: 0 bug). The vectors are right here, so report what was
	// actually returned rather than propagating the zero.
	if info.Dimension == 0 && len(vecs[0]) > 0 {
		info.Dimension = len(vecs[0])
	}

	rep := Analyze(corpus.Name, info, pairs, configured)
	return rep, nil
}

// Analyze turns labelled pairs into the report. Split from Calibrate so the
// statistics can be driven by MEASURED numbers — the reference embeddinggemma
// run is a test fixture that goes straight through this function, which is how
// the documented table stays pinned in CI without a live embedder.
func Analyze(corpusName string, emb EmbedderInfo, pairs []ScoredPair, configured ConfiguredBands) CalibrationReport {
	dup := scoresOf(pairs, ClassDuplicate)
	rel := scoresOf(pairs, ClassRelated)
	unrel := scoresOf(pairs, ClassUnrelated)
	nonDup := append(append([]float64{}, rel...), unrel...)

	rep := CalibrationReport{
		Corpus:   corpusName,
		Embedder: emb,
		Classes: []ClassStats{
			statsFor(ClassDuplicate, dup),
			statsFor(ClassRelated, rel),
			statsFor(ClassUnrelated, unrel),
		},
		Pairs: pairs,
	}

	dupMin, dupOK := minOf(dup)
	relMax, relOK := maxOf(rel)
	relMin, _ := minOf(rel)
	unrelMax, unrelOK := maxOf(unrel)
	nonDupMax, nonDupOK := maxOf(nonDup)

	if dupOK && relOK {
		rep.DuplicateVsRelatedGap = dupMin - relMax
	}
	if relOK && unrelOK {
		rep.RelatedVsUnrelatedGap = relMin - unrelMax
		rep.RelatedUnrelatedOverlap = rep.RelatedVsUnrelatedGap <= 0
	}

	// Separability is judged against EVERY non-duplicate, not just the
	// related class: a merge does not care which class the fact it destroyed
	// came from, and on a model where unrelated outscores related, using
	// max(RELATED) alone would report a safe window that is not safe.
	if dupOK && nonDupOK {
		rep.Separable = dupMin > nonDupMax
		rep.SafeWindowLow, rep.SafeWindowHigh = nonDupMax, dupMin
	}

	switch {
	case rep.Separable:
		// Midpoint of the safe window: the furthest any threshold can sit
		// from both failure modes at once.
		mid := (nonDupMax + dupMin) / 2
		rep.RecommendedMerge = round4(mid)
		// Rounding must never push the recommendation OUT of the window. On a
		// window narrower than the 4-decimal grid (e.g. (0.70001, 0.70002])
		// round4 lands at or below its floor, and a threshold at or below the
		// floor MERGES a distinct fact — the unrecoverable direction. Keep the
		// exact midpoint in that case: an ugly number beats a destructive one.
		if rep.RecommendedMerge <= nonDupMax || rep.RecommendedMerge > dupMin {
			rep.RecommendedMerge = mid
		}
	case nonDupOK:
		// No window exists. Recommend on the ASYMMETRY rather than on an
		// accuracy score: a threshold that is too high leaves duplicates
		// lying around (untidy, and every one of them is still recoverable),
		// a threshold that is too low merges distinct facts (data loss). So
		// recommend the lowest grid value that still makes zero false
		// merges, and let the report show how few duplicates it catches.
		rep.RecommendedMerge = gridAbove(nonDupMax)
	}

	rep.RecommendedRelated = recommendRelated(rel, rep.RecommendedMerge)
	if len(rel) > 0 {
		rep.RecommendedRelatedRecall = float64(countAtLeast(rel, rep.RecommendedRelated)) / float64(len(rel))
	}
	rep.RecommendedRelatedFalsePositives = countAtLeast(unrel, rep.RecommendedRelated)

	rep.Sweep = sweep(dup, rel, unrel, nonDup, []float64{
		rep.RecommendedMerge, rep.RecommendedRelated,
		configured.MergeThreshold, configured.RelatedThreshold,
	})

	rep.Configured = configured
	rep.Configured.DuplicatesMerged = countAtLeast(dup, configured.MergeThreshold)
	rep.Configured.FalseMerges = countAtLeast(nonDup, configured.MergeThreshold)
	rep.Configured.RelatedCaught = countAtLeast(rel, configured.RelatedThreshold)
	rep.Configured.UnrelatedFlagged = countAtLeast(unrel, configured.RelatedThreshold)
	rep.Configured.MergeInert = len(dup) > 0 && rep.Configured.DuplicatesMerged == 0
	rep.Configured.MergeDestructive = rep.Configured.FalseMerges > 0
	rep.Configured.RelatedInert = len(rel) > 0 && rep.Configured.RelatedCaught == 0

	return rep
}

// recommendRelated returns the highest 0.01-grid threshold that still recalls
// relatedRecallTarget of the related class, kept strictly below the merge band
// (above it a "related" pair would have merged instead).
func recommendRelated(rel []float64, merge float64) float64 {
	if len(rel) == 0 {
		return 0
	}
	desc := append([]float64{}, rel...)
	sort.Sort(sort.Reverse(sort.Float64Slice(desc)))
	// k-th largest is the lowest score the target still includes.
	k := int(math.Ceil(relatedRecallTarget * float64(len(desc))))
	if k < 1 {
		k = 1
	}
	if k > len(desc) {
		k = len(desc)
	}
	t := gridBelow(desc[k-1])
	if merge > 0 && t >= merge {
		t = gridStrictlyBelow(merge)
	}
	if t < 0.01 {
		t = 0.01
	}
	return round4(t)
}

// sweep renders the threshold table: a fixed 0.30..0.95 grid so two runs are
// comparable line-for-line, plus whatever extra points matter for this run
// (the recommendations and the configured bands), so the numbers an operator
// is about to act on are never off the table.
func sweep(dup, rel, unrel, nonDup []float64, extra []float64) []SweepRow {
	seen := map[float64]bool{}
	var ts []float64
	add := func(t float64) {
		t = round4(t)
		if t <= 0 || t > 1 || seen[t] {
			return
		}
		seen[t] = true
		ts = append(ts, t)
	}
	for t := 0.30; t <= 0.9501; t += 0.05 {
		add(t)
	}
	for _, t := range extra {
		add(t)
	}
	sort.Float64s(ts)

	rows := make([]SweepRow, 0, len(ts))
	for _, t := range ts {
		rows = append(rows, SweepRow{
			Threshold:        t,
			DuplicatesMerged: countAtLeast(dup, t),
			FalseMerges:      countAtLeast(nonDup, t),
			RelatedCaught:    countAtLeast(rel, t),
			UnrelatedFlagged: countAtLeast(unrel, t),
		})
	}
	return rows
}

func scoresOf(pairs []ScoredPair, class string) []float64 {
	var out []float64
	for _, p := range pairs {
		if p.Class == class {
			out = append(out, p.Score)
		}
	}
	return out
}

func countAtLeast(scores []float64, t float64) int {
	n := 0
	for _, s := range scores {
		if s >= t {
			n++
		}
	}
	return n
}

func minOf(s []float64) (float64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m, true
}

func maxOf(s []float64) (float64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	m := s[0]
	for _, v := range s[1:] {
		if v > m {
			m = v
		}
	}
	return m, true
}

func statsFor(class string, scores []float64) ClassStats {
	st := ClassStats{Class: class, N: len(scores)}
	if len(scores) == 0 {
		return st
	}
	s := append([]float64{}, scores...)
	sort.Float64s(s)
	st.Min = round4(s[0])
	st.Max = round4(s[len(s)-1])
	st.P05 = round4(percentile(s, 0.05))
	st.Median = round4(percentile(s, 0.50))
	st.P95 = round4(percentile(s, 0.95))
	return st
}

// percentile is linear interpolation between the two ranks straddling
// p*(n-1) — the numpy/pandas default, chosen so an operator cross-checking a
// run against the same scores in a notebook gets the same number rather than
// a nearest-rank value that differs by a whole sample on a 12-sample class.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := p * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo < 0 {
		lo = 0
	}
	if hi >= n {
		hi = n - 1
	}
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// gridAbove returns the smallest 0.01-grid value STRICTLY above x (capped at
// 1.0). Used where a threshold must exclude x — `score >= threshold` merges,
// so landing exactly on x would include it.
func gridAbove(x float64) float64 {
	v := (math.Floor(round4(x)*100) + 1) / 100
	if v > 1 {
		v = 1
	}
	return round4(v)
}

// gridBelow returns the largest 0.01-grid value at or below x — the highest
// threshold that still INCLUDES x.
func gridBelow(x float64) float64 {
	return round4(math.Floor(round4(x)*100) / 100)
}

// gridStrictlyBelow returns the largest 0.01-grid value strictly below x.
func gridStrictlyBelow(x float64) float64 {
	v := gridBelow(x)
	if v >= x {
		v = round4(v - 0.01)
	}
	return v
}

// round4 keeps every reported and compared value on a 4-decimal grid, so
// binary-float dust cannot make two runs of the same data disagree in the
// last digit or push a sweep threshold off its intended value.
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
