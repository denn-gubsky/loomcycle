package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryEmbedSearchKeys is the window a facet count is taken over (RFC GL P3):
// wider than MemoryEmbedSearch's 51-row pool, under the same filter. These run
// on the id-segment fixture: sixty decoys, three targets, two derived units.

func TestMemoryEmbedSearchKeys_ReturnsAWindowWiderThanTheSearchPool(t *testing.T) {
	s, sid := idSegmentFixture(t)
	ctx := context.Background()
	q := []float32{1, 0, 0, 0}

	win, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, q, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !win.Exact {
		t.Error("a sequential scan reported an inexact window")
	}
	keys := win.Keys
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

	win, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:", IDs: []string{"t1", "t3"}}, q, 100)
	if err != nil {
		t.Fatal(err)
	}
	keys := win.Keys
	sort.Strings(keys)
	if !sameKeys(keys, "doc.chunk:t1", "doc.chunk:t3") {
		t.Errorf("IDs window = %v, want exactly t1 and t3", keys)
	}

	limitedWin, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, q, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(limitedWin.Keys) != 5 {
		t.Errorf("limit 5 returned %d keys", len(limitedWin.Keys))
	}
}

func TestMemoryEmbedSearchKeys_EmptyScopeIsNoRows(t *testing.T) {
	s, _ := idSegmentFixture(t)
	win, err := s.MemoryEmbedSearchKeys(context.Background(), "", store.MemoryScopeUser, "nobody",
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, []float32{1, 0, 0, 0}, 100)
	if err != nil || len(win.Keys) != 0 || !win.Exact {
		t.Fatalf("empty scope = %+v, %v; want no keys, exact, no error", win, err)
	}
}

// TestMemoryEmbedSearchKeys_AnHNSWIndexDoesNotCutTheWindow: with an operator's
// HNSW index (migration 0017 invites one) pgvector returns at most
// hnsw.ef_search candidates, 40 by default, so a window of 100 came back with 40
// keys and no error. The keys query raises ef_search for itself, and reports the
// window inexact, since the index's candidate list still precedes the filter.
// Sequential scans and sorts are off for the session so the planner takes the
// index on a table this small (with sorts on it orders a primary-key scan).
func TestMemoryEmbedSearchKeys_AnHNSWIndexDoesNotCutTheWindow(t *testing.T) {
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; needs a pgvector-enabled Postgres")
	}
	dsn := appendOption(appendOption(pgDSNFromEnv(t), "enable_seqscan", "off"), "enable_sort", "off")
	fix := freshSchemaWithVectors(t, dsn, true)
	t.Cleanup(fix.cleanup)
	s := fix.store
	ctx := context.Background()
	const sid = "hnsw"
	for i := 0; i < 120; i++ {
		key := fmt.Sprintf("doc.chunk:c%03d", i)
		v, _ := json.Marshal("chunk")
		if err := s.MemorySet(ctx, "", store.MemoryScopeUser, sid, key, v, 0); err != nil {
			t.Fatal(err)
		}
		vec := []float32{1, float32(i) / 120, float32(i%7) / 7, float32(i%3) / 3}
		if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, sid, key, store.MemoryEmbedding{
			Provider: "test", Model: "m", Dimension: 4, Vector: vec, EmbedText: "chunk", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	pool := s.(*Store).pool
	for _, stmt := range []string{
		`ALTER TABLE memory_embeddings ALTER COLUMN embedding TYPE vector(4)`,
		`CREATE INDEX memory_embeddings_hnsw ON memory_embeddings USING hnsw (embedding vector_cosine_ops)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	win, err := s.MemoryEmbedSearchKeys(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, []float32{1, 0, 0, 0}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Keys) != 100 {
		t.Errorf("window of 100 over an HNSW index = %d keys, want 100", len(win.Keys))
	}
	if win.Exact {
		t.Error("a window served by an approximate index reported Exact")
	}
}
