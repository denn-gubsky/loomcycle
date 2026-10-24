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

// The id-segment filter (MemorySearchFilter.IDs / ExcludeIDs) must apply INSIDE
// the statement, before the LIMIT: each leg is capped at 51 rows, so a corpus of
// three chunks inside a scope of sixty closer decoys is unreachable by any
// post-filter. These tests seed exactly that shape on a real pgvector store.

func idSegmentFixture(t *testing.T) (store.Store, string) {
	t.Helper()
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; needs a pgvector-enabled Postgres")
	}
	fix := freshSchemaWithVectors(t, pgDSNFromEnv(t), true)
	t.Cleanup(fix.cleanup)
	s := fix.store
	ctx := context.Background()
	const sid = "idseg"
	seed := func(key, text string, vec []float32) {
		v, _ := json.Marshal(text)
		if err := s.MemorySet(ctx, "", store.MemoryScopeUser, sid, key, v, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, sid, key, store.MemoryEmbedding{
			Provider: "test", Model: "m", Dimension: 4, Vector: vec, EmbedText: text, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Sixty decoys that match the query better on BOTH legs: an identical vector,
	// and the query's words twice over.
	for i := 0; i < 60; i++ {
		seed(fmt.Sprintf("doc.chunk:decoy%02d", i), "protocol 86 board protocol 86 board", []float32{1, 0, 0, 0})
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		seed("doc.chunk:"+id, "protocol 86 norm "+id, []float32{1, 1, 0, 0})
	}
	// A derived unit of t1 and of a decoy: the id segment of a unit key is its
	// chunk id, so the same list must reach them.
	seed("doc.unit:t1:claim:0", "protocol 86 claim", []float32{1, 1, 0, 0})
	seed("doc.unit:decoy00:claim:0", "protocol 86 claim", []float32{1, 0, 0, 0})
	return s, sid
}

func keysOf(rows []store.MemorySearchEntry) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key)
	}
	sort.Strings(out)
	return out
}

func sameKeys(got []string, want ...string) bool {
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestMemoryEmbedSearch_IDsRestrictBeforeLimit(t *testing.T) {
	s, sid := idSegmentFixture(t)
	ctx := context.Background()
	q := []float32{1, 0, 0, 0}

	// Precondition: without the list the 51-row pool holds no target at all.
	all, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, q, 51)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Key == "doc.chunk:t1" || r.Key == "doc.chunk:t2" || r.Key == "doc.chunk:t3" {
			t.Fatalf("precondition: a target reached the unfiltered pool (%s)", r.Key)
		}
	}

	got, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:", IDs: []string{"t1", "t2", "t3"}}, q, 3)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(got); !sameKeys(k, "doc.chunk:t1", "doc.chunk:t2", "doc.chunk:t3") {
		t.Errorf("IDs search = %v, want exactly the three targets", k)
	}

	// The unit namespace: the same list, relative to the unit prefix.
	units, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.unit:", IDs: []string{"t1", "t2", "t3"}}, q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(units); !sameKeys(k, "doc.unit:t1:claim:0") {
		t.Errorf("IDs on the unit namespace = %v, want only t1's unit", k)
	}
}

func TestMemoryFullTextSearch_IDsRestrictBeforeLimit(t *testing.T) {
	s, sid := idSegmentFixture(t)
	ctx := context.Background()

	all, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:"}, "protocol 86", 51)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Key == "doc.chunk:t1" || r.Key == "doc.chunk:t2" || r.Key == "doc.chunk:t3" {
			t.Fatalf("precondition: a target reached the unfiltered pool (%s)", r.Key)
		}
	}

	got, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:", IDs: []string{"t1", "t2", "t3"}}, "protocol 86", 3)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(got); !sameKeys(k, "doc.chunk:t1", "doc.chunk:t2", "doc.chunk:t3") {
		t.Errorf("IDs full-text search = %v, want exactly the three targets", k)
	}

	excl, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.unit:", ExcludeIDs: []string{"decoy00"}}, "protocol 86", 10)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(excl); !sameKeys(k, "doc.unit:t1:claim:0") {
		t.Errorf("ExcludeIDs full-text on units = %v, want only t1's unit", k)
	}
}

func TestMemoryEmbedSearch_ExcludeIDs(t *testing.T) {
	s, sid := idSegmentFixture(t)
	ctx := context.Background()
	q := []float32{1, 0, 0, 0}
	var excl []string
	for i := 0; i < 60; i++ {
		excl = append(excl, fmt.Sprintf("decoy%02d", i))
	}
	got, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:", ExcludeIDs: excl}, q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(got); !sameKeys(k, "doc.chunk:t1", "doc.chunk:t2", "doc.chunk:t3") {
		t.Errorf("ExcludeIDs search = %v, want only the three non-excluded chunks", k)
	}
	// IDs and ExcludeIDs compose as AND.
	both, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid,
		store.MemorySearchFilter{KeyPrefix: "doc.chunk:", IDs: []string{"t1", "t2"}, ExcludeIDs: []string{"t2"}}, q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if k := keysOf(both); !sameKeys(k, "doc.chunk:t1") {
		t.Errorf("IDs AND ExcludeIDs = %v, want only t1", k)
	}
}
