package eval

import (
	"context"
	"math"
	"strings"
	"testing"
)

// calibDim is wide enough to give every base its own axis plus two spare
// "noise" axes to tilt a probe into.
const calibDim = 16

// controlledCorpus builds a 3-base corpus whose every cosine is EXACTLY what
// the test asks for: each base is a unit axis, and each probe is that axis
// tilted by a stated cosine into another axis. Tilting into a spare axis
// (>= len(bases)) makes the probe orthogonal to every other base, so unrelated
// pairs score 0; tilting into ANOTHER BASE's axis puts a chosen value on that
// unrelated pair. Nothing here depends on tokenization, dimension, or hash
// collisions, so a fixture's premise cannot rot.
func controlledCorpus(t *testing.T, dupCos, relCos float64, relTiltInto int, relTiltCos float64) (CalibrationCorpus, *FixedVectorEmbedder) {
	t.Helper()
	corpus := CalibrationCorpus{
		Name:  "controlled",
		Bases: []string{"base-0", "base-1", "base-2"},
	}
	emb := NewFixedVectorEmbedder(calibDim)
	reg := func(text string, vec []float32) {
		if err := emb.Register(text, vec); err != nil {
			t.Fatalf("register %q: %v", text, err)
		}
	}
	for i, b := range corpus.Bases {
		reg(b, UnitAxis(calibDim, i))
	}
	for i := range corpus.Bases {
		dup := "dup-" + corpus.Bases[i]
		// Spare axis 10+i: orthogonal to every base, so this probe's
		// unrelated pairs are 0 and only the duplicate pair is non-zero.
		reg(dup, UnitTilt(calibDim, i, 10+i, dupCos))
		corpus.Duplicates = append(corpus.Duplicates, LabeledProbe{Text: dup, Base: i + 1})

		rel := "rel-" + corpus.Bases[i]
		into := 13 + i
		cos := relCos
		if relTiltInto >= 0 {
			// Tilt into a real base's axis so ONE unrelated pair carries
			// relTiltCos — the case where the unrelated class, not the
			// related class, is what a merge threshold has to clear.
			into = relTiltInto
			cos = relCos
			if i == relTiltInto {
				into = 13 + i
			}
		}
		v := UnitTilt(calibDim, i, into, cos)
		if relTiltInto >= 0 && i != relTiltInto {
			// UnitTilt already puts sqrt(1-cos^2) on `into`; assert the
			// caller's expectation instead of silently disagreeing.
			if got := float64(v[into]); math.Abs(got-relTiltCos) > 1e-6 {
				t.Fatalf("rel probe %d: unrelated component = %.6f, want %.6f (pick relCos so sqrt(1-cos^2) matches)", i, got, relTiltCos)
			}
		}
		reg(rel, v)
		corpus.Related = append(corpus.Related, LabeledProbe{Text: rel, Base: i + 1})
	}
	return corpus, emb
}

func classStats(t *testing.T, rep CalibrationReport, class string) ClassStats {
	t.Helper()
	for _, c := range rep.Classes {
		if c.Class == class {
			return c
		}
	}
	t.Fatalf("no stats for class %q", class)
	return ClassStats{}
}

// TestCalibrate_RecommendsAThresholdInsideTheSafeWindow — the recommendation's
// whole job. Duplicates at 0.90, related at 0.60, unrelated at 0: any merge
// threshold in (0.60, 0.90] merges every duplicate and nothing else, and the
// recommendation must land there rather than merely near it.
func TestCalibrate_RecommendsAThresholdInsideTheSafeWindow(t *testing.T) {
	corpus, emb := controlledCorpus(t, 0.90, 0.60, -1, 0)
	rep, err := Calibrate(context.Background(), corpus, emb, ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if !rep.Separable {
		t.Fatalf("Separable = false, want true (dup 0.90 vs non-dup 0.60)")
	}
	if rep.RecommendedMerge <= rep.SafeWindowLow || rep.RecommendedMerge > rep.SafeWindowHigh {
		t.Errorf("RecommendedMerge = %v, want in (%v, %v]", rep.RecommendedMerge, rep.SafeWindowLow, rep.SafeWindowHigh)
	}
	// The classes are exactly where the fixture put them.
	if got := classStats(t, rep, ClassDuplicate); got.N != 3 || got.Min != 0.9 || got.Max != 0.9 {
		t.Errorf("duplicate stats = %+v, want n=3 min=max=0.9", got)
	}
	if got := classStats(t, rep, ClassRelated); got.N != 3 || got.Min != 0.6 {
		t.Errorf("related stats = %+v, want n=3 min=0.6", got)
	}
	// 6 probes x 2 non-own bases: derived, never listed.
	if got := classStats(t, rep, ClassUnrelated); got.N != 12 {
		t.Errorf("unrelated n = %d, want 12 (6 probes x 2 non-own bases)", got.N)
	}
	// The configured 0.95 merges none of three duplicates — inert, which is
	// exactly the state this command exists to surface.
	if !rep.Configured.MergeInert || rep.Configured.MergeDestructive {
		t.Errorf("Configured = %+v, want merge_inert with no false merges", rep.Configured)
	}
	if rep.Embedder.Provider != "eval" || rep.Embedder.Dimension != calibDim {
		t.Errorf("Embedder = %+v, want the stub attributed with its dimension", rep.Embedder)
	}
}

// TestCalibrate_OverlappingClassesAreReportedNotSeparable — when a related
// probe outscores a duplicate, NO threshold separates them. The report must say
// so rather than emit a recommendation that looks authoritative, because the
// caller turns this into a non-zero exit.
func TestCalibrate_OverlappingClassesAreReportedNotSeparable(t *testing.T) {
	// Related at 0.95 sits ABOVE the duplicates at 0.90.
	corpus, emb := controlledCorpus(t, 0.90, 0.95, -1, 0)
	rep, err := Calibrate(context.Background(), corpus, emb, ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if rep.Separable {
		t.Fatalf("Separable = true, want false (related 0.95 > duplicate 0.90)")
	}
	if rep.DuplicateVsRelatedGap >= 0 {
		t.Errorf("DuplicateVsRelatedGap = %v, want negative", rep.DuplicateVsRelatedGap)
	}
	// With no window, the recommendation is the conservative one: the lowest
	// threshold that makes zero false merges. It must not sit where it would
	// merge a related pair.
	if rep.RecommendedMerge <= rep.SafeWindowLow {
		t.Errorf("RecommendedMerge = %v, want above max(non-duplicate) %v even with no window",
			rep.RecommendedMerge, rep.SafeWindowLow)
	}
	for _, row := range rep.Sweep {
		if row.Threshold == rep.RecommendedMerge && row.FalseMerges != 0 {
			t.Errorf("recommended threshold %v makes %d false merges, want 0", row.Threshold, row.FalseMerges)
		}
	}
}

// TestCalibrate_SafeWindowClearsUnrelatedNotOnlyRelated — the sharp edge in
// judging separability. Here the RELATED class is well below the duplicates
// but one UNRELATED pair sits between them; a window computed from max(RELATED)
// alone would recommend a threshold that destroys a distinct fact.
func TestCalibrate_SafeWindowClearsUnrelatedNotOnlyRelated(t *testing.T) {
	// cos 0.6 on the own base leaves sqrt(1-0.36) = 0.8 on the tilt axis,
	// which is base 0's axis for probes 1 and 2 — an unrelated pair at 0.8,
	// above every related pair (0.6) and below every duplicate (0.9).
	corpus, emb := controlledCorpus(t, 0.90, 0.60, 0, 0.8)
	rep, err := Calibrate(context.Background(), corpus, emb, ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if got := classStats(t, rep, ClassUnrelated); math.Abs(got.Max-0.8) > 1e-4 {
		t.Fatalf("unrelated max = %v, want 0.8 (fixture premise)", got.Max)
	}
	if math.Abs(rep.SafeWindowLow-0.8) > 1e-4 {
		t.Errorf("SafeWindowLow = %v, want 0.8 — the window's floor is max(non-duplicate), not max(related)", rep.SafeWindowLow)
	}
	if rep.RecommendedMerge <= 0.8 {
		t.Errorf("RecommendedMerge = %v, want above the 0.8 unrelated pair", rep.RecommendedMerge)
	}

	// And the same edge for the VERDICT, not just the window: duplicates at
	// 0.70 with the related class far below at 0.60 but an unrelated pair at
	// 0.80 is NOT separable. Judged against max(related) alone it would read
	// as separable and the command would exit 0 on a model that cannot be
	// calibrated at all.
	corpus2, emb2 := controlledCorpus(t, 0.70, 0.60, 0, 0.8)
	rep2, err := Calibrate(context.Background(), corpus2, emb2, ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if rep2.Separable {
		t.Errorf("Separable = true, want false — an unrelated pair at 0.80 outscores every duplicate at 0.70")
	}
}

// TestAnalyze_SweepCountsEachClassAtEachThreshold — the sweep is what an
// operator reads to pick a number by hand, so its arithmetic is pinned
// directly rather than inferred from a recommendation.
func TestAnalyze_SweepCountsEachClassAtEachThreshold(t *testing.T) {
	pairs := []ScoredPair{
		{Class: ClassDuplicate, Score: 0.95},
		{Class: ClassDuplicate, Score: 0.85},
		{Class: ClassDuplicate, Score: 0.75},
		{Class: ClassRelated, Score: 0.65},
		{Class: ClassRelated, Score: 0.45},
		{Class: ClassUnrelated, Score: 0.55},
		{Class: ClassUnrelated, Score: 0.05},
	}
	rep := Analyze("sweep", EmbedderInfo{}, pairs, ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})

	want := map[float64]SweepRow{
		// threshold: dup merged / false merges / related caught / unrelated flagged
		0.50: {DuplicatesMerged: 3, FalseMerges: 2, RelatedCaught: 1, UnrelatedFlagged: 1},
		0.70: {DuplicatesMerged: 3, FalseMerges: 0, RelatedCaught: 0, UnrelatedFlagged: 0},
		0.80: {DuplicatesMerged: 2, FalseMerges: 0, RelatedCaught: 0, UnrelatedFlagged: 0},
		0.95: {DuplicatesMerged: 1, FalseMerges: 0, RelatedCaught: 0, UnrelatedFlagged: 0},
	}
	got := map[float64]SweepRow{}
	for _, r := range rep.Sweep {
		got[r.Threshold] = r
	}
	for th, w := range want {
		g, ok := got[th]
		if !ok {
			t.Errorf("sweep has no row at %v", th)
			continue
		}
		if g.DuplicatesMerged != w.DuplicatesMerged || g.FalseMerges != w.FalseMerges ||
			g.RelatedCaught != w.RelatedCaught || g.UnrelatedFlagged != w.UnrelatedFlagged {
			t.Errorf("sweep[%v] = %+v, want %+v", th, g, w)
		}
	}
	// The recommended and configured thresholds are always on the table —
	// an operator must never have to interpolate the row that matters.
	for _, th := range []float64{rep.RecommendedMerge, rep.RecommendedRelated, 0.95, 0.85} {
		if _, ok := got[th]; !ok {
			t.Errorf("sweep is missing the row at %v", th)
		}
	}
}

// TestAnalyze_MeasuredEmbeddingGemmaReproducesThePublishedTable — pins the
// documented measurement into CI. Every published number for the 768-dim
// `embeddinggemma` run is asserted here, including the headline finding: the
// SHIPPED DEFAULT MERGES NOTHING on the model loomcycle's own docs recommend
// for self-hosting.
func TestAnalyze_MeasuredEmbeddingGemmaReproducesThePublishedTable(t *testing.T) {
	rep := Analyze("measured", MeasuredEmbeddingGemmaInfo(), MeasuredEmbeddingGemmaPairs(),
		ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})

	for _, w := range []ClassStats{
		{Class: ClassDuplicate, N: 12, Min: 0.7181, P05: 0.7909, Median: 0.9035, P95: 0.9376, Max: 0.9487},
		{Class: ClassRelated, N: 12, Min: 0.3722, P05: 0.3884, Median: 0.5205, P95: 0.6129, Max: 0.6775},
		{Class: ClassUnrelated, N: 72, Min: 0.1337, P05: 0.1460, Median: 0.2953, P95: 0.5018, Max: 0.5858},
	} {
		if got := classStats(t, rep, w.Class); got != w {
			t.Errorf("%s stats = %+v, want %+v", w.Class, got, w)
		}
	}

	if math.Abs(rep.DuplicateVsRelatedGap-0.0406) > 1e-4 {
		t.Errorf("DuplicateVsRelatedGap = %v, want +0.0406", rep.DuplicateVsRelatedGap)
	}
	if math.Abs(rep.RelatedVsUnrelatedGap-(-0.2136)) > 1e-4 {
		t.Errorf("RelatedVsUnrelatedGap = %v, want -0.2136", rep.RelatedVsUnrelatedGap)
	}
	if !rep.RelatedUnrelatedOverlap {
		t.Error("RelatedUnrelatedOverlap = false; the measured related and unrelated classes DO overlap")
	}

	// THE ASSERTION THIS FIXTURE EXISTS FOR: the recommendation lands inside
	// the measured safe window (0.6775, 0.7181].
	if rep.RecommendedMerge <= 0.6775 || rep.RecommendedMerge > 0.7181 {
		t.Errorf("RecommendedMerge = %v, want in (0.6775, 0.7181]", rep.RecommendedMerge)
	}
	if !rep.Separable {
		t.Error("Separable = false; duplicate and related DO separate on this model")
	}

	// The published sweep: 0.68-0.70 merges all 12 duplicates with zero false
	// merges out of the 84 non-duplicate pairs.
	rows := map[float64]SweepRow{}
	for _, r := range rep.Sweep {
		rows[r.Threshold] = r
	}
	if r, ok := rows[0.70]; !ok || r.DuplicatesMerged != 12 || r.FalseMerges != 0 {
		t.Errorf("sweep[0.70] = %+v (present=%v), want 12 duplicates merged and 0 false merges", r, ok)
	}
	if r, ok := rows[0.45]; !ok || r.RelatedCaught != 9 || r.UnrelatedFlagged != 8 {
		t.Errorf("sweep[0.45] = %+v (present=%v), want 9/12 related and 8/72 unrelated", r, ok)
	}
	if r, ok := rows[0.50]; !ok || r.RelatedCaught != 7 || r.UnrelatedFlagged != 4 {
		t.Errorf("sweep[0.50] = %+v (present=%v), want 7/12 related and 4/72 unrelated", r, ok)
	}

	// The headline: 0.95 merges 0 of 12, and 0.85 catches 0 of 12 related.
	if !rep.Configured.MergeInert || rep.Configured.DuplicatesMerged != 0 {
		t.Errorf("Configured = %+v, want merge_inert with 0 of 12 duplicates merged", rep.Configured)
	}
	if !rep.Configured.RelatedInert {
		t.Errorf("Configured = %+v, want related_inert (max related 0.6775 < 0.85)", rep.Configured)
	}
	if rep.Configured.MergeDestructive {
		t.Errorf("Configured = %+v, want no false merges — 0.95 fails SAFE, which is why the default stands", rep.Configured)
	}
}

// TestBuiltinCalibrationCorpus_IsUsableWithNoSetup — the bundled corpus must
// parse, validate, and carry the labelled pairs the reference run measured, or
// `memory-calibrate` with no flags does nothing useful.
func TestBuiltinCalibrationCorpus_IsUsableWithNoSetup(t *testing.T) {
	c, err := BuiltinCalibrationCorpus()
	if err != nil {
		t.Fatalf("BuiltinCalibrationCorpus: %v", err)
	}
	if len(c.Bases) != 12 || len(c.Duplicates) != 12 || len(c.Related) != 12 {
		t.Fatalf("corpus = %d bases / %d duplicates / %d related, want 12 each",
			len(c.Bases), len(c.Duplicates), len(c.Related))
	}
	// Each base must be covered exactly once by each probe class, or a
	// "duplicate" and its base are not actually a pair.
	dupSeen, relSeen := map[int]int{}, map[int]int{}
	for _, p := range c.Duplicates {
		dupSeen[p.Base]++
	}
	for _, p := range c.Related {
		relSeen[p.Base]++
	}
	for i := 1; i <= len(c.Bases); i++ {
		if dupSeen[i] != 1 || relSeen[i] != 1 {
			t.Errorf("base %d has %d duplicate / %d related probes, want 1 each", i, dupSeen[i], relSeen[i])
		}
	}
}

// TestLoadCalibrationCorpus_RejectsAnUnusableCorpus — a mislabelled corpus is
// indistinguishable from a bad model in the output, so the structural errors
// that CAN be caught are caught at load.
func TestLoadCalibrationCorpus_RejectsAnUnusableCorpus(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"one base", `{"bases":["a"],"duplicates":[{"text":"x","base":1}],"related":[{"text":"y","base":1}]}`, "at least 2 base"},
		{"no duplicates", `{"bases":["a","b"],"related":[{"text":"y","base":1}]}`, "duplicate probe"},
		{"no related", `{"bases":["a","b"],"duplicates":[{"text":"x","base":1}]}`, "related probe"},
		{"base out of range", `{"bases":["a","b"],"duplicates":[{"text":"x","base":5}],"related":[{"text":"y","base":1}]}`, "out of range"},
		{"empty probe", `{"bases":["a","b"],"duplicates":[{"text":"  ","base":1}],"related":[{"text":"y","base":1}]}`, "empty text"},
		{"not json", `nope`, "parse corpus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadCalibrationCorpus(strings.NewReader(tc.json))
			if err == nil {
				t.Fatalf("LoadCalibrationCorpus accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
