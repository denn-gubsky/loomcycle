package postgres

import (
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// An id list bound as `= ANY($n::text[])` is hashed only when the planner sees the
// array as a CONSTANT, which it does in a plan built for the bound values (a
// custom plan). A cached prepared statement switches to a GENERIC plan after five
// executions whenever that looks no dearer, and in a generic plan the array is a
// parameter: no hash, a linear walk of the whole list for every row. Measured on
// 20,000 rows with a 20,000-id list: 9 ms custom, 18.4 s generic. pgx caches
// prepared statements per connection by default, so a busy replica reached the
// generic plan on its sixth filtered search.
//
// The assertion is on the plan cache, not on a timing: no prepared statement
// carrying an id list may have run a generic plan.
func TestMemorySearch_IDListStatementsNeverRunAGenericPlan(t *testing.T) {
	fix := freshSchemaWithVectorsOrSkip(t)
	ctx := context.Background()
	// ONE connection, so every search and the plan-cache probe share a session:
	// pg_prepared_statements is per session.
	s, err := Open(ctx, Config{DSN: fix.storeDSN, MaxOpenConns: 1, PgvectorEnabled: true})
	if err != nil {
		t.Fatalf("open single-connection store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	const sid = "plans"
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory(tenant_id, scope, scope_id, key, value, created_at, updated_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), to_jsonb('chunk ' || i), now(), now()
		  FROM generate_series(0, 1999) AS i`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory_embeddings(tenant_id, scope, scope_id, key, provider, model, dimension, embedding, embed_text, created_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), 'test', 'm', 4,
		       ('[' || (i % 7) || ',' || (i % 11) || ',' || (i % 13) || ',1]')::vector,
		       'clause about topic' || (i % 10), now()
		  FROM generate_series(0, 1999) AS i`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE memory; ANALYZE memory_embeddings`); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 1500)
	for i := 0; i < 1500; i++ {
		ids = append(ids, md5Hex(fmt.Sprintf("c%d", i)))
	}
	filters := map[string]store.MemorySearchFilter{
		"ids":     {KeyPrefix: "doc.chunk:", IDs: ids},
		"exclude": {KeyPrefix: "doc.chunk:", ExcludeIDs: ids},
		// A mixed (no-selector) Memory search confines the lists to the document
		// class; its statement binds the same list and must plan the same way.
		"mixed ids":     {IDs: ids, IDsPrefixes: []string{"doc.chunk:", "doc.unit:"}},
		"mixed exclude": {ExcludeIDs: ids, IDsPrefixes: []string{"doc.chunk:", "doc.unit:"}},
	}
	for name, f := range filters {
		for i := 0; i < 12; i++ {
			if _, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid, f, []float32{1, 2, 3, 1}, 51); err != nil {
				t.Fatalf("%s vector #%d: %v", name, i, err)
			}
			if _, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, sid, f, "topic3", 51); err != nil {
				t.Fatalf("%s full-text #%d: %v", name, i, err)
			}
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT generic_plans, custom_plans, left(regexp_replace(statement, '\s+', ' ', 'g'), 120)
		  FROM pg_prepared_statements
		 WHERE statement LIKE '%::text[])%' AND statement NOT LIKE '%pg_prepared_statements%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var generic, custom int64
		var stmt string
		if err := rows.Scan(&generic, &custom, &stmt); err != nil {
			t.Fatal(err)
		}
		if generic > 0 {
			t.Errorf("an id-list statement ran %d generic plan(s) (custom %d): in a generic plan the "+
				"list is a parameter and `= ANY` walks it linearly for every row — %s…", generic, custom, stmt)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// freshSchemaWithVectorsOrSkip is freshSchemaWithVectors behind the pgvector gate.
func freshSchemaWithVectorsOrSkip(t *testing.T) freshFixture {
	t.Helper()
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; needs a pgvector-enabled Postgres")
	}
	fix := freshSchemaWithVectors(t, pgDSNFromEnv(t), true)
	t.Cleanup(fix.cleanup)
	return fix
}

func md5Hex(s string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(s)))
}
