package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestIDSegmentDocumentCondition_DenylistIsAnAntiJoinWithinATenfoldEstimate is
// TestIDSegmentCondition_DenylistEstimateIsWithinTenfold for the class-aware form
// a mixed search uses (IDsPrefixes), where non-document rows must pass the lists.
//
// The scope mixes what a Memory search with no selector reads: chunk bodies,
// their units, notes, and facts homed in a chunk (provenance). Two assertions:
// the estimate of the filtered join is within a factor of ten of the actual, and
// a denylist plans as an anti-join.
//
// Measured, the estimate half held for the `<> ALL` form too: inside the class
// OR, the provenance disjunct's own default selectivity keeps the estimate near
// the scope's size, so the one-row collapse never happens there. What the form
// cost instead was the join filter (see idSegmentDocumentCondition), which the
// anti-join half catches.
func TestIDSegmentDocumentCondition_DenylistIsAnAntiJoinWithinATenfoldEstimate(t *testing.T) {
	fix := freshSchemaWithVectorsOrSkip(t)
	s := fix.store.(*Store)
	ctx := context.Background()
	const sid, n = "estimate-mixed", 3000
	// key, origin per row class: chunks, one unit per chunk for the first 1,000,
	// a few notes, and a few facts homed in a chunk — a scope that is mostly
	// documents, as a document store is. (With the non-document rows a large share
	// of the scope, they alone carry the estimate within tenfold, and the case
	// would not see the document disjuncts collapse.)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory(tenant_id, scope, scope_id, key, value, origin, created_at, updated_at)
		SELECT '', 'user', $1, k, to_jsonb(k), o, now(), now() FROM (
		  SELECT 'doc.chunk:' || md5('c' || i) AS k, NULL::text AS o FROM generate_series(0, $2 - 1) AS i
		  UNION ALL SELECT 'doc.unit:' || md5('c' || i) || ':claim:0', NULL FROM generate_series(0, 999) AS i
		  UNION ALL SELECT 'notes/n' || i, NULL FROM generate_series(0, 19) AS i
		  UNION ALL SELECT 'doc.chunk:' || md5('f' || i), 'consolidator' FROM generate_series(0, 9) AS i
		) rows`, sid, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory_embeddings(tenant_id, scope, scope_id, key, provider, model, dimension, embedding, embed_text, created_at)
		SELECT '', 'user', $1, key, 'test', 'm', 4,
		       ('[' || (length(key) % 7) || ',' || (ascii(right(key, 1)) % 11) || ',1,1]')::vector,
		       'clause about topic', now()
		  FROM memory WHERE scope_id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE memory; ANALYZE memory_embeddings`); err != nil {
		t.Fatal(err)
	}
	ids := func(from, to int) []string {
		out := make([]string, 0, to-from)
		for i := from; i < to; i++ {
			out = append(out, md5Hex(fmt.Sprintf("c%d", i)))
		}
		return out
	}
	cases := map[string]store.MemorySearchFilter{
		"exclude half":           {ExcludeIDs: ids(0, 1500)},
		"exclude most":           {ExcludeIDs: ids(0, 2500)},
		"exclude ids not stored": {ExcludeIDs: ids(n, n+1500)},
		"allow half":             {IDs: ids(0, 1500)},
		"allow and exclude":      {IDs: ids(0, 2000), ExcludeIDs: ids(1000, 2500)},
	}
	legs := map[string]string{
		"vector":    ` AND me.dimension = 4`,
		"full-text": ` AND me.embed_text_tsv @@ plainto_tsquery('english', 'clause')`,
	}
	for name, f := range cases {
		f.IDsPrefixes = []string{"doc.chunk:", "doc.unit:"}
		for leg, legCond := range legs {
			cond, args := idSegmentCondition(f, []any{sid})
			from := ` FROM memory_embeddings me
			  JOIN memory m
			    ON me.tenant_id = m.tenant_id AND me.scope = m.scope
			   AND me.scope_id = m.scope_id AND me.key = m.key
			 WHERE me.tenant_id = '' AND me.scope = 'user' AND me.scope_id = $1
			   AND (m.expires_at IS NULL OR m.expires_at > now())
			   AND m.superseded_at IS NULL` + legCond + cond

			var actual float64
			if err := s.pool.QueryRow(ctx, `SELECT count(*)`+from, planPerExecution(args)...).Scan(&actual); err != nil {
				t.Fatalf("%s/%s count: %v", name, leg, err)
			}
			var raw []byte
			if err := s.pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT me.key`+from, planPerExecution(args)...).Scan(&raw); err != nil {
				t.Fatalf("%s/%s explain: %v", name, leg, err)
			}
			var plan []struct {
				Plan struct {
					NodeType string  `json:"Node Type"`
					Rows     float64 `json:"Plan Rows"`
				} `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
				t.Fatalf("%s/%s decode plan: %v (%s)", name, leg, err, raw)
			}
			est := plan[0].Plan.Rows
			if actual == 0 {
				t.Fatalf("%s/%s: the fixture leaves no row; the case asserts nothing", name, leg)
			}
			if est*10 < actual || est > actual*10 {
				t.Errorf("%s/%s: the planner estimates %.0f rows (top node %s) where %.0f survive the class-aware id lists",
					name, leg, est, plan[0].Plan.NodeType, actual)
			}
			// The denylist is a hashed anti-join against the list, never a per-row
			// test inside the class OR: there a `<> ALL` or NOT EXISTS stays a
			// filter on the join, and a NOT EXISTS a correlated subplan that scans
			// the whole list for every row.
			if len(f.ExcludeIDs) > 0 && !bytes.Contains(raw, []byte(`"Join Type": "Anti"`)) {
				t.Errorf("%s/%s: the denylist is not planned as an anti-join:\n%s", name, leg, raw)
			}
		}
	}
}
