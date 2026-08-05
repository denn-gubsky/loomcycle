package postgres

// RFC BW §7 required this measurement before shipping the filter, not after: a WHERE
// clause on a pgvector query can degrade badly — an ANN index returns candidates, the
// predicate discards them, and the planner may fall back to a scan.
//
// MEASURED RESULT: the opposite. On a 2,942-row scope shaped like the reference
// deployment (~98% document chunks), excluding documents was ~31x FASTER —
// 30ms vs 928ms unfiltered, with only-documents at 181ms.
//
// And the reason matters more than the number. Migration 0017 deliberately creates NO
// ANN index ("the v0.9.0 default is sequential scan with the (scope, scope_id) partial
// filter... HNSW is intentionally NOT created here because it requires a
// single-dimension column"). With no index there is nothing to degrade: the query is a
// scan plus a sort, so a predicate that cuts 2,942 candidate rows to 42 cuts the work
// proportionally.
//
// THE RISK RETURNS IF AN OPERATOR OPTS INTO HNSW, which 0017 explicitly invites. That
// is the case this test would catch, which is why the ratio assertion stays even though
// today's margin is enormous in the other direction.
//
// Not a Go Benchmark: it needs a seeded scope, so it runs once under -run and logs
// timings for a human to read.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestMemorySearchFilter_ANNCostOnADocumentHeavyScope(t *testing.T) {
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; needs a pgvector-enabled Postgres")
	}
	fix := freshSchemaWithVectors(t, pgDSNFromEnv(t), true)
	t.Cleanup(fix.cleanup)
	s := fix.store
	ctx := context.Background()
	const (
		sid   = "annbench"
		docs  = 2900
		facts = 42
	)
	v, _ := json.Marshal("a body of text about assorted topics")
	seed := func(key string, i int) {
		if err := s.MemorySet(ctx, "", store.MemoryScopeUser, sid, key, v, 0); err != nil {
			t.Fatal(err)
		}
		// Spread the vectors so the ANN index has real work rather than one cluster.
		vec := []float32{float32(i%97) / 97, float32(i%31) / 31, float32(i%13) / 13, 1}
		if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, sid, key, store.MemoryEmbedding{
			Provider: "test", Model: "m", Dimension: 4, Vector: vec,
			EmbedText: key, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < docs; i++ {
		seed(fmt.Sprintf("doc.chunk:%05d", i), i)
	}
	for i := 0; i < facts; i++ {
		seed(fmt.Sprintf("memory/fact/%03d", i), i*7)
	}

	q := []float32{0.5, 0.5, 0.5, 1}
	timeIt := func(label string, f store.MemorySearchFilter) (time.Duration, int) {
		// Warm once so the first call's plan/cache does not dominate.
		if _, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid, f, q, 10); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		start := time.Now()
		const reps = 20
		var n int
		for i := 0; i < reps; i++ {
			rows, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid, f, q, 10)
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			n = len(rows)
		}
		return time.Since(start) / reps, n
	}

	unfiltered, nAll := timeIt("unfiltered", store.MemorySearchFilter{})
	excluded, nFacts := timeIt("exclude docs", store.MemorySearchFilter{ExcludeKeyPrefix: "doc.chunk:"})
	onlyDocs, nDocs := timeIt("only docs", store.MemorySearchFilter{KeyPrefix: "doc.chunk:"})

	t.Logf("scope: %d document chunks + %d facts", docs, facts)
	t.Logf("unfiltered   %8s  rows=%d", unfiltered.Round(time.Microsecond), nAll)
	t.Logf("exclude docs %8s  rows=%d", excluded.Round(time.Microsecond), nFacts)
	t.Logf("only docs    %8s  rows=%d", onlyDocs.Round(time.Microsecond), nDocs)

	// The CORRECTNESS assertion is the point; the timings are reported for a human to
	// read. Only a pathological regression fails the test, so a slow CI box does not.
	if nFacts != 10 {
		t.Errorf("exclude-docs returned %d rows, want 10 — the whole reason the filter is "+
			"in SQL is that a post-filter on a 51-row pool could not fill a top-10", nFacts)
	}
	// Generous, because the measured direction is the reverse: this fires only if an
	// added ANN index turns the predicate into a planner fallback (RFC BW §7).
	if excluded > 40*unfiltered+40*time.Millisecond {
		t.Errorf("excluding documents cost %s vs %s unfiltered — that is the filtered-ANN "+
			"fallback RFC BW §7 warned about, and it appears once an HNSW index exists; "+
			"consider a partial index matching the key predicate", excluded, unfiltered)
	}
}
