package postgres

// A MEASUREMENT, not a guard: what the id-segment filter (MemorySearchFilter.IDs /
// ExcludeIDs) costs on a real Postgres + pgvector scope, so the decision to add a
// document_id column to memory_embeddings is taken on numbers rather than on a guess.
//
// It runs only when LOOMCYCLE_BENCH_FILTER=1 (plus the usual pgvector DSN), because
// seeding 100,000 rows of 1,536 dimensions takes minutes. Knobs:
//
//	LOOMCYCLE_BENCH_FILTER_N     comma list of scope sizes   (default 20000)
//	LOOMCYCLE_BENCH_FILTER_DIM   vector dimension            (default 1536, Cohere v4's)
//	LOOMCYCLE_BENCH_FILTER_DOCS  documents the chunks belong to (default 2000)
//	LOOMCYCLE_BENCH_FILTER_REPS  timed runs per cell, median reported (default 7)
//
// The rows are seeded straight in SQL (generate_series) rather than through
// MemorySet/MemoryEmbedSet: the point is the search statements, and 100,000
// round-trips would make the seed the slowest part by far.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func benchEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func TestMemorySearchFilter_IDListCostOnALargeScope(t *testing.T) {
	if os.Getenv("LOOMCYCLE_BENCH_FILTER") != "1" {
		t.Skip("LOOMCYCLE_BENCH_FILTER not set; a measurement, run by hand")
	}
	if os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("LOOMCYCLE_TEST_PG_VECTOR not set; needs a pgvector-enabled Postgres")
	}
	sizes := []int{20000}
	if raw := os.Getenv("LOOMCYCLE_BENCH_FILTER_N"); raw != "" {
		sizes = sizes[:0]
		for _, f := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n <= 0 {
				t.Fatalf("LOOMCYCLE_BENCH_FILTER_N: bad size %q", f)
			}
			sizes = append(sizes, n)
		}
	}
	dim := benchEnvInt("LOOMCYCLE_BENCH_FILTER_DIM", 1536)
	docs := benchEnvInt("LOOMCYCLE_BENCH_FILTER_DOCS", 2000)
	reps := benchEnvInt("LOOMCYCLE_BENCH_FILTER_REPS", 7)

	var report []string
	for _, n := range sizes {
		report = append(report, measureIDListCost(t, n, dim, docs, reps)...)
	}
	t.Logf("\n| N | dim | filter | list | vector: median (min) | full-text: median (min) | rows v/ft |\n|---|---|---|---|---|---|---|\n%s",
		strings.Join(report, "\n"))
}

func measureIDListCost(t *testing.T, n, dim, docs, reps int) []string {
	fix := freshSchemaWithVectors(t, pgDSNFromEnv(t), true)
	defer fix.cleanup()
	s := fix.store.(*Store)
	ctx := context.Background()
	const sid = "bench"

	// Chunk i belongs to document i*docs/n, so documents hold contiguous chunk runs
	// and a set of the first k documents is the first k*n/docs chunks. Ids are 32 hex
	// characters, the shape newDocID mints. One chunk in ten mentions "topic3", the
	// word the full-text leg searches for.
	start := time.Now()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory(tenant_id, scope, scope_id, key, value, created_at, updated_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), to_jsonb('chunk ' || i), now(), now()
		  FROM generate_series(0, $2 - 1) AS i`, sid, n); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	// The correlated `WHERE i IS NOT NULL` makes the vector subquery run per row, so
	// every row gets its own random vector instead of one shared one.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory_embeddings(tenant_id, scope, scope_id, key, provider, model, dimension, embedding, embed_text, created_at, document_id)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), 'bench', 'm', $3,
		       (SELECT array_agg(random())::real[] FROM generate_series(1, $3) WHERE i IS NOT NULL)::vector,
		       'section ' || i || ' clause about topic' || (i % 10) || ' and the board',
		       now(), md5('d' || (i::bigint * $4 / $2))
		  FROM generate_series(0, $2 - 1) AS i`, sid, n, dim, docs); err != nil {
		t.Fatalf("seed memory_embeddings: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE memory; ANALYZE memory_embeddings`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("seeded %d rows of dim %d in %s", n, dim, time.Since(start).Round(time.Millisecond))

	chunkIDs := func(k int) []string {
		out := make([]string, 0, k)
		for i := 0; i < k; i++ {
			// Past the scope's size the ids are synthetic: they cost what a real id
			// costs to bind and hash, and match nothing.
			out = append(out, md5Hex(fmt.Sprintf("c%d", i)))
		}
		return out
	}

	q := make([]float32, dim)
	for i := range q {
		q[i] = 0.5
	}
	ftQuery := "topic3 board"

	timeCell := func(label string, f store.MemorySearchFilter) (cellTiming, cellTiming, int, int) {
		f.KeyPrefix = "doc.chunk:"
		run := func() (time.Duration, time.Duration, int, int) {
			t0 := time.Now()
			v, err := s.MemoryEmbedSearch(ctx, "", store.MemoryScopeUser, sid, f, q, 51)
			if err != nil {
				t.Fatalf("%s vector: %v", label, err)
			}
			t1 := time.Now()
			ft, err := s.MemoryFullTextSearch(ctx, "", store.MemoryScopeUser, sid, f, ftQuery, 51)
			if err != nil {
				t.Fatalf("%s full-text: %v", label, err)
			}
			return t1.Sub(t0), time.Since(t1), len(v), len(ft)
		}
		run() // warm: plan cache, shared buffers
		var vs, fs []time.Duration
		var nv, nf int
		for i := 0; i < reps; i++ {
			dv, df, a, b := run()
			vs, fs = append(vs, dv), append(fs, df)
			nv, nf = a, b
		}
		return timingOf(vs), timingOf(fs), nv, nf
	}

	type cell struct {
		label string
		size  int
		f     store.MemorySearchFilter
	}
	cells := []cell{{label: "none", f: store.MemorySearchFilter{}}}
	for _, k := range []int{1000, 20000, 50000} {
		cells = append(cells, cell{label: "IDs", size: k, f: store.MemorySearchFilter{IDs: chunkIDs(k)}})
	}
	cells = append(cells, cell{label: "ExcludeIDs", size: 20000, f: store.MemorySearchFilter{ExcludeIDs: chunkIDs(20000)}})
	for _, k := range []int{10, 100, 1000} {
		docIDs := make([]string, 0, k)
		for i := 0; i < k; i++ {
			docIDs = append(docIDs, md5Hex(fmt.Sprintf("d%d", i)))
		}
		cells = append(cells, cell{label: "DocumentIDs", size: k, f: store.MemorySearchFilter{DocumentIDs: docIDs}})
	}

	var rows []string
	for _, c := range cells {
		v, ft, nv, nf := timeCell(c.label, c.f)
		rows = append(rows, fmt.Sprintf("| %d | %d | %s | %d | %s | %s | %d/%d |",
			n, dim, c.label, c.size, v, ft, nv, nf))
	}

	// The precondition probe a document-id search pays once per call.
	var probe []time.Duration
	for i := 0; i <= reps; i++ {
		t0 := time.Now()
		if _, err := s.MemoryEmbedMissingDocumentID(ctx, "", store.MemoryScopeUser, sid, []string{"doc.chunk:", "doc.unit:"}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if i > 0 {
			probe = append(probe, time.Since(t0))
		}
	}
	rows = append(rows, fmt.Sprintf("| %d | %d | missing-document-id probe | - | %s | - | - |", n, dim, timingOf(probe)))

	explainIDList(ctx, t, s, sid, q, ftQuery, chunkIDs(50000), false)
	explainIDList(ctx, t, s, sid, q, ftQuery, chunkIDs(20000), true)
	return rows
}

// cellTiming is a cell's median and fastest run. The minimum is reported beside the
// median because a shared machine's load lands on single runs: the minimum is the
// closest a run got to the statement's own cost.
type cellTiming struct{ median, min time.Duration }

func (c cellTiming) String() string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
	return ms(c.median) + " (min " + ms(c.min) + ")"
}

func timingOf(ds []time.Duration) cellTiming {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return cellTiming{median: ds[len(ds)/2], min: ds[0]}
}

func hasDocumentIDColumn(ctx context.Context, t *testing.T, s *Store) bool {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = 'memory_embeddings' AND column_name = 'document_id')`).Scan(&ok); err != nil {
		t.Fatalf("probe document_id column: %v", err)
	}
	return ok
}

// explainIDList prints the plans of both legs with a 50,000-id allowlist. The
// statements are a hand copy of the store's (same joins, same predicates), kept
// here because the store builds its SQL inline; a drift only blurs a measurement,
// it cannot make a test pass that should fail.
func explainIDList(ctx context.Context, t *testing.T, s *Store, sid string, q []float32, ftQuery string, ids []string, exclude bool) {
	probe := "= ANY($2::text[])"
	if exclude {
		probe = "<> ALL($2::text[])"
	}
	base := `FROM memory_embeddings me
	         JOIN memory m
	            ON me.tenant_id = m.tenant_id AND me.scope = m.scope
	           AND me.scope_id = m.scope_id AND me.key = m.key
	        WHERE me.tenant_id = '' AND me.scope = 'user' AND me.scope_id = $1
	          AND (m.expires_at IS NULL OR m.expires_at > now())
	          AND m.superseded_at IS NULL
	          AND me.key LIKE 'doc.chunk:%'
	          AND split_part(substr(me.key, char_length('doc.chunk:'::text) + 1), ':', 1) ` + probe
	plans := map[string]string{
		"vector": `EXPLAIN (ANALYZE, BUFFERS) SELECT me.key, 1.0 - (me.embedding <=> $3::vector) AS score ` + base +
			` AND me.dimension = $4 ORDER BY me.embedding <=> $3::vector LIMIT 51`,
		"full-text": `EXPLAIN (ANALYZE, BUFFERS) SELECT me.key, ts_rank(me.embed_text_tsv, plainto_tsquery('english', $3)) AS score ` + base +
			` AND me.embed_text_tsv @@ plainto_tsquery('english', $3) AND $4::int > 0 ORDER BY score DESC LIMIT 51`,
	}
	for _, leg := range []string{"vector", "full-text"} {
		var arg3 any = encodePgvector(q)
		if leg == "full-text" {
			arg3 = ftQuery
		}
		rows, err := s.pool.Query(ctx, plans[leg], planPerExecution([]any{sid, ids, arg3, len(q)})...)
		if err != nil {
			t.Fatalf("explain %s: %v", leg, err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("explain %s scan: %v", leg, err)
			}
			lines = append(lines, line)
		}
		rows.Close()
		t.Logf("EXPLAIN ANALYZE %s leg, %s=%d:\n%s", leg, map[bool]string{false: "IDs", true: "ExcludeIDs"}[exclude], len(ids), strings.Join(lines, "\n"))
	}
}
