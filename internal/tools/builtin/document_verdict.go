package builtin

// document_verdict.go — recording that a fact was checked against its span, and
// withholding the ones that failed (RFC CC phase 3a).
//
// CONFIDENCE IS THE AXIS. One number for how much a claim is trusted, not a second status
// competing with it — two axes that can disagree is a bug generator. So a verdict does not
// add a flag; it SETS the number, and `judged_at` / `judge_reason` record the provenance
// of that number rather than duplicating it.
//
// WHICH SURFACES WITHHOLD, and this is a scope decision rather than an oversight. A fact
// lives in two tiers: the k/v tier is authoritative and is what Memory.recall reads, while
// the entity tier is the graph mirror and is the only place verification state lives.
// Nothing in the recall path reads the entity tier, so making recall respect a verdict
// would mean either duplicating it onto the k/v row — a second copy of one truth, behind a
// store migration — or joining across the tiers on every recall.
//
// Neither is worth it, so the line is drawn where the state already is: the FACT surfaces
// withhold, and Memory.recall does not. The k/v tier records what was said; the entity
// tier is the curated view of what is believed. A refuted claim stays retrievable as raw
// material and stops being an asserted fact, which is nearer to "quarantine, never delete"
// than removing it from recall would be.
//
// The cost, stated so nobody discovers it: a plain Memory.recall still returns a refuted
// claim's text.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Verdict → confidence. The mapping lives HERE and the op takes a word, so a caller
// cannot write an arbitrary number and no two callers can disagree about what
// "unsupported" is worth.
//
// `unclear` and `mistyped` deliberately land ABOVE the withholding floor. A judge that is
// unsure is not evidence against a claim, and the failure this whole line guards against is
// the false refusal — a fact withheld on a maybe is a fact nobody will notice is gone.
const (
	confidenceSupported   = 0.9
	confidenceUnclear     = 0.5
	confidenceUnsupported = 0.0

	// confidenceMistyped is "the span supports this claim, but it is filed as something
	// the ontology does not say it is".
	//
	// A SEPARATE VERDICT because the fix is different. An unsupported fact should go
	// away; a mistyped one should be RETYPED, and a verdict that collapsed the two would
	// tell an operator to delete a true fact. `location:user` as the subject of a
	// sentence about living in Cluj-Napoca is the case that motivated it: loosely
	// entailed by its span, and wrong.
	//
	// It stays VISIBLE — reduced, not withheld. The claim is true, so withholding it
	// would be a false refusal in service of a filing error, and the number sits low
	// enough to sort behind clean facts and to be queried for.
	confidenceMistyped = 0.4

	// withholdBelowConfidence is the floor the fact surfaces apply.
	//
	// Set so ONLY an explicit `unsupported` falls below it. Raising it to catch `unclear`
	// would trade an invisible loss for a visible one in the wrong direction.
	withholdBelowConfidence = 0.25
)

// judgeFact records the outcome of checking a fact against its recorded span.
//
// A NARROW OP, like propose_entity: it sets the confidence from a verdict word, stamps
// when the judgement happened, and does nothing else. A judge cannot edit the claim, move
// it, retire it, or invent a timestamp — so "the judge got it wrong" is always recoverable
// by re-judging, and never by restoring content it overwrote.
func (d *Document) judgeFact(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	verdict := strings.ToLower(strings.TrimSpace(in.Verdict))
	var confidence float64
	switch verdict {
	case "supported":
		confidence = confidenceSupported
	case "unclear":
		confidence = confidenceUnclear
	case "unsupported":
		confidence = confidenceUnsupported
	case "mistyped":
		confidence = confidenceMistyped
	default:
		return errResult("judge_fact: verdict must be \"supported\", \"unclear\", \"mistyped\" or " +
			"\"unsupported\" — a number is not accepted, because the scale belongs to the " +
			"server and two callers using different scales would make the floor meaningless"), nil
	}
	if strings.TrimSpace(in.Reason) == "" {
		return errResult("judge_fact: reason is required — a verdict an operator cannot act on " +
			"is a verdict nobody trusts, and a withheld fact with no stated ground is indistinguishable " +
			"from a bug"), nil
	}

	chunkID := strings.TrimSpace(in.ID)
	if chunkID == "" {
		if nk := strings.TrimSpace(in.NaturalKey); nk != "" {
			id, err := d.chunkIDByNaturalKey(ctx, key, nk)
			if err != nil {
				return errResult("judge_fact: lookup: " + err.Error()), nil
			}
			chunkID = id
		}
	}
	if chunkID == "" {
		return errResult("judge_fact: name the fact by id or natural_key"), nil
	}
	prev, found, err := d.readChunkMeta(ctx, key, chunkID)
	if err != nil {
		return errResult("judge_fact: " + err.Error()), nil
	}
	if !found {
		// Only a fact can be judged. A plain chunk has no claim to check and no span to
		// check it against.
		return errResult("judge_fact: that chunk is not a fact (it carries no entity metadata)"), nil
	}
	// A fact with no span cannot be CHECKED against anything — but a person can still
	// vouch for it, and that is a different act with a different authority.
	//
	// THIS RULE USED TO REFUSE EVERYONE, and its stated reason was that such a verdict
	// "would be indistinguishable from one that was checked". That justification stopped
	// being true when `judged_by` landed: the store now records whether a machine or a
	// human reached a verdict, so an operator's evidence-free affirmation is no longer
	// mistakable for an entailment check. The refusal outlived its own argument, and in
	// the meantime it made a legitimate operator action impossible — confirming a fact
	// stored before spans existed, which is most of what an older store holds.
	//
	// AN AGENT IS STILL REFUSED. It cannot vouch: its "I checked this" is exactly the
	// claim the span exists to substantiate, and letting a run affirm what it cannot
	// check is the laundering this line is built against. An operator supplying no span
	// is not making that claim — they are putting their own name to it, which the reason
	// and `judged_by` record.
	// `mistyped` is excluded from that relaxation, by anyone. It is not a claim about
	// whether the fact is TRUE — it says the span carries the claim but the filing is
	// wrong — so without a span there is nothing for it to be about. An operator who
	// wants to correct a filing retypes the fact, which is an edit, not a verdict.
	judgedBy := judgedByForVerdict(ctx)
	mayVouch := judgedBy == "operator" && verdict != "mistyped"
	if prev.SourceQuote == "" && verdict != "unclear" && !mayVouch {
		return errResult("judge_fact: that fact records no source span, so there is nothing to " +
			"check it against. An operator may still vouch for it from the console — a run " +
			"may not, because affirming what you cannot check is the failure a span exists " +
			"to prevent. Leave it unjudged, which reads as unverified"), nil
	}

	now := time.Now().UnixNano()
	if err := d.exec(ctx, key,
		`UPDATE chunk_memory_meta SET confidence = ?, judged_at = ?, judge_reason = ?, judged_by = ? WHERE chunk_id = ?`,
		confidence, now, in.Reason, judgedBy, chunkID); err != nil {
		return errResult("judge_fact: " + err.Error()), nil
	}
	return jsonResult(map[string]any{
		"chunk_id":   chunkID,
		"verdict":    verdict,
		"confidence": confidence,
		"judged_at":  now,
		"withheld":   confidence < withholdBelowConfidence,
	})
}

// judgedByForVerdict records WHO reached a verdict, derived from the context rather than
// accepted from the caller.
//
// SERVER-STAMPED FOR THE REASON `origin` IS: a writer must not get to label its own
// provenance. An agent asked to record "an operator decided this" would be one prompt
// injection away from laundering a machine's verdict into a human's, and the whole value
// of the distinction is that an operator reading a refusal months later can tell which
// it was.
//
// The presence of a run id is the discriminator, exactly as originForEntityWrite uses it:
// no run means the call came from the off-run console, which only an authenticated
// operator can reach. An agent's verdict records the AGENT'S NAME rather than a generic
// marker — "memory/judge" and some other agent that happened to hold the Document tool
// are different claims, and the second is worth being able to see.
func judgedByForVerdict(ctx context.Context) string {
	if tools.RunID(ctx) == "" {
		return "operator"
	}
	if name := tools.AgentName(ctx); name != "" {
		return name
	}
	return "agent"
}

// withholdClause is the SQL the fact surfaces apply, plus whether it applies at all.
//
// FAILS OPEN ON NULL, which is the whole correctness of this feature. Nothing in the
// memory pipeline supplies a confidence today — the extractor's contract does not include
// one — so every existing fact has NULL, and a naive `confidence < floor` in a tier where
// NULL sorts low would withhold the entire store the moment this shipped.
//
// NULL means "never assessed" and stays visible. Only an explicit number below the floor
// is withheld, which is exactly the "not yet judged versus judged and refuted" distinction
// the design rests on.
func withholdClause(col string, includeRefuted bool) string {
	if includeRefuted {
		return ""
	}
	// Rendered FROM the constant. A literal here would be a second definition of the
	// floor, and the two would eventually disagree about which facts are visible.
	floor := strconv.FormatFloat(withholdBelowConfidence, 'f', -1, 64)
	return "(" + col + " IS NULL OR " + col + " >= " + floor + ")"
}
