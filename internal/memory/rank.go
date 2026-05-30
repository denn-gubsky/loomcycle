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
//	score = semantic_weight · cos_sim
//	      + recency_weight  · exp(-Δt·ln2 / recency_half_life_hours)
//	      + source_weight   · source_score
//	      + frequency_weight· log(1 + access_count)
//
// The DEFAULT (semantic=1, all others=0) reproduces today's pure-cosine
// ordering exactly — operators who don't pass a `rank` block see no
// behavior change and no migration.
//
// Reserved (no-op today): source_weight and frequency_weight are part of
// the locked wire shape, but the in-process memory entry carries no
// `source` or `access_count` field yet, so those two terms contribute 0
// until that tracking lands in a later slice. They are accepted (not
// rejected) so the wire shape is forward-stable; SourceFrequencyReserved
// lets the caller surface a note rather than silently ignoring a non-zero
// weight.
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

// IsPureSemantic reports whether only the semantic term is active. When
// true the hybrid re-rank is a no-op reorder (cosine order is final), so
// the caller can skip candidate-pool over-fetch.
func (c RankConfig) IsPureSemantic() bool {
	return c.RecencyWeight == 0 && c.SourceWeight == 0 && c.FrequencyWeight == 0
}

// SourceFrequencyReserved reports whether the caller set a non-zero
// source or frequency weight — terms that are reserved but contribute 0
// today. The Memory tool surfaces this as a note so the weight isn't
// silently ignored.
func (c RankConfig) SourceFrequencyReserved() bool {
	return c.SourceWeight != 0 || c.FrequencyWeight != 0
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

// Score computes the hybrid score for one search candidate. e.Score is the
// store's cosine similarity in [0,1]. See RankConfig for the reserved
// source/frequency terms (0 today).
func Score(e store.MemorySearchEntry, cfg RankConfig, now time.Time) float64 {
	s := cfg.SemanticWeight * e.Score
	if cfg.RecencyWeight != 0 {
		s += cfg.RecencyWeight * recencyDecay(e.CreatedAt, now, cfg.RecencyHalfLifeHours)
	}
	// source_weight · source_score and frequency_weight · log(1+access_count)
	// are reserved: no source/access_count on the entry yet → contribute 0.
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
