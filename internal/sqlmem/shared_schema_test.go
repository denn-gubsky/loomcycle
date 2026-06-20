package sqlmem

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// shared_schema_test.go — RFC AA Phase 3g: operator-defined read-only shared
// schemas (postgres tier). Gated on LOOMCYCLE_TEST_SQLMEM_PG_DSN.

// sharedSchemaManager sets up a read-only shared schema `refdata` (loaded +
// granted SELECT to PUBLIC, the operator recipe) and returns a Manager that
// exposes it via SharedSchemas. The shared schema is created BEFORE NewPostgres
// so the boot probe finds it.
func sharedSchemaManager(t *testing.T) (*Manager, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres SQL Memory tests")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw aux: %v", err)
	}
	dropAllScopes(t, raw)
	ctx := context.Background()
	for _, s := range []string{
		`DROP SCHEMA IF EXISTS refdata CASCADE`,
		`CREATE SCHEMA refdata`,
		`CREATE TABLE refdata.countries (code text PRIMARY KEY, name text)`,
		`INSERT INTO refdata.countries VALUES ('US','United States'),('UA','Ukraine')`,
		`GRANT USAGE ON SCHEMA refdata TO PUBLIC`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA refdata TO PUBLIC`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA refdata GRANT SELECT ON TABLES TO PUBLIC`,
	} {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			_ = raw.Close()
			t.Fatalf("shared-schema setup %q: %v", s, err)
		}
	}
	m, err := NewPostgres(ctx, Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 10000, SharedSchemas: []string{"refdata"}})
	if err != nil {
		_ = raw.Close()
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = m.Close()
		dropAllScopes(t, raw)
		_, _ = raw.ExecContext(ctx, `DROP SCHEMA IF EXISTS refdata CASCADE`)
		_ = raw.Close()
	})
	return m, raw
}

// TestSharedSchema_ReadOnly: a scope can SELECT the shared table (unqualified via
// search_path AND schema-qualified), but every write is engine-denied.
func TestSharedSchema_ReadOnly(t *testing.T) {
	m, _ := sharedSchemaManager(t)
	ctx := context.Background()
	key := agentKey("t1", "reader")

	res, err := m.Query(ctx, key, "SELECT name FROM countries WHERE code='US'", nil) // unqualified via search_path
	if err != nil {
		t.Fatalf("unqualified read of shared table: %v", err)
	}
	if got, _ := res.Rows[0][0].(string); got != "United States" {
		t.Fatalf("unqualified read = %q, want United States", got)
	}
	res, err = m.Query(ctx, key, "SELECT name FROM refdata.countries WHERE code='UA'", nil) // qualified
	if err != nil {
		t.Fatalf("qualified read of shared table: %v", err)
	}
	if got, _ := res.Rows[0][0].(string); got != "Ukraine" {
		t.Fatalf("qualified read = %q, want Ukraine", got)
	}

	if _, err := m.Exec(ctx, key, "INSERT INTO refdata.countries VALUES ('FR','France')", nil, 0); err == nil {
		t.Fatal("INSERT into shared schema was allowed — read-only must be engine-denied")
	}
	if _, err := m.Exec(ctx, key, "UPDATE refdata.countries SET name='x' WHERE code='US'", nil, 0); err == nil {
		t.Fatal("UPDATE of shared schema was allowed — read-only must be engine-denied")
	}
}

// TestSharedSchema_Isolation: read access to the shared schema does NOT widen
// cross-scope isolation — another scope still cannot read this scope's schema.
func TestSharedSchema_Isolation(t *testing.T) {
	m, _ := sharedSchemaManager(t)
	ctx := context.Background()
	a := agentKey("t1", "agent-a")
	b := agentKey("t1", "agent-b")

	if _, err := m.Exec(ctx, a, "CREATE TABLE secrets (x text)", nil, 0); err != nil {
		t.Fatalf("create a.secrets: %v", err)
	}
	if _, err := m.Exec(ctx, a, "INSERT INTO secrets VALUES ('topsecret')", nil, 0); err != nil {
		t.Fatalf("insert a.secrets: %v", err)
	}
	// b reads the shared schema fine...
	if _, err := m.Query(ctx, b, "SELECT count(*) FROM countries", nil); err != nil {
		t.Fatalf("agent-b read shared: %v", err)
	}
	// ...but still cannot reach agent-a's scope schema.
	schemaA, _, err := pgScopeNames(a)
	if err != nil {
		t.Fatalf("pgScopeNames: %v", err)
	}
	if _, err := m.Query(ctx, b, "SELECT x FROM "+q(schemaA)+".secrets", nil); err == nil {
		t.Fatal("agent-b read agent-a's schema — shared access wrongly widened cross-scope isolation")
	}
}

// TestSharedSchema_Shadowing: a scope's own table shadows a same-named shared
// table for unqualified refs; the shared one stays reachable when qualified.
func TestSharedSchema_Shadowing(t *testing.T) {
	m, _ := sharedSchemaManager(t)
	ctx := context.Background()
	key := agentKey("t1", "shadow")

	if _, err := m.Exec(ctx, key, "CREATE TABLE countries (code text, name text)", nil, 0); err != nil {
		t.Fatalf("create own countries: %v", err)
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO countries VALUES ('XX','Scope Local')", nil, 0); err != nil {
		t.Fatalf("insert own: %v", err)
	}
	res, err := m.Query(ctx, key, "SELECT name FROM countries", nil) // unqualified → the scope's own
	if err != nil {
		t.Fatalf("unqualified read: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("unqualified countries returned %d rows, want 1 (the scope's own, not shared)", len(res.Rows))
	}
	if got, _ := res.Rows[0][0].(string); got != "Scope Local" {
		t.Fatalf("unqualified countries = %q, want the scope's own (Scope Local)", got)
	}
	res, err = m.Query(ctx, key, "SELECT name FROM refdata.countries WHERE code='US'", nil) // qualified → shared
	if err != nil {
		t.Fatalf("qualified shared read after shadowing: %v", err)
	}
	if got, _ := res.Rows[0][0].(string); got != "United States" {
		t.Fatalf("qualified shared = %q, want United States", got)
	}
}

// TestSharedSchema_MisconfigSkipped: invalid / nonexistent / reserved shared
// schema names are dropped at construction (boot warning); the runtime starts and
// scopes provision without them.
func TestSharedSchema_MisconfigSkipped(t *testing.T) {
	m, _ := pgTestManager(t, Config{SharedSchemas: []string{"does_not_exist", "Bad-Name!", "sqlmem_meta", "pg_catalog"}})
	ctx := context.Background()
	pb, ok := m.backend.(*postgresBackend)
	if !ok {
		t.Fatalf("backend is not postgres (%T)", m.backend)
	}
	if len(pb.sharedSchemas) != 0 {
		t.Fatalf("invalid/missing/reserved shared schemas were not all skipped: %v", pb.sharedSchemas)
	}
	// A scope still provisions + works despite the misconfig.
	if _, err := m.Exec(ctx, agentKey("t1", "ok"), "CREATE TABLE t (x int)", nil, 0); err != nil {
		t.Fatalf("scope op after shared-schema misconfig: %v", err)
	}
}
