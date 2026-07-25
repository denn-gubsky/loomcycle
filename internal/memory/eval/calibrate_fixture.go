package eval

// The reference calibration measurement, as data.
//
// PROVENANCE. Measured 2026-07-25 against a live deployment's embedder —
// `embeddinggemma:latest` served by `ollama-local`, 768 dimensions — using the
// bundled 12-fact corpus and its 24 labelled probes, reading raw cosine out of
// `Memory op=search` (whose `score` is the raw cosine: an exact self-match
// returns 1.0). It is kept as a fixture, not as prose in a doc, so the
// published table is CI-enforced: if the analysis ever stops producing those
// numbers from those scores, a test fails instead of a doc quietly going stale.
//
// WHY THE VALUES ARE RECONSTRUCTED. What was recorded is each class's
// five-number summary, not its 96 individual cosines. These slices are the
// reconstruction: every one reproduces its class's published min / p05 / median
// / p95 / max EXACTLY under the analysis's percentile rule, and the middle of
// each distribution is a linear ramp because the middle was not measured. That
// is enough to pin everything the recommendation depends on — which is driven
// by max(RELATED) and min(DUPLICATE), both measured directly — and enough to
// reproduce the published sweep counts at 0.45 / 0.50 / 0.68 / 0.70. It is NOT
// enough to claim the shape of the interior, and nothing here should be used as
// if it were.
//
// WHY UNRELATED IS n=72 HERE AND LARGER FROM THE COMMAND. The hand measurement
// read each probe's neighbours out of a top-K search, so it saw 3 non-own bases
// per probe (24 x 3 = 72). Calibrate scores the FULL pairwise matrix — every
// probe against every base — which for the same corpus is 24 x 11 = 264
// unrelated pairs. The extra pairs are the easy negatives a top-K read never
// returns, so a full-matrix run reports a lower unrelated median and a max that
// can only be greater than or equal to this one. That direction is deliberate:
// max(UNRELATED) is what a merge threshold has to clear.

// measuredDuplicateScores etc. are the per-class cosines. Sorted ascending for
// readability; the analysis sorts its own copy, so order is not load-bearing.
var (
	measuredDuplicateScores = []float64{
		0.7181, 0.8505, 0.8600, 0.8750, 0.8900, 0.9010,
		0.9060, 0.9100, 0.9180, 0.9240, 0.9285, 0.9487,
	}
	measuredRelatedScores = []float64{
		0.3722, 0.40165, 0.4300, 0.4600, 0.4900, 0.5180,
		0.5230, 0.5300, 0.5400, 0.5500, 0.56005, 0.6775,
	}
)

// measuredUnrelatedScores builds the 72-sample unrelated class: the five
// measured anchor points, with ramps standing in for the unmeasured middle.
func measuredUnrelatedScores() []float64 {
	out := []float64{0.1337, 0.1400, 0.1420, 0.1440, 0.1476} // p05 falls between the last two
	out = append(out, ramp(0.1500, 0.2900, 30)...)
	out = append(out, 0.2940, 0.2966) // the two samples the median interpolates
	out = append(out, ramp(0.3000, 0.4400, 27)...)
	out = append(out, 0.4550, 0.4650, 0.4750, 0.4900)
	out = append(out, 0.5162, 0.5400, 0.5600, 0.5858) // p95 falls between 0.4900 and 0.5162
	return out
}

// ramp returns n evenly spaced values from lo to hi inclusive.
func ramp(lo, hi float64, n int) []float64 {
	out := make([]float64, n)
	if n == 1 {
		out[0] = lo
		return out
	}
	step := (hi - lo) / float64(n-1)
	for i := range out {
		out[i] = lo + step*float64(i)
	}
	return out
}

// MeasuredEmbeddingGemmaPairs returns the reference measurement as labelled
// pairs, ready for Analyze. Exported so the CLI's report-rendering test can
// print the real numbers without a live embedder — `denn-desktop` is not always
// reachable, and a doc table that only reproduces on one machine is not
// reproducible.
func MeasuredEmbeddingGemmaPairs() []ScoredPair {
	var out []ScoredPair
	add := func(class string, scores []float64) {
		for _, s := range scores {
			out = append(out, ScoredPair{Class: class, Score: s})
		}
	}
	add(ClassDuplicate, measuredDuplicateScores)
	add(ClassRelated, measuredRelatedScores)
	add(ClassUnrelated, measuredUnrelatedScores())
	return out
}

// MeasuredEmbeddingGemmaInfo is the model the reference numbers belong to. A
// calibration result without its model is not a result.
func MeasuredEmbeddingGemmaInfo() EmbedderInfo {
	return EmbedderInfo{Provider: "ollama-local", Model: "embeddinggemma:latest", Dimension: 768}
}
