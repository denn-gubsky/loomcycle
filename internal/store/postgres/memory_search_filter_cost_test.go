package postgres

// A MEASUREMENT, not a guard: what the id-segment filter (MemorySearchFilter.IDs /
// ExcludeIDs) costs on a real Postgres + pgvector scope, next to the same search
// unfiltered, so a change to the predicate is judged on numbers. The guard for the
// denylist's plan is TestIDSegmentCondition_DenylistEstimateIsWithinTenfold; this
// test is what produced the numbers that guard stands for.
//
// It runs only when LOOMCYCLE_BENCH_FILTER=1 (plus the usual pgvector DSN), because
// seeding 100,000 rows of 1,536 dimensions takes a while. Knobs:
//
//	LOOMCYCLE_BENCH_FILTER_N     comma list of scope sizes   (default 20000)
//	LOOMCYCLE_BENCH_FILTER_DIM   vector dimension            (default 1536, Cohere v4's)
//	LOOMCYCLE_BENCH_FILTER_REPS  timed runs per cell, median reported (default 7)
//
// The rows are seeded straight in SQL (generate_series) rather than through
// MemorySet/MemoryEmbedSet: the point is the search statements, and 100,000
// round-trips would make the seed the slowest part by far.

import (
	"context"
	"crypto/sha256"
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
	reps := benchEnvInt("LOOMCYCLE_BENCH_FILTER_REPS", 7)

	var report []string
	for _, n := range sizes {
		report = append(report, measureIDListCost(t, n, dim, reps)...)
	}
	t.Logf("\n| N | dim | filter | list | vector: median (min) | full-text: median (min) | rows v/ft | result keys |\n|---|---|---|---|---|---|---|---|\n%s",
		strings.Join(report, "\n"))
}

func measureIDListCost(t *testing.T, n, dim, reps int) []string {
	fix := freshSchemaWithVectors(t, pgDSNFromEnv(t), true)
	defer fix.cleanup()
	s := fix.store.(*Store)
	ctx := context.Background()
	const sid = "bench"

	// Ids are 32 hex characters, the shape newDocID mints. One chunk in ten mentions
	// "topic3", the word the full-text leg searches for.
	start := time.Now()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory(tenant_id, scope, scope_id, key, value, created_at, updated_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), to_jsonb('chunk ' || i), now(), now()
		  FROM generate_series(0, $2 - 1) AS i`, sid, n); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	// Each row's vector is sin() over (row, component): deterministic, so two runs
	// (say before and after a change) seed the same rows and the "result keys"
	// column compares across them, and distinct per row because the subquery is
	// correlated with i.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory_embeddings(tenant_id, scope, scope_id, key, provider, model, dimension, embedding, embed_text, created_at)
		SELECT '', 'user', $1, 'doc.chunk:' || md5('c' || i), 'bench', 'm', $3,
		       (SELECT array_agg(sin(i::float8 * $3 + g))::real[] FROM generate_series(1, $3) AS g)::vector,
		       'section ' || i || ' clause about topic' || (i % 10) || ' and the board',
		       now()
		  FROM generate_series(0, $2 - 1) AS i`, sid, n, dim); err != nil {
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

	// A fixed query vector, so a run before a change and a run after it rank the
	// same seeded rows and the "result keys" column can be compared across runs.
	q := make([]float32, dim)
	for i := range q {
		q[i] = 0.5
	}
	ftQuery := "topic3 board"

	timeCell := func(label string, f store.MemorySearchFilter) (cellTiming, cellTiming, int, int, string) {
		f.KeyPrefix = "doc.chunk:"
		var keys []string
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
			keys = append(keysOf(v), keysOf(ft)...)
			return t1.Sub(t0), time.Since(t1), len(v), len(ft)
		}
		run() // warm: shared buffers
		var vs, fs []time.Duration
		var nv, nf int
		for i := 0; i < reps; i++ {
			dv, df, a, b := run()
			vs, fs = append(vs, dv), append(fs, df)
			nv, nf = a, b
		}
		return timingOf(vs), timingOf(fs), nv, nf, keysDigest(keys)
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
	for _, k := range []int{1000, 20000, 50000} {
		cells = append(cells, cell{label: "ExcludeIDs", size: k, f: store.MemorySearchFilter{ExcludeIDs: chunkIDs(k)}})
	}

	var rows []string
	for _, c := range cells {
		v, ft, nv, nf, digest := timeCell(c.label, c.f)
		rows = append(rows, fmt.Sprintf("| %d | %d | %s | %d | %s | %s | %d/%d | %s |",
			n, dim, c.label, c.size, v, ft, nv, nf, digest))
	}

	explainIDList(ctx, t, s, sid, q, ftQuery, store.MemorySearchFilter{KeyPrefix: "doc.chunk:", IDs: chunkIDs(50000)})
	explainIDList(ctx, t, s, sid, q, ftQuery, store.MemorySearchFilter{KeyPrefix: "doc.chunk:", ExcludeIDs: chunkIDs(20000)})
	return rows
}

// keysDigest is a short fingerprint of a cell's ranked result keys (both legs, in
// order), so "results identical before and after" is one column to compare.
func keysDigest(keys []string) string {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return fmt.Sprintf("%x", sum[:6])
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

// explainIDList prints the plans of both legs for one id filter. The surrounding
// statements are a hand copy of the store's (same joins, same predicates, same
// projection — the projection matters: `embedding::text` evaluated below the LIMIT
// is most of a bad plan's cost), but the id predicate is the store's own, built by
// idSegmentCondition. A drift in the copy only blurs a measurement; it cannot make
// a test pass that should fail.
func explainIDList(ctx context.Context, t *testing.T, s *Store, sid string, q []float32, ftQuery string, f store.MemorySearchFilter) {
	t.Helper()
	label := fmt.Sprintf("IDs=%d", len(f.IDs))
	if len(f.ExcludeIDs) > 0 {
		label = fmt.Sprintf("ExcludeIDs=%d", len(f.ExcludeIDs))
	}
	base := `FROM memory_embeddings me
	         JOIN memory m
	            ON me.tenant_id = m.tenant_id AND me.scope = m.scope
	           AND me.scope_id = m.scope_id AND me.key = m.key
	        WHERE me.tenant_id = '' AND me.scope = 'user' AND me.scope_id = $1
	          AND (m.expires_at IS NULL OR m.expires_at > now())
	          AND m.superseded_at IS NULL
	          AND me.key LIKE 'doc.chunk:%'`
	cols := `me.key, m.value, m.expires_at, m.created_at, m.updated_at, m.observed_at, m.valid_at, m.invalid_at, %s AS score,
	         me.provider, me.model, me.embedding::text AS embedding_text, m.access_count, coalesce(m.origin, '') AS origin `
	for _, leg := range []string{"vector", "full-text"} {
		var arg2 any = encodePgvector(q)
		if leg == "full-text" {
			arg2 = ftQuery
		}
		cond, args := idSegmentCondition(f, []any{sid, arg2, len(q)})
		var sql string
		if leg == "vector" {
			sql = `EXPLAIN (ANALYZE, BUFFERS) SELECT ` + fmt.Sprintf(cols, `1.0 - (me.embedding <=> $2::vector)`) + base +
				` AND me.dimension = $3` + cond + ` ORDER BY me.embedding <=> $2::vector LIMIT 51`
		} else {
			sql = `EXPLAIN (ANALYZE, BUFFERS) SELECT ` + fmt.Sprintf(cols, `ts_rank(me.embed_text_tsv, plainto_tsquery('english', $2))`) + base +
				` AND me.embed_text_tsv @@ plainto_tsquery('english', $2) AND $3::int > 0` + cond + ` ORDER BY score DESC LIMIT 51`
		}
		rows, err := s.pool.Query(ctx, sql, planPerExecution(args)...)
		if err != nil {
			t.Fatalf("explain %s: %v", leg, err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("explain %s scan: %v", leg, err)
			}
			// A plan names a constant array in full; 20,000 ids would bury the plan.
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			lines = append(lines, line)
		}
		rows.Close()
		t.Logf("EXPLAIN ANALYZE %s leg, %s:\n%s", leg, label, strings.Join(lines, "\n"))
	}
}
