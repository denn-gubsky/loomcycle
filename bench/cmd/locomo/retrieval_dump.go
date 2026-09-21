package main

// retrieval_dump.go — the QPP arm: dump the SHAPE of each question's retrieved set
// instead of answering it.
//
// WHY THIS EXISTS. The LongMemEval result (bench/results/locomo/2026-09-20-longmemeval/)
// is that attaching question-anchored turns lifts the answerable slice by +24.5pp and
// costs the `_abs` slice 26.7pp, because retrieval ALWAYS returns something: for a
// question whose answer is not in the history, the reader gets on-topic material and
// reads "relevant material present" as "the answer is here". A prompt cannot fix it —
// one added sentence restored `_abs` and collapsed the answerable slice — so the gate
// has to be a RUNTIME one, decided before the turns are handed over.
//
// The hypothesis under test: the score vector of a retrieved set says whether the set
// answers the question. Borrowed from prompt-injection detection, where a foreign
// insertion shows up as a break in token homogeneity. Stage 1 on LoCoMo
// (2026-09-21-qpp-probe) found the sign is right but the instrument saturated — its
// negatives were off-topic, so raw cosine separated them perfectly and nothing could be
// ranked against it. `_abs` is the real test precisely because those questions are
// ON-TOPIC by construction, which is what makes them hard and what stops top1 from
// saturating.
//
// This arm runs the SAME per-instance loop as the answer axis — purge, ingest, index —
// and then, instead of spawning an answerer and a judge, issues one trace search per
// question and records the score vector. No answering model, no judge.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// retrievalDumpRow is one question's retrieved set, as measured.
type retrievalDumpRow struct {
	SampleID string `json:"sample_id"`
	Question string `json:"question"`
	Category int    `json:"category"`
	// Abstain is the LABEL: true means the history does not contain the answer, so
	// the correct behaviour is to refuse. It is what the probe is trying to predict.
	Abstain bool `json:"abstain"`
	Found   int  `json:"found"`
	// Scores is the raw cosine per hit, Ranks the hybrid rank the rows were ordered
	// by. Both are kept because they are different signals: the first is affinity to
	// the query, the second is what the ranker actually committed to.
	Scores []float64 `json:"scores"`
	Ranks  []float64 `json:"ranks"`
	// The derived signals, computed here so a reader of the file does not have to
	// re-derive them and get a different answer.
	Top1    float64 `json:"top1"`
	MeanK   float64 `json:"mean_k"`
	StdK    float64 `json:"std_k"`
	NQC     float64 `json:"nqc"`
	Gap     float64 `json:"gap_top1_mean"`
	PplxRaw float64 `json:"pplx_raw"`
	PplxT05 float64 `json:"pplx_t05"`
	PplxT01 float64 `json:"pplx_t01"`
	// Texts is the retrieved material itself, capped per turn. Carried so a SECOND
	// decider — a model asked whether this material answers the question — can be run
	// off the dump instead of re-ingesting 130 instances to ask it. Empty unless
	// -retrieval-dump-texts, because the scores are what the shape analysis needs and
	// the bodies multiply the file by an order of magnitude.
	Texts []string `json:"texts,omitempty"`
	Error string   `json:"error,omitempty"`
}

// dumpTexts is set by -retrieval-dump-texts. A package-level flag value rather than a
// parameter threaded through dumpRetrieval's signature: the dump is one call site and
// the alternative is a signature nobody else needs.
var dumpTexts bool

// perplexity is exp(entropy) of the score vector read as a distribution — the
// "homogeneity" the hypothesis is about, in the units it is usually stated in: k for a
// flat vector (no winner), 1 for a single dominant hit.
//
// ⚠️ TEMPERATURE IS NOT OPTIONAL. Measured on LoCoMo: over RAW normalised cosines the
// metric is dead — 23.92 against 23.99 on a ceiling of 24 — because cosines occupy far
// too narrow a band for entropy to resolve. PplxRaw is kept only so that stays visible
// in the data rather than being a claim in a comment.
func perplexity(v []float64, temp float64) float64 {
	if len(v) == 0 {
		return 0
	}
	w := make([]float64, len(v))
	if temp > 0 {
		max := v[0]
		for _, x := range v {
			if x > max {
				max = x
			}
		}
		for i, x := range v {
			w[i] = math.Exp((x - max) / temp)
		}
	} else {
		for i, x := range v {
			w[i] = math.Max(x, 1e-12)
		}
	}
	sum := 0.0
	for _, x := range w {
		sum += x
	}
	if sum == 0 {
		return 0
	}
	h := 0.0
	for _, x := range w {
		p := x / sum
		if p > 0 {
			h -= p * math.Log(p)
		}
	}
	return math.Exp(h)
}

func meanStd(v []float64) (float64, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	m := sum / float64(len(v))
	sq := 0.0
	for _, x := range v {
		sq += (x - m) * (x - m)
	}
	return m, math.Sqrt(sq / float64(len(v)))
}

// dumpRetrieval measures one conversation's questions against the store as it stands.
// Best-effort per question: a failed search is recorded with its error rather than
// aborting the run, because one bad search is not a reason to throw away 129 good
// instances.
func dumpRetrieval(ctx context.Context, rest *Client, userID string, conv Conversation,
	topK int, out io.Writer, stdout io.Writer) (int, error) {
	enc := json.NewEncoder(out)
	measured := 0
	for _, q := range conv.Queries {
		if err := ctx.Err(); err != nil {
			return measured, err
		}
		row := retrievalDumpRow{
			SampleID: conv.SampleID, Question: q.Question,
			Category: q.Category, Abstain: q.Abstain,
		}
		res, err := rest.SearchScored(ctx, "user", userID, q.Question, topK, []string{"traces"})
		if err != nil {
			row.Error = err.Error()
		} else {
			for _, e := range res.Entries {
				row.Scores = append(row.Scores, e.Score)
				row.Ranks = append(row.Ranks, e.RankScore)
			}
			if dumpTexts {
				for _, e := range res.Entries {
					row.Texts = append(row.Texts, trimForDump(string(e.Value)))
				}
			}
			row.Found = len(row.Scores)
			if row.Found > 0 {
				sorted := append([]float64(nil), row.Scores...)
				sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
				row.Top1 = sorted[0]
				row.MeanK, row.StdK = meanStd(row.Scores)
				if row.MeanK != 0 {
					row.NQC = row.StdK / row.MeanK
				}
				row.Gap = row.Top1 - row.MeanK
				row.PplxRaw = perplexity(row.Scores, 0)
				row.PplxT05 = perplexity(row.Scores, 0.05)
				row.PplxT01 = perplexity(row.Scores, 0.01)
			}
		}
		if err := enc.Encode(row); err != nil {
			return measured, fmt.Errorf("write dump row: %w", err)
		}
		measured++
	}
	fmt.Fprintf(stdout, "  retrieval-dump: measured %d question(s)\n", measured)
	return measured, nil
}

// trimForDump bounds one turn in the dump. The verifier reads these, and a model asked
// to judge sufficiency over unbounded material judges the truncation instead.
func trimForDump(s string) string {
	const max = 1200
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// openRetrievalDump opens the dump file for append across instances, since the answer
// axis walks conversations one at a time and each contributes its own rows.
func openRetrievalDump(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
