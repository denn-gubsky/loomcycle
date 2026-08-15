package builtin

// document_verbatim.go — answering a lookup question with a stored claim and its
// citation, with no model in the answer path at all.
//
// THE POINT IS THE REMOVAL, not the retrieval. A conversational agent asked "what is my
// GitHub username" reads the fact and writes a sentence, and that sentence is generated
// text: it can drift, hedge, or embellish. Returning the stored claim verbatim alongside
// the span it was verified against removes runtime fabrication by construction rather
// than by instruction. That only works for lookup-shaped questions — "how should I
// structure this migration" has no verbatim answer — so this is an opportunistic fast
// path and never the whole answer path.
//
// THE ERROR DIRECTION IS INVERTED FROM THE JUDGE'S, and getting that backwards would
// undo the line. For the judge, the dangerous failure is the false REFUSAL: a withheld
// true fact is invisible. Here the dangerous failure is the confident WRONG answer,
// because verbatim delivery reads as authority — a synthesised answer invites doubt in a
// way a quoted one does not. So every ambiguity resolves to NO ANSWER:
//
//   - the best-matching fact is not verified        → no answer
//   - it carries no span                            → no answer (nothing to cite)
//   - it does not clear the score floor             → no answer
//   - a second fact is within the margin            → no answer
//
// Silence is always available and always safe: the caller falls back to ordinary recall,
// which is what it would have done anyway.
//
// WHY IT IS A DEDICATED OP rather than a field on recall. The RFC sketched this as
// something recall reports alongside its results. It cannot be: `Memory.recall` reads the
// k/v tier and verification state lives only on the entity tier, so that shape means a
// cross-tier join on EVERY recall to serve a path most callers will not use. A dedicated
// op puts the cost exactly where the value is, and matches the RFC's own "the consumer
// decides".

import (
	"context"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// verbatimMinConfidence is what a fact must carry to be quoted as authority.
	//
	// Set at `supported` exactly, so ONLY a fact a judge affirmed outright can be
	// returned. `mistyped` is deliberately excluded even though its claim is supported:
	// the measurement that shipped with the judge found mistyping unreliable on local
	// models, and a path whose whole premise is authority is the wrong place to spend
	// that uncertainty.
	verbatimMinConfidence = confidenceSupported

	// verbatimDefaultMinScore is the similarity a match must reach before it can be an
	// answer at all.
	//
	// ⚠️ A SHIPPED CONSTANT HERE IS RIGHT FOR AT MOST ONE EMBEDDING MODEL. Cosine scale
	// is a property of the model, not of the data — the same pair measured 0.7675 on one
	// embedder and 0.9005 on another, which is why the consolidation bands are
	// calibrated per deployment rather than shipped. This is not calibrated, so it is
	// (a) deliberately conservative, (b) overridable per call, and (c) always reported
	// back with the actual scores so an operator can see what their embedder does before
	// trusting it. A silent default in both directions is exactly the failure the
	// calibration tool exists for.
	verbatimDefaultMinScore = 0.60

	// verbatimMinMargin is how far the winner must clear the runner-up.
	//
	// Two facts that both plausibly answer a question are not an answer; they are a
	// question about which one is meant. Quoting either as authority would be the
	// confident-wrong failure this op is shaped against.
	verbatimMinMargin = 0.05

	// verbatimSearchTopK is how deep to look. Small on purpose: this path is only ever
	// interested in whether there is ONE clear winner, and a deeper scan cannot change
	// that answer — it can only find more runners-up.
	verbatimSearchTopK = 8
)

// verbatimAnswer returns the stored claim that answers a lookup question, or nothing.
//
// The response ALWAYS explains itself. A caller that gets no answer needs to know whether
// the store has nothing, has something unverified, or has two things — those lead to
// three different actions (write it down, run the judge, ask a sharper question), and a
// bare empty result leads to none of them.
func (d *Document) verbatimAnswer(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return errResult("verbatim_answer: missing required field: query (the lookup question to answer)"), nil
	}
	if d.Embedder == nil {
		return errResult("verbatim_answer: requires a configured embedder / vector memory"), nil
	}
	minScore := verbatimDefaultMinScore
	if in.MinScore > 0 {
		minScore = in.MinScore
	}

	vec, err := d.Embedder.Embed(ctx, []string{q})
	if err != nil {
		return errResult("verbatim_answer: embed: " + err.Error()), nil
	}
	if len(vec) == 0 {
		return errResult("verbatim_answer: embed: embedder returned no vector"), nil
	}
	entries, err := d.Store.MemoryEmbedSearch(ctx, direntTenant(ctx), mscope, key.ScopeID,
		store.MemorySearchFilter{KeyPrefix: chunkBodyKeyPrefix}, vec[0], verbatimSearchTopK)
	if err != nil {
		return errResult("verbatim_answer: " + err.Error()), nil
	}

	// FACTS ONLY, in score order. A document chunk is prose, not a claim: it has no
	// span to cite and nothing asserted it, so it can neither be an answer nor block
	// one. Letting document prose out-rank a fact would make this useless in any store
	// that also holds documents, which is every store.
	type candidate struct {
		id    string
		score float64
		meta  chunkMetaRow
	}
	var facts []candidate
	for _, e := range entries {
		cid := ChunkIDFromBodyKey(e.Key)
		if cid == "" {
			continue
		}
		meta, found, merr := d.readChunkMeta(ctx, key, cid)
		if merr != nil {
			return errResult("verbatim_answer: " + merr.Error()), nil
		}
		if !found {
			continue // not a fact
		}
		facts = append(facts, candidate{id: cid, score: e.Score, meta: meta})
	}
	if len(facts) == 0 {
		return jsonResult(map[string]any{
			"answered": false,
			"reason":   "no stored fact matches the question",
		})
	}

	// THE TOP-RANKED FACT MUST ITSELF BE THE VERIFIED ONE. Skipping past an unverified
	// better match to quote a verified worse one would answer with something we know is
	// not the closest thing we have — verified, and wrong.
	best := facts[0]
	cb, berr := d.readBody(ctx, mscope, key.ScopeID, best.id)
	if berr != nil {
		return errResult("verbatim_answer: " + berr.Error()), nil
	}
	body := cb.Body
	out := map[string]any{
		"answered": false,
		"chunk_id": best.id,
		"score":    best.score,
	}
	switch {
	case best.score < minScore:
		out["reason"] = "the closest stored fact is not close enough to be quoted as an answer"
		out["min_score"] = minScore
		return jsonResult(out)
	case best.meta.JudgedAt == nil:
		// The distinction the whole RFC rests on, surfaced here because it is
		// actionable: this fact may well be true, it has simply never been checked.
		out["reason"] = "the closest stored fact has not been verified, and an unverified " +
			"claim quoted verbatim reads as authority it has not earned"
		return jsonResult(out)
	case best.meta.Confidence == nil || *best.meta.Confidence < verbatimMinConfidence:
		out["reason"] = "the closest stored fact was checked and not affirmed outright"
		return jsonResult(out)
	case strings.TrimSpace(best.meta.SourceQuote) == "":
		// Belt and braces: judge_fact refuses a verdict without a span, so this should
		// be unreachable. It is checked anyway because the failure mode is quoting an
		// answer with no citation, and the cost of the check is one comparison.
		out["reason"] = "the closest stored fact carries no source span, so there is nothing to cite"
		return jsonResult(out)
	}
	if len(facts) > 1 && best.score-facts[1].score < verbatimMinMargin {
		out["reason"] = "two stored facts match this question about equally well, so there is " +
			"no single answer to quote"
		out["runner_up"] = map[string]any{"chunk_id": facts[1].id, "score": facts[1].score}
		return jsonResult(out)
	}

	return jsonResult(map[string]any{
		"answered":   true,
		"answer":     body,
		"source":     best.meta.SourceQuote,
		"chunk_id":   best.id,
		"score":      best.score,
		"confidence": *best.meta.Confidence,
		"judged_at":  *best.meta.JudgedAt,
	})
}

// verificationStats reports how much of the scope's fact store is actually verified.
//
// It exists because the phase that produced this op is gated on "verified facts are the
// norm rather than the exception", and nothing could report that. A gate nobody can read
// is a gate nobody applies — the decision gets made on impression instead, which is the
// habit this whole line was built to replace.
//
// WHOLE-STORE, unlike the consolidation pass's per-run figure, which is bounded by its
// scan window and says so. This is one aggregate query and is the number to quote.
func (d *Document) verificationStats(ctx context.Context, key sqlmem.ScopeKey) (tools.Result, error) {
	res, err := d.query(ctx, key, `
		SELECT count(*),
		       sum(CASE WHEN source_quote IS NOT NULL AND source_quote <> '' THEN 1 ELSE 0 END),
		       sum(CASE WHEN judged_at IS NOT NULL THEN 1 ELSE 0 END),
		       sum(CASE WHEN judged_at IS NOT NULL AND confidence >= `+
		sqlFloat(verbatimMinConfidence)+` THEN 1 ELSE 0 END),
		       sum(CASE WHEN confidence IS NOT NULL AND confidence < `+
		sqlFloat(withholdBelowConfidence)+` THEN 1 ELSE 0 END)
		  FROM chunk_memory_meta`)
	if err != nil {
		return errResult("verification_stats: " + err.Error()), nil
	}
	if len(res.Rows) == 0 {
		return jsonResult(map[string]any{"facts": 0})
	}
	r := res.Rows[0]
	facts := asInt(r[0])
	withSpan, judged, supported, withheld := asInt(r[1]), asInt(r[2]), asInt(r[3]), asInt(r[4])
	out := map[string]any{
		"facts":     facts,
		"with_span": withSpan,
		"judged":    judged,
		"supported": supported,
		"withheld":  withheld,
		// Named for what it is rather than "unverified": a fact with no span cannot be
		// verified by anyone, ever, so it is a different population from one merely
		// awaiting a judge. Reported separately for the same reason the sweep counts
		// them separately.
		"unverifiable_no_span": facts - withSpan,
		"awaiting_judge":       withSpan - judged,
	}
	if facts > 0 {
		// The gate figure itself, computed here so two callers cannot derive it
		// differently. Verified means judged AND affirmed — not merely looked at.
		out["verified_share"] = float64(supported) / float64(facts)
	}
	return jsonResult(out)
}

// sqlFloat renders a threshold constant into SQL. Rendered FROM the constant, never
// written out beside it, for the reason the withholding clause gives: a literal is a
// second definition, and the two eventually disagree about which facts count.
func sqlFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
