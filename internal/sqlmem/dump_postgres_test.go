package sqlmem

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

// dump_postgres_test.go — RFC AA Phase 3e postgres-tier snapshot dump tests.
// Gated on LOOMCYCLE_TEST_SQLMEM_PG_DSN (CI's go-postgres job); skipped locally
// without a real aux DB.

// freshPgManager opens a SECOND manager against the same aux DB with an empty
// provisioning cache — a true "fresh host" restore target (provision recreates a
// dropped schema). Closed before the pgTestManager cleanup drops scopes.
func freshPgManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	cfg.PgDSN = dsn
	if cfg.StatementTimeoutMS == 0 {
		cfg.StatementTimeoutMS = 30000
	}
	if cfg.MaxRows == 0 {
		cfg.MaxRows = 10000
	}
	m, err := NewPostgres(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPostgres (fresh): %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// dropScopeSchema drops a durable scope's schema underneath a manager (the role
// is left for the restore to reuse), simulating a host where the scope is gone.
func dropScopeSchema(t *testing.T, raw *sql.DB, key ScopeKey) {
	t.Helper()
	schema, _, err := pgScopeNames(key)
	if err != nil {
		t.Fatalf("pgScopeNames: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `DROP SCHEMA `+q(schema)+` CASCADE`); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
}

func TestTier_Postgres(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	if got := m.Tier(); got != "postgres" {
		t.Fatalf("Tier() = %q, want postgres", got)
	}
}

// TestListScopes_Postgres: durable scopes come back from the registry; the run
// scope (registered? no — never) is excluded.
func TestListScopes_Postgres(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	mk := func(k ScopeKey) {
		if _, err := m.Exec(ctx, k, "CREATE TABLE t (id int)", nil, 0); err != nil {
			t.Fatalf("create %v: %v", k, err)
		}
	}
	mk(ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: "a1"})
	mk(ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"})
	mk(ScopeKey{Scope: "run", ScopeID: "run-1"})

	scopes, err := m.ListScopes(ctx)
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	got := map[ScopeKey]bool{}
	for _, s := range scopes {
		if s.Scope == "run" {
			t.Fatalf("ListScopes returned a run scope: %v", s)
		}
		got[s] = true
	}
	for _, want := range []ScopeKey{
		{Tenant: "t1", Scope: "agent", ScopeID: "a1"},
		{Tenant: "t1", Scope: "user", ScopeID: "u1"},
	} {
		if !got[want] {
			t.Fatalf("ListScopes missing %v (got %v)", want, scopes)
		}
	}
}

// TestDump_PostgresRoundTrip: bigserial + text + jsonb + timestamptz + a unique
// constraint + an index + a NULL round-trip through Export → drop → fresh
// Restore. The serial counter (setval) continues past the restored ids.
func TestDump_PostgresRoundTrip(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")

	for _, s := range []string{
		`CREATE TABLE docs (id bigserial PRIMARY KEY, title text NOT NULL, meta jsonb, created timestamptz, UNIQUE (title))`,
		`CREATE INDEX idx_docs_created ON docs (created)`,
	} {
		if _, err := m.Exec(ctx, key, s, nil, 0); err != nil {
			t.Fatalf("ddl %q: %v", s, err)
		}
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO docs (title, meta, created) VALUES ($1, $2::jsonb, now())`, []any{"first", `{"a": 1}`}, 0); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO docs (title, meta, created) VALUES ($1, NULL, now())`, []any{"second"}, 0); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	dump, err := m.ExportScope(ctx, key)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	// Drop the scope schema; restore through a fresh manager (empty cache).
	dropScopeSchema(t, raw, key)
	fresh := freshPgManager(t, Config{})
	if err := fresh.RestoreScope(ctx, key, dump); err != nil {
		t.Fatalf("RestoreScope: %v", err)
	}

	res, err := fresh.Query(ctx, key, `SELECT id, title, meta, created FROM docs ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("Query restored: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("restored %d rows, want 2", len(res.Rows))
	}
	if got, _ := res.Rows[0][1].(string); got != "first" {
		t.Fatalf("row0 title = %q, want first", got)
	}
	if got, _ := res.Rows[0][2].(string); !strings.Contains(got, `"a"`) {
		t.Fatalf("row0 meta = %q, want jsonb containing \"a\"", got)
	}
	if res.Rows[1][2] != nil {
		t.Fatalf("row1 meta = %v, want NULL", res.Rows[1][2])
	}
	if res.Rows[0][3] == nil {
		t.Fatalf("row0 created is NULL, want a timestamp")
	}
	// The unique constraint survived: a duplicate title is rejected.
	if _, err := fresh.Exec(ctx, key, `INSERT INTO docs (title) VALUES ('first')`, nil, 0); err == nil {
		t.Fatal("duplicate title accepted — UNIQUE constraint lost on restore")
	}
	// The bigserial counter continues past the restored max id (setval restored).
	if _, err := fresh.Exec(ctx, key, `INSERT INTO docs (title) VALUES ('third')`, nil, 0); err != nil {
		t.Fatalf("post-restore insert: %v", err)
	}
	idRes, err := fresh.Query(ctx, key, `SELECT id FROM docs WHERE title='third'`, nil)
	if err != nil {
		t.Fatalf("read new id: %v", err)
	}
	if got, _ := idRes.Rows[0][0].(int64); got <= 2 {
		t.Fatalf("post-restore serial id = %d, want > 2 (setval did not restore the counter)", got)
	}
}

// TestDump_PostgresVectorRoundTrip: a pgvector column round-trips via the
// ::text/::vector bridge. Skipped when pgvector isn't installed.
func TestDump_PostgresVectorRoundTrip(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	if !m.VectorsEnabled() {
		t.Skip("pgvector not installed in the sqlmem_ext schema of the test aux DB")
	}
	ctx := context.Background()
	key := agentKey("t1", "vec")
	if _, err := m.Exec(ctx, key, `CREATE TABLE emb (id int PRIMARY KEY, v vector(3))`, nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO emb VALUES (1,'[1,0,0]'),(2,'[0,1,0]')`, nil, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	dump, err := m.ExportScope(ctx, key)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	dropScopeSchema(t, raw, key)
	fresh := freshPgManager(t, Config{})
	if err := fresh.RestoreScope(ctx, key, dump); err != nil {
		t.Fatalf("RestoreScope: %v", err)
	}
	// The vector column is queryable with a distance operator after restore.
	res, err := fresh.Query(ctx, key, `SELECT id FROM emb ORDER BY v <-> '[1,0,0]'::vector LIMIT 1`, nil)
	if err != nil {
		t.Fatalf("knn after restore: %v", err)
	}
	if got, _ := res.Rows[0][0].(int64); got != 1 {
		t.Fatalf("nearest id = %d, want 1", got)
	}
}

// TestRestoreScope_PostgresIdempotent: restoring twice does not duplicate rows.
func TestRestoreScope_PostgresIdempotent(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "w")
	if _, err := m.Exec(ctx, key, `CREATE TABLE t (id int PRIMARY KEY, v text)`, nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := m.Exec(ctx, key, `INSERT INTO t VALUES ($1,$2)`, []any{i, "row"}, 0); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	dump, err := m.ExportScope(ctx, key)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	dropScopeSchema(t, raw, key)
	fresh := freshPgManager(t, Config{})
	for i := 0; i < 2; i++ {
		if err := fresh.RestoreScope(ctx, key, dump); err != nil {
			t.Fatalf("RestoreScope #%d: %v", i, err)
		}
	}
	res, err := fresh.Query(ctx, key, `SELECT count(*) FROM t`, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got, _ := res.Rows[0][0].(int64); got != 3 {
		t.Fatalf("after double restore count = %d, want 3 (idempotent)", got)
	}
}
