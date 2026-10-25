package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The planner's row estimate for an id-filtered search must stay near the truth,
// because both legs choose their plan from it. `seg <> ALL($excl)` is estimated as
// the product of one `<>` selectivity per list element (0.995 each with no
// statistics on the expression), so a list of a thousand ids or more is estimated at
// ONE row whatever the scope holds. On one row the planner joins memory first and
// then sorts, evaluating the whole projection — `embedding::text` included — for
// every surviving row on one core: on 100,000 rows of 1,536 dimensions with 20,000
// ids excluded, 4.9 s on the vector leg and 0.5 s on the full-text leg, against
// 0.28 s and 17 ms unfiltered.
//
// The assertion is on the estimate, not on a timing: for each list shape the
// estimated row count of the filtered join is within a factor of ten of the actual
// count. The predicate is the store's own (idSegmentCondition); only the statement
// around it — the two legs' FROM/JOIN/WHERE without their ORDER BY and LIMIT, which
// would cap the estimate — is written here.
func TestIDSegmentCondition_DenylistEstimateIsWithinTenfold(t *testing.T) {
	fix := freshSchemaWithVectorsOrSkip(t)
	s := fix.store.(*Store)
	ctx := context.Background()
	const sid, n = "estimate", 3000
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory(tenant_id, scope, scope_id, key, value, created_at, updated_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), to_jsonb('chunk ' || i), now(), now()
		  FROM generate_series(0, $2 - 1) AS i`, sid, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory_embeddings(tenant_id, scope, scope_id, key, provider, model, dimension, embedding, embed_text, created_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), 'test', 'm', 4,
		       ('[' || (i % 7) || ',' || (i % 11) || ',' || (i % 13) || ',1]')::vector,
		       'clause about topic' || (i % 10), now()
		  FROM generate_series(0, $2 - 1) AS i`, sid, n); err != nil {
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
		f.KeyPrefix = "doc.chunk:"
		for leg, legCond := range legs {
			cond, args := idSegmentCondition(f, []any{sid})
			from := ` FROM memory_embeddings me
			  JOIN memory m
			    ON me.tenant_id = m.tenant_id AND me.scope = m.scope
			   AND me.scope_id = m.scope_id AND me.key = m.key
			 WHERE me.tenant_id = '' AND me.scope = 'user' AND me.scope_id = $1
			   AND (m.expires_at IS NULL OR m.expires_at > now())
			   AND m.superseded_at IS NULL
			   AND me.key LIKE 'doc.chunk:%'` + legCond + cond

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
				t.Errorf("%s/%s: the planner estimates %.0f rows (top node %s) where %.0f survive the id lists; "+
					"an estimate this far off picks a nested loop and a serial sort over every surviving row",
					name, leg, est, plan[0].Plan.NodeType, actual)
			}
		}
	}
}
