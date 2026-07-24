// Package memory holds the retrieval-side logic that sits between the
// Memory tool and the store's vector search: the hybrid ranker (this
// file) and, in later slices, search-time dedup and the pluggable
// MemoryBackend interface. It imports only internal/store (no cycle).
package memory

import (
	"math"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RankConfig is the per-search hybrid-ranking weight block (RFC I
// Decision 1). The score of a candidate is:
//
//	score = semantic_weight · semantic_signal
//	      + recency_weight  · exp(-Δt·ln2 / recency_half_life_hours)
//	      + source_weight   · source_score
//	      + frequency_weight· log(1 + access_count)
//
// semantic_signal was raw cosine similarity before RFC BL hybrid retrieval.
// Post-hybrid, the in-process backend fuses the vector + full-text legs by
// Reciprocal Rank Fusion first (see FuseRRF) and carries the fused RRF value
// in each candidate's SemanticScore, so semantic_weight then scales that
// fused rank signal. The raw cosine stays in Score (the field the Memory tool
// renders) and is never overwritten. Backends that don't fuse (a remote
// backend, and the
// in-process pure-vector fast path) set SemanticScore to the raw cosine.
//
// The DEFAULT (semantic=1, all others=0) keeps ordering equal to the
// underlying retrieval order — operators who don't pass a `rank` block see
// no re-rank and no migration.
//
// frequency_weight (RFC BL): now WIRED — access_count rides on each search
// entry (PR1 added the column; the search legs populate it), so a
// frequently-recalled entry is rewarded. source_weight stays RESERVED (it
// contributes 0): P1 exposes no numeric source_score — origin/class are
// categorical provenance with no ranking semantics until consolidation (P2)
// defines curated origins. It is accepted (not rejected) so the wire shape
// is forward-stable; SourceReserved lets the caller surface a note rather
// than silently ignoring a non-zero source_weight.
type RankConfig struct {
	SemanticWeight       float64 `json:"semantic_weight"`
	RecencyWeight        float64 `json:"recency_weight"`
	RecencyHalfLifeHours float64 `json:"recency_half_life_hours"`
	SourceWeight         float64 `json:"source_weight"`
	FrequencyWeight      float64 `json:"frequency_weight"`
}

// DefaultRankConfig is pure semantic — identical to pre-RFC-I behavior.
func DefaultRankConfig() RankConfig {
	return RankConfig{SemanticWeight: 1.0}
}

// IsPureSemantic reports whether the config is equivalent to "order by
// cosine, descending" — the store's native order — so the caller can skip
// the candidate-pool over-fetch and the re-rank entirely.
//
// This requires a STRICTLY POSITIVE semantic weight with all other terms
// zero: only then does sorting by score reproduce cosine-descending. A zero
// or negative semantic weight does NOT (zero collapses every score to 0;
// negative inverts the order), so those must take the real re-rank path —
// otherwise RankCandidates would return cosine order while ScoreAll renders
// a contradicting (flat or inverted) rank_score.
func (c RankConfig) IsPureSemantic() bool {
	return c.SemanticWeight > 0 && c.RecencyWeight == 0 && c.SourceWeight == 0 && c.FrequencyWeight == 0
}

// SourceFrequencyReserved reports whether the caller set a non-zero source
// OR frequency weight. Retained for backends where BOTH remain no-ops — the
// A remote REST backend returns entries with no access_count, so its
// frequency_weight contributes 0 just like source_weight. The in-process
// backend, which now populates access_count, uses SourceReserved instead.
func (c RankConfig) SourceFrequencyReserved() bool {
	return c.SourceWeight != 0 || c.FrequencyWeight != 0
}

// SourceReserved reports whether the caller set a non-zero source_weight —
// the one term still reserved after RFC BL wired frequency_weight. The
// Memory tool surfaces this as a note so the weight isn't silently ignored.
func (c RankConfig) SourceReserved() bool {
	return c.SourceWeight != 0
}

// recencyDecay is exp(-age·ln2/halfLife): 1.0 at age 0, 0.5 at one
// half-life, → 0 as the entry ages. A non-positive half-life disables the
// recency term (returns 0) so a misconfigured block can't divide by zero
// or reward stale entries.
func recencyDecay(createdAt, now time.Time, halfLifeHours float64) float64 {
	if halfLifeHours <= 0 {
		return 0
	}
	ageHours := now.Sub(createdAt).Hours()
	if ageHours < 0 {
		ageHours = 0 // a clock-skewed future timestamp is treated as "now"
	}
	return math.Exp(-ageHours * math.Ln2 / halfLifeHours)
}

// Score computes the hybrid score for one search candidate. e.SemanticScore is
// the semantic signal — raw cosine for a non-fused/pure-vector path, or the
// fused RRF value after FuseRRF has run; e.Score (raw cosine, rendered by the
// tool) is deliberately NOT read here. See RankConfig for the term weights.
func Score(e store.MemorySearchEntry, cfg RankConfig, now time.Time) float64 {
	s := cfg.SemanticWeight * e.SemanticScore
	if cfg.RecencyWeight != 0 {
		s += cfg.RecencyWeight * recencyDecay(e.CreatedAt, now, cfg.RecencyHalfLifeHours)
	}
	if cfg.FrequencyWeight != 0 {
		// log1p keeps the term sub-linear so a runaway access_count can't
		// dominate the semantic signal; a never-accessed entry (count 0)
		// contributes exactly 0 (log1p(0)=0), so the term is inert unless
		// both the weight AND real access history are present.
		s += cfg.FrequencyWeight * math.Log1p(float64(e.AccessCount))
	}
	// source_weight · source_score is reserved: P1 has no numeric
	// source_score signal (see RankConfig), so it contributes 0.
	return s
}

// RankCandidates returns the candidates re-ordered by descending hybrid
// score. It is stable: equal scores keep the input order (which is the
// store's cosine order), so the default config yields exactly the store's
// ordering. The input slice is not mutated.
func RankCandidates(candidates []store.MemorySearchEntry, cfg RankConfig, now time.Time) []store.MemorySearchEntry {
	if len(candidates) < 2 || cfg.IsPureSemantic() {
		// Pure-semantic: the store already returned cosine-descending, so
		// re-sorting would only risk perturbing equal-score ties. Return
		// as-is for an exact zero-regression match.
		return candidates
	}
	type scored struct {
		e store.MemorySearchEntry
		s float64
	}
	pairs := make([]scored, len(candidates))
	for i, c := range candidates {
		pairs[i] = scored{e: c, s: Score(c, cfg, now)}
	}
	// Stable: equal scores keep cosine order (the store's input order).
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].s > pairs[b].s })
	out := make([]store.MemorySearchEntry, len(pairs))
	for i := range pairs {
		out[i] = pairs[i].e
	}
	return out
}

// ScoreAll returns the hybrid score for each candidate, index-aligned with
// the input. Exposed so the Memory tool can attach a rank_score to each
// rendered entry (observability without re-deriving the math).
func ScoreAll(candidates []store.MemorySearchEntry, cfg RankConfig, now time.Time) []float64 {
	out := make([]float64, len(candidates))
	for i := range candidates {
		out[i] = Score(candidates[i], cfg, now)
	}
	return out
}

// RRFDefaultK is the Reciprocal Rank Fusion constant (Cormack et al., 2009):
// score contribution for a list where an item sits at 0-based rank r is
// 1/(k+r). Larger k flattens the head so no single leg dominates; 60 is the
// widely-used default and behaves well without per-corpus tuning.
const RRFDefaultK = 60

// FuseRRF fuses the vector-ranked and full-text-ranked candidate lists into a
// single ranked list via Reciprocal Rank Fusion (RFC BL hybrid retrieval).
// Each input list MUST be pre-sorted best-first (the store legs are). The
// returned slice is the UNION keyed by entry Key (unique within a scope),
// sorted by descending fused score, with each entry's SemanticScore set to its
// RRF value:  Σ_over-lists-containing-it  1/(k + rank_in_list).
//
// The fused value goes into SemanticScore, NOT Score: downstream ranking
// (RankCandidates / Score / ScoreAll) reads SemanticScore as the semantic
// signal, so the recency and frequency terms layer on top of the fused rank
// with no second code path — while Score is left untouched (the raw vector-leg
// cosine the Memory tool renders as the stable, documented `score` field). A
// full-text-only hit that never appeared in the vector leg keeps whatever
// Score its full-text row carried (its natural relevance value); we do NOT
// fabricate a cosine for it.
//
// The vector list is added first, so for an entry present in BOTH legs the
// vector copy is authoritative for its fields (Score = its cosine; the stored
// Vector, needed for dedup; EmbeddedWith; AccessCount) while still accruing the
// full-text leg's rank contribution. When the full-text list is empty
// (SQLite, or a query with no lexical match) the union is exactly the vector
// list and RRF preserves its order, so pure-vector retrieval still works.
//
// k <= 0 falls back to RRFDefaultK. The input slices are not mutated.
func FuseRRF(vector, fulltext []store.MemorySearchEntry, k int) []store.MemorySearchEntry {
	if k <= 0 {
		k = RRFDefaultK
	}
	type acc struct {
		entry store.MemorySearchEntry
		rrf   float64
		order int // first-seen order, for a stable tie-break
	}
	idx := make(map[string]int, len(vector)+len(fulltext))
	accs := make([]acc, 0, len(vector)+len(fulltext))
	add := func(list []store.MemorySearchEntry) {
		for rank, e := range list {
			contrib := 1.0 / float64(k+rank)
			if i, ok := idx[e.Key]; ok {
				accs[i].rrf += contrib
				continue
			}
			idx[e.Key] = len(accs)
			accs = append(accs, acc{entry: e, rrf: contrib, order: len(accs)})
		}
	}
	add(vector) // first → authoritative for shared entries' fields
	add(fulltext)

	// Stable by first-seen order on equal fused score: the vector leg is
	// added first, so ties resolve toward the vector ordering.
	sort.SliceStable(accs, func(a, b int) bool {
		if accs[a].rrf != accs[b].rrf {
			return accs[a].rrf > accs[b].rrf
		}
		return accs[a].order < accs[b].order
	})
	out := make([]store.MemorySearchEntry, len(accs))
	for i := range accs {
		out[i] = accs[i].entry
		// Carry the fused rank as the semantic signal; leave Score (raw cosine)
		// intact so the tool still renders the documented-stable `score` field.
		out[i].SemanticScore = accs[i].rrf
	}
	return out
}
