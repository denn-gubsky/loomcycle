package main

// oracle.go — the READING CEILING arm (research §7b L1).
//
// WHAT IT MEASURES, AND WHY IT COMES FIRST. Every other lever in the local-reader
// plan is a RETRIEVAL lever: get the right turns in front of the model. None of them
// can beat the number the model scores when the right turns are ALREADY in front of
// it. That number is this arm, and it is unmeasured — so the estimates for L2 and L3
// are extrapolations from a ceiling nobody has seen.
//
// Shape: RFC DB's DB-1 applied to LoCoMo. Each question's gold evidence turns
// (`qa.evidence` dia_ids, which the loader has already resolved into Query.Expected)
// are rendered into the prompt. No store, no tools, no retrieval — the reader only
// reads. Same 150 questions, same judge, so the number is directly comparable to
// every store arm on the same host.
//
// ⚠️ THE DEEPSEEK RUN IS AN INSTRUMENT CHECK, NOT A DATA POINT. Its oracle must land
// AT OR ABOVE its traces arm (0.7877): handed the gold turns directly it cannot do
// worse than when it had to find them. If it lands below, the evidence rendering is
// wrong and every local oracle number measures this file rather than the reader.
//
// ⚠️ ABSTENTION IS THE OTHER HALF OF THE READING. An oracle arm that still abstains
// with the gold evidence in the prompt is a reader that will not COMMIT, which no
// retrieval work fixes. That is why the report's freed-abstention quality matters
// here more than anywhere else (§7b L7).
//
// ⚠️ LOCOMO'S `evidence` ANNOTATION IS OFF BY ONE ON A LARGE MINORITY OF QUESTIONS,
// and that is why this file renders a NEIGHBOUR WINDOW rather than the annotated
// turns alone. Measured on conv-26: the annotation for "what are the new shoes used
// for" (gold: running) is Caroline ASKING "for walking or running?" — the answer is
// Melanie's next turn, which the annotation does not name. The first instrument
// check rendered the annotation verbatim and deepseek scored 0.6747, BELOW its own
// traces arm at 0.7877, abstaining on 33 of 150 with the "gold" evidence in front of
// it. It was reading faithfully; the annotation did not contain the answer. A
// lexical check over all 150 confirms it: the gold tokens are fully present in 39 of
// 70 single-hop annotations at width 0 and 48 at width 2.
//
// So a width-0 oracle measures LOCOMO'S ANNOTATION, not the reader's ceiling. The
// window restores the adjacency pair the annotation points into. It is a documented
// DEVIATION from §7b's literal "the gold evidence turns": the spec assumed the
// annotation named the answer.

import (
	"fmt"
	"sort"
	"strings"
)

// oracleEvidenceHeader introduces the block. Deliberately plain: the coerced-prompt
// arm collapsed a small model by adding procedure, so this adds none — it labels the
// evidence and says nothing about how to use it.
const oracleEvidenceHeader = "Conversation turns:"

// OracleCoverage is what the attach could and could not render. Both counters are
// reported, because they fail differently and only one of them used to be visible.
type OracleCoverage struct {
	Unresolved int // no annotated id resolved: the question is answered from nothing
	Partial    int // SOME ids resolved: the reader is handed less evidence than the key claims
	Turns      int // turns rendered in total, the cost of the window
	MaxChars   int // largest single evidence block, against the reader's context
}

// AttachOracleEvidence fills each query's Evidence with its gold supporting turns
// AND their neighbours within `window` positions in the same session, rendered
// exactly as they are stored and embedded elsewhere in this harness (Turn.Body —
// "[date] Speaker: text"), so the oracle reads the same surface a retrieval arm
// would have handed it.
//
// window=0 renders the annotation verbatim and is kept so the annotation's own
// ceiling stays measurable; see the file header for why it is not the default.
//
// ⚠️ PARTIAL RESOLUTION IS COUNTED SEPARATELY. Counting only the all-ids-missing
// case reports a question whose 3 ids resolved to 1 as fine while the reader holds a
// third of the key — the same silent-truncation shape as a seed list cut by a limit.
// It is 0 on conv-26 today; it is counted so it cannot become non-zero in silence.
func AttachOracleEvidence(conv Conversation, qs []Query, window int) ([]Query, OracleCoverage) {
	if window < 0 {
		window = 0
	}
	// Turn order within a session is the adjacency the window walks; DiaID alone
	// cannot give it, so index positionally.
	type place struct{ session, idx int }
	at := make(map[string]place, len(conv.Turns))
	bySession := make(map[int][]Turn)
	for _, t := range conv.Turns {
		if t.DiaID == "" {
			continue
		}
		at[t.DiaID] = place{t.Session, len(bySession[t.Session])}
		bySession[t.Session] = append(bySession[t.Session], t)
	}

	var cov OracleCoverage
	out := make([]Query, len(qs))
	for i, q := range qs {
		out[i] = q
		// Dedupe: adjacent annotations overlap, and a turn rendered twice reads to
		// the model as two separate statements of the same thing.
		seen := make(map[string]bool)
		var picked []Turn
		found := 0
		for _, id := range q.Expected {
			p, ok := at[id]
			if !ok {
				continue
			}
			found++
			turns := bySession[p.session]
			lo, hi := p.idx-window, p.idx+window
			if lo < 0 {
				lo = 0
			}
			if hi >= len(turns) {
				hi = len(turns) - 1
			}
			for j := lo; j <= hi; j++ {
				if !seen[turns[j].DiaID] {
					seen[turns[j].DiaID] = true
					picked = append(picked, turns[j])
				}
			}
		}
		if found == 0 {
			cov.Unresolved++
			continue
		}
		if found < len(q.Expected) {
			cov.Partial++
		}
		// Chronological, so a "[date] Speaker:" sequence reads as the conversation
		// it was rather than as the annotation's arbitrary order.
		sort.SliceStable(picked, func(a, b int) bool {
			pa, pb := at[picked[a].DiaID], at[picked[b].DiaID]
			if pa.session != pb.session {
				return pa.session < pb.session
			}
			return pa.idx < pb.idx
		})
		var b strings.Builder
		for _, t := range picked {
			b.WriteString(t.Body())
			b.WriteString("\n")
		}
		cov.Turns += len(picked)
		body := oracleEvidenceHeader + "\n" + b.String()
		if len(body) > cov.MaxChars {
			cov.MaxChars = len(body)
		}
		out[i].Evidence = body
	}
	return out, cov
}

// oraclePrompt puts the evidence BEFORE the question.
//
// Order is not cosmetic: the question last is what a reader sees as the instruction,
// and burying it above several turns of transcript is how a small model ends up
// answering about the wrong entity — the failure the coerced arm showed when the
// prompt grew a procedure.
func oraclePrompt(q Query) string {
	if q.Evidence == "" {
		return q.Question
	}
	return q.Evidence + "\n" + fmt.Sprintf("Question: %s", q.Question)
}
