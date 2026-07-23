package memory

import (
	"math"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A keyword-only match — an entry the vector leg ranks LAST — must surface via
// the full-text leg after Reciprocal Rank Fusion. This is the core hybrid
// guarantee: a lexical hit a pure-vector top-K would drop is promoted by RRF.
func TestSearch_RRFSurfacesKeywordOnlyMatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Vector leg (cosine order, best-first): D is the WORST match (last).
	vector := []store.MemorySearchEntry{
		entry("A", 0.90, 0, now),
		entry("B", 0.80, 0, now),
		entry("C", 0.70, 0, now),
		entry("D", 0.10, 0, now), // vector rank 3 → outside a top-2 cut
	}
	// Full-text leg: D is the single lexical match (keyword rank 0).
	fulltext := []store.MemorySearchEntry{
		entry("D", 0, 0, now),
	}

	// Sanity: pure-vector top-2 would be [A, B] — D is excluded.
	if vector[0].Key != "A" || vector[1].Key != "B" {
		t.Fatalf("fixture: expected pure-vector top-2 A,B, got %s,%s", vector[0].Key, vector[1].Key)
	}

	fused := FuseRRF(vector, fulltext, RRFDefaultK)
	if len(fused) != 4 {
		t.Fatalf("fused union size = %d, want 4 (A,B,C,D)", len(fused))
	}
	// D appears in BOTH legs, so its RRF sums 1/(k+3)+1/(k+0) and it leaps to
	// the top — proving the keyword-only match surfaced past the vector head.
	if fused[0].Key != "D" {
		t.Fatalf("keyword-only match did not surface: fused order = %s,%s,%s,%s",
			fused[0].Key, fused[1].Key, fused[2].Key, fused[3].Key)
	}
	// It must land within a top-2 cut (which pure vector denied it).
	if fused[0].Key != "D" && fused[1].Key != "D" {
		t.Fatalf("D not in fused top-2: %s,%s", fused[0].Key, fused[1].Key)
	}
}

// With an empty full-text leg (SQLite, or no lexical match) the union is
// exactly the vector list and RRF preserves its order — pure-vector retrieval
// still works, and the fused Score decreases monotonically with vector rank.
func TestFuseRRF_EmptyFullTextPreservesVectorOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	vector := []store.MemorySearchEntry{
		entry("a", 0.9, 0, now),
		entry("b", 0.8, 0, now),
		entry("c", 0.7, 0, now),
	}
	fused := FuseRRF(vector, nil, RRFDefaultK)
	if len(fused) != 3 {
		t.Fatalf("fused size = %d, want 3", len(fused))
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if fused[i].Key != w {
			t.Fatalf("order[%d] = %s, want %s (vector order not preserved)", i, fused[i].Key, w)
		}
	}
	// The fused RRF signal (SemanticScore) strictly decreases with rank; Score
	// stays the raw cosine (not the RRF value), so assert on SemanticScore.
	if !(fused[0].SemanticScore > fused[1].SemanticScore && fused[1].SemanticScore > fused[2].SemanticScore) {
		t.Fatalf("fused SemanticScore not strictly descending: %v %v %v",
			fused[0].SemanticScore, fused[1].SemanticScore, fused[2].SemanticScore)
	}
}

// Fix #1 (RFC BL PR2 review): RRF fusion must NOT clobber the raw-cosine Score
// the Memory tool renders — the fused rank rides SemanticScore instead. Assert
// each fused entry's Score stays ≈ its vector-leg cosine while the ORDER
// reflects the RRF fusion (a keyword-only hit is promoted past its vector rank).
func TestFuseRRF_PreservesRawCosineScore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Vector leg (cosine order): C is the WORST vector match (last).
	vector := []store.MemorySearchEntry{
		entry("A", 0.90, 0, now),
		entry("B", 0.80, 0, now),
		entry("C", 0.10, 0, now),
	}
	// Full-text leg: C is the sole lexical hit (keyword rank 0) — its RRF sums
	// across both legs and should lift it to the top.
	fulltext := []store.MemorySearchEntry{
		entry("C", 0.0, 0, now),
	}

	fused := FuseRRF(vector, fulltext, RRFDefaultK)

	// Score stays the raw vector cosine for every entry present in the vector
	// leg — fusion must not overwrite it with the ~1/60 RRF value.
	wantCosine := map[string]float64{"A": 0.90, "B": 0.80, "C": 0.10}
	for _, e := range fused {
		if got := wantCosine[e.Key]; math.Abs(e.Score-got) > 1e-9 {
			t.Errorf("entry %s: score = %v, want raw cosine %v (fusion clobbered Score)", e.Key, e.Score, got)
		}
		// The fused semantic signal lives in SemanticScore: a small positive RRF
		// value (≈1/(k+rank)), clearly distinct from the cosine in Score.
		if e.SemanticScore <= 0 || e.SemanticScore > 0.5 {
			t.Errorf("entry %s: SemanticScore = %v, want a small positive RRF value", e.Key, e.SemanticScore)
		}
	}
	// Order reflects RRF: C, the keyword-only hit, is promoted above its
	// last-place vector rank because it co-ranks in both legs.
	if fused[0].Key != "C" {
		t.Fatalf("RRF did not promote the keyword-only hit: order = %s,%s,%s",
			fused[0].Key, fused[1].Key, fused[2].Key)
	}
}
