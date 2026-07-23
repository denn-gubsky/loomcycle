package memory

import (
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
	// RRF scores strictly decrease with rank.
	if !(fused[0].Score > fused[1].Score && fused[1].Score > fused[2].Score) {
		t.Fatalf("fused scores not strictly descending: %v %v %v", fused[0].Score, fused[1].Score, fused[2].Score)
	}
}
