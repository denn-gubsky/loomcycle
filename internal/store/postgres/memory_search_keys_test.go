package postgres

import (
	"context"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryEmbedSearchKeys is the window a facet count is taken over (RFC GL P3):
// wider than MemoryEmbedSearch's 51-row pool, under the same filter. These run
// on the id-segment fixture: sixty decoys, three targets, two derived units.

func TestMemoryEmbedSearchKeys_ReturnsAWindowWiderThanTheSearchPool(t *testing.T) {
	s, sid := idSegmentFixture(t)
	ctx := context.Background()
	q := []float32{1, 0, 0, 0}

	keys, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, q, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 63 chunk bodies; the units are outside the prefix.
	if len(keys) != 63 {
		t.Fatalf("window = %d keys, want all 63 chunk bodies (more than the 51 a search returns)", len(keys))
	}
	// Closest first: the three targets sit at a larger cosine distance than every
	// decoy, so they close the window.
	tail := append([]string(nil), keys[60:]...)
	sort.Strings(tail)
	if !sameKeys(tail, "doc.chunk:t1", "doc.chunk:t2", "doc.chunk:t3") {
		t.Errorf("last three keys = %v, want the three farther targets", tail)
	}
}

func TestMemoryEmbedSearchKeys_AppliesTheIDListAndTheLimit(t *testing.T) {
	s, sid := idSegmentFixture(t)
	ctx := context.Background()
	q := []float32{1, 0, 0, 0}

	keys, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:", IDs: []string{"t1", "t3"}}, q, 100)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	if !sameKeys(keys, "doc.chunk:t1", "doc.chunk:t3") {
		t.Errorf("IDs window = %v, want exactly t1 and t3", keys)
	}

	limited, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, q, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 5 {
		t.Errorf("limit 5 returned %d keys", len(limited))
	}
}

func TestMemoryEmbedSearchKeys_EmptyScopeIsNoRows(t *testing.T) {
	s, _ := idSegmentFixture(t)
	keys, err := s.MemoryEmbedSearchKeys(context.Background(), "", store.MemoryScopeUser, "nobody",
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, []float32{1, 0, 0, 0}, 100)
	if err != nil || len(keys) != 0 {
		t.Fatalf("empty scope = %v, %v; want no keys, no error", keys, err)
	}
}
