package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestEmbedDirective(t *testing.T) {
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		{map[string]any{"$embed": "hello"}, "hello", true},
		{map[string]any{"$embed": ""}, "", true}, // shape valid; emptiness handled by resolveEmbedArgs
		{map[string]any{"embed": "x"}, "", false},
		{map[string]any{"$embed": "a", "b": float64(1)}, "", false}, // not single-key
		{"plain", "", false},
		{map[string]any{"$embed": float64(42)}, "", false}, // non-string
	}
	for i, c := range cases {
		got, ok := embedDirective(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("case %d: embedDirective(%v) = (%q,%v); want (%q,%v)", i, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestEncodePgvector(t *testing.T) {
	if got := encodePgvector([]float32{1, 0, 0.5}); got != "[1,0,0.5]" {
		t.Fatalf("encodePgvector = %q, want [1,0,0.5]", got)
	}
}

// TestResolveEmbedArgs_Refusals: $embed needs an embedder AND a vector-capable
// tier. On the sqlite fixture (VectorsEnabled=false) it must refuse, with no
// embedder it must refuse, and a no-directive args list passes through.
func TestResolveEmbedArgs_Refusals(t *testing.T) {
	tool, _, ctx, cleanup := sqlMemoryFixture(t) // sqlite → not vector-capable
	defer cleanup()

	emb := newFakeEmbedder("fake", "fake-1", "hi")
	tool.Embedder = emb
	if _, err := tool.resolveEmbedArgs(ctx, []any{map[string]any{"$embed": "hi"}}); err == nil || !strings.Contains(err.Error(), "vector columns") {
		t.Fatalf("sqlite tier should refuse $embed; got %v", err)
	}

	tool.Embedder = nil
	if _, err := tool.resolveEmbedArgs(ctx, []any{map[string]any{"$embed": "hi"}}); err == nil || !strings.Contains(err.Error(), "embedder") {
		t.Fatalf("no embedder should refuse $embed; got %v", err)
	}

	out, err := tool.resolveEmbedArgs(ctx, []any{"plain", float64(42)})
	if err != nil || len(out) != 2 || out[0] != "plain" {
		t.Fatalf("no-directive passthrough failed: out=%v err=%v", out, err)
	}
}

// TestMemorySQL_EmbedDirectiveRoundTrip drives the full Phase-3c path through the
// tool: CREATE a vector table, INSERT with {"$embed": text}, then a KNN query
// with {"$embed": text} returns the doc embedded from the SAME text first (the
// fake embedder is deterministic, so identical text → identical vector →
// distance 0). Gated on a pgvector-enabled aux DB.
func TestMemorySQL_EmbedDirectiveRoundTrip(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the vector round-trip")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	if !mgr.VectorsEnabled() {
		_ = mgr.Close()
		_ = raw.Close()
		t.Skip("pgvector not installed in the sqlmem_ext schema of the test aux DB")
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})

	emb := newFakeEmbedder("fake", "fake-1", "apple", "zebra")
	s, _ := sqlite.Open(":memory:")
	defer s.Close()
	tool := &Memory{Store: s, SqlMem: mgr, Embedder: emb}
	ctx := tools.WithAgentName(context.Background(), "vec-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t", AgentID: "a", RootRunID: "run-v"})
	ctx = tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"agent"}})

	exec := func(payload string) {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(payload))
		if err != nil || res.IsError {
			t.Fatalf("%s -> err=%v %s", payload, err, res.Text)
		}
	}
	exec(`{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE docs (id int, body text, embedding vector(2))"}`)
	exec(`{"op":"sql_exec","scope":"agent","statement":"INSERT INTO docs (id, body, embedding) VALUES (1, $1, $2::vector)","args":["apple",{"$embed":"apple"}]}`)
	exec(`{"op":"sql_exec","scope":"agent","statement":"INSERT INTO docs (id, body, embedding) VALUES (2, $1, $2::vector)","args":["zebra",{"$embed":"zebra"}]}`)

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"agent","statement":"SELECT body FROM docs ORDER BY embedding <=> $1::vector LIMIT 1","args":[{"$embed":"apple"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("knn query: err=%v %s", err, res.Text)
	}
	var out struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Text)
	}
	if got, _ := out.Rows[0][0].(string); got != "apple" {
		t.Fatalf("nearest body = %v, want apple (same-text embedding should be closest)", out.Rows[0][0])
	}
	if strings.Contains(res.Text, "$embed") {
		t.Fatal("$embed directive leaked into the query result")
	}
}

// dropAllSqlmemScopes removes every sqlmem_* scope schema + role (test cleanup;
// leaves sqlmem_ext + its pgvector install alone).
func dropAllSqlmemScopes(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if rows, err := raw.QueryContext(ctx, `SELECT nspname FROM pg_namespace WHERE nspname LIKE 'sqlmem\_s\_%'`); err == nil {
		var names []string
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				names = append(names, n)
			}
		}
		rows.Close()
		for _, n := range names {
			_, _ = raw.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+n+`" CASCADE`)
		}
	}
	if rr, err := raw.QueryContext(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'sqlmem\_r\_%'`); err == nil {
		var names []string
		for rr.Next() {
			var n string
			if rr.Scan(&n) == nil {
				names = append(names, n)
			}
		}
		rr.Close()
		for _, n := range names {
			_, _ = raw.ExecContext(ctx, `DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname='`+n+`') THEN DROP ROLE "`+n+`"; END IF; END $$`)
		}
	}
}
