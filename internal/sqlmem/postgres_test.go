package sqlmem

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The postgres tier tests run against a REAL postgres aux database. They are
// skipped unless LOOMCYCLE_TEST_SQLMEM_PG_DSN points at one (a SEPARATE db,
// reached by a non-superuser admin role with CREATE on the db + CREATEROLE).
// Mirrors internal/store/postgres's LOOMCYCLE_TEST_PG_DSN gating; CI runs them
// in the go-postgres job.

func pgTestManager(t *testing.T, cfg Config) (*Manager, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres SQL Memory tests")
	}
	cfg.PgDSN = dsn
	if cfg.StatementTimeoutMS == 0 {
		cfg.StatementTimeoutMS = 30000
	}
	if cfg.MaxRows == 0 {
		cfg.MaxRows = 10000
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw aux: %v", err)
	}
	dropAllScopes(t, raw) // clean slate
	m, err := NewPostgres(context.Background(), cfg)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = m.Close() // close scope pools first so DROP ROLE isn't "role in use"
		dropAllScopes(t, raw)
		_ = raw.Close()
	})
	return m, raw
}

// dropAllScopes removes every sqlmem_* schema and role from the aux DB so a
// test starts and ends clean (durable scopes otherwise persist).
func dropAllScopes(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	// Clear the GC bookkeeping (best-effort — absent when GC was never enabled).
	_, _ = raw.ExecContext(ctx, `TRUNCATE sqlmem_meta.scope_access`)
	schemas, err := raw.QueryContext(ctx, `SELECT nspname FROM pg_namespace WHERE nspname LIKE 'sqlmem\_s\_%'`)
	if err == nil {
		var names []string
		for schemas.Next() {
			var n string
			if schemas.Scan(&n) == nil {
				names = append(names, n)
			}
		}
		schemas.Close()
		for _, n := range names {
			_, _ = raw.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+q(n)+` CASCADE`)
		}
	}
	roles, err := raw.QueryContext(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'sqlmem\_r\_%'`)
	if err == nil {
		var names []string
		for roles.Next() {
			var n string
			if roles.Scan(&n) == nil {
				names = append(names, n)
			}
		}
		roles.Close()
		for _, n := range names {
			// Schemas were dropped above and the role holds no per-scope DB grant,
			// so a plain DROP ROLE suffices.
			_, _ = raw.ExecContext(ctx, `DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname=`+lit(n)+`) THEN DROP ROLE `+q(n)+`; END IF; END $$`)
		}
	}
}

func TestPostgres_RoundTripsWithinOneScope(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")

	if _, err := m.Exec(ctx, key, "CREATE TABLE notes (id SERIAL PRIMARY KEY, body TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := m.Exec(ctx, key, "INSERT INTO notes (body) VALUES ($1)", []any{"hello"}, 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("RowsAffected = %d, want 1", res.RowsAffected)
	}
	q, err := m.Query(ctx, key, "SELECT id, body FROM notes ORDER BY id", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(q.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(q.Rows))
	}
	if body, _ := q.Rows[0][1].(string); body != "hello" {
		t.Fatalf("body = %#v, want hello", q.Rows[0][1])
	}
}

// TestPostgres_CrossScopeQualifiedReadDenied is the isolation crux: the
// validator ALLOWS a fully-qualified cross-schema SELECT (leading SELECT, no
// denied function), so isolation rests on the per-scope role having no USAGE on
// another scope's schema. We derive the victim's real (hashed) schema name and
// inject it into the attacker's query — the engine must refuse it.
func TestPostgres_CrossScopeQualifiedReadDenied(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	victim := agentKey("t1", "victim")
	attacker := agentKey("t1", "attacker")

	if _, err := m.Exec(ctx, victim, "CREATE TABLE secrets (s TEXT)", nil, 0); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if _, err := m.Exec(ctx, victim, "INSERT INTO secrets VALUES ('topsecret')", nil, 0); err != nil {
		t.Fatalf("insert victim: %v", err)
	}
	// Make sure the attacker scope/role exists.
	if _, err := m.Exec(ctx, attacker, "CREATE TABLE own (x INT)", nil, 0); err != nil {
		t.Fatalf("create attacker: %v", err)
	}

	victimSchema, _, err := pgScopeNames(victim)
	if err != nil {
		t.Fatalf("pgScopeNames: %v", err)
	}
	// Fully-qualified cross-schema read — must be refused by the engine
	// (permission denied for schema), NOT return the secret.
	res, err := m.Query(ctx, attacker, "SELECT s FROM "+q(victimSchema)+".secrets", nil)
	if err == nil {
		t.Fatalf("attacker read victim's secrets cross-schema; want a permission error (got %d rows)", len(res.Rows))
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("error = %v; want a 'permission denied for schema' refusal", err)
	}
}

// TestPostgres_SetRoleFunctionPivotDenied is the regression for the CRITICAL
// adversarial finding: a `SET role` function clause (or set_config('role',…) /
// RESET ROLE) that pivots the agent into another scope's role. On the abandoned
// "shared admin + SET LOCAL ROLE" design this returned the victim's secret
// (session_user stayed the admin, a WITH-SET member of every scope role). With
// the agent's session_user = its OWN per-scope role, the pivot is engine-denied:
// the scope role is a member of nothing, so creating the function is refused.
func TestPostgres_SetRoleFunctionPivotDenied(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	victim := agentKey("t1", "pivot-victim")
	attacker := agentKey("t1", "pivot-attacker")

	if _, err := m.Exec(ctx, victim, "CREATE TABLE secrets (s TEXT)", nil, 0); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if _, err := m.Exec(ctx, victim, "INSERT INTO secrets VALUES ('TOPSECRET')", nil, 0); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	if _, err := m.Exec(ctx, attacker, "CREATE TABLE own (x INT)", nil, 0); err != nil {
		t.Fatalf("create attacker: %v", err)
	}
	vSchema, vRole, _ := pgScopeNames(victim)

	// The exploit: a SET-role function clause pivoting into the victim's role.
	pivotFn := fmt.Sprintf(
		`CREATE OR REPLACE FUNCTION pwn() RETURNS text LANGUAGE sql SET role TO %s AS 'SELECT s FROM %s.secrets LIMIT 1'`,
		q(vRole), q(vSchema),
	)
	if _, err := m.Exec(ctx, attacker, pivotFn, nil, 0); err == nil {
		// If creation somehow succeeds, invoking it must NOT yield the secret.
		res, qerr := m.Query(ctx, attacker, "SELECT pwn()", nil)
		if qerr == nil && len(res.Rows) > 0 {
			if got, _ := res.Rows[0][0].(string); got == "TOPSECRET" {
				t.Fatal("CRITICAL: attacker pivoted into the victim scope via a SET-role function and read the secret")
			}
		}
	}
	// set_config / RESET ROLE escalations stay on the attacker's own role.
	if _, err := m.Query(ctx, attacker, "SELECT current_user", nil); err != nil {
		t.Fatalf("baseline query: %v", err)
	}
	// And a direct fully-qualified read of the victim is denied (no USAGE).
	if _, err := m.Query(ctx, attacker, "SELECT s FROM "+q(vSchema)+".secrets", nil); err == nil {
		t.Fatal("attacker read the victim's secrets directly; want permission denied")
	}
}

func TestPostgres_CrossTenantIsolation(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	a := agentKey("tenantA", "shared-name")
	b := agentKey("tenantB", "shared-name")

	if _, err := m.Exec(ctx, a, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := m.Exec(ctx, a, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	// Same agent name, different tenant → different schema → no such table.
	if _, err := m.Query(ctx, b, "SELECT x FROM t", nil); err == nil {
		t.Fatal("tenant B saw tenant A's table; want an error (separate schema)")
	}
	// And the two map to distinct schemas.
	sa, _, _ := pgScopeNames(a)
	sb, _, _ := pgScopeNames(b)
	if sa == sb {
		t.Fatalf("tenant A and B hashed to the same schema %q", sa)
	}
}

// TestPostgres_EngineEscapesDenied exercises the engine-enforced denials
// through the live backend (the per-scope role is non-superuser): COPY PROGRAM,
// a server-side file function, and CREATE EXTENSION must all be refused. (Most
// are also validator-blocked; this proves the role is the floor.)
func TestPostgres_EngineEscapesDenied(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "escaper")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := []struct {
		name     string
		stmt     string
		readOnly bool
	}{
		{"copy program", "COPY t TO PROGRAM 'id'", false},
		{"pg_read_file", "SELECT pg_read_file('/etc/hostname')", true},
		{"create extension", "CREATE EXTENSION dblink", false},
		{"set role", "SET ROLE postgres", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.readOnly {
				_, err = m.Query(ctx, key, tc.stmt, nil)
			} else {
				_, err = m.Exec(ctx, key, tc.stmt, nil, 0)
			}
			if err == nil {
				t.Fatalf("%q was allowed; want a refusal", tc.stmt)
			}
		})
	}
}

func TestPostgres_QuotaRefused(t *testing.T) {
	m, _ := pgTestManager(t, Config{QuotaBytes: 64 * 1024})
	ctx := context.Background()
	key := agentKey("t1", "quota")
	if _, err := m.Exec(ctx, key, "CREATE TABLE big (b TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	blob := strings.Repeat("x", 8*1024)
	var lastErr error
	for i := 0; i < 200; i++ {
		_, lastErr = m.Exec(ctx, key, "INSERT INTO big VALUES ($1)", []any{blob}, 0)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("no write refused; want a quota error after the schema grew")
	}
	if !strings.Contains(lastErr.Error(), "quota") {
		t.Fatalf("error = %v; want a quota refusal", lastErr)
	}
}

func TestPostgres_StatementTimeout(t *testing.T) {
	m, _ := pgTestManager(t, Config{StatementTimeoutMS: 400})
	ctx := context.Background()
	key := agentKey("t1", "slow")
	if _, err := m.Query(ctx, key, "SELECT pg_sleep(3)", nil); err == nil {
		t.Fatal("pg_sleep(3) under a 400ms timeout returned no error; want a timeout")
	}
}

func TestPostgres_MaxRowsTruncates(t *testing.T) {
	m, _ := pgTestManager(t, Config{MaxRows: 3})
	ctx := context.Background()
	key := agentKey("t1", "rows")
	if _, err := m.Exec(ctx, key, "CREATE TABLE n (i INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO n SELECT generate_series(1, 10)", nil, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	q, err := m.Query(ctx, key, "SELECT i FROM n ORDER BY i", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !q.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(q.Rows) != 3 {
		t.Fatalf("rows = %d, want 3 (MaxRows)", len(q.Rows))
	}
}

func TestPostgres_DropRunScope(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	runID := "run-abc-123"
	key := ScopeKey{Scope: "run", ScopeID: runID}

	if _, err := m.Exec(ctx, key, "CREATE TABLE scratch (x INT)", nil, 0); err != nil {
		t.Fatalf("create run scope: %v", err)
	}
	schema, role, _ := pgScopeNames(key)

	removed, err := m.DropRunScope(runID)
	if err != nil {
		t.Fatalf("DropRunScope: %v", err)
	}
	if !removed {
		t.Fatal("DropRunScope removed=false; want true (schema existed)")
	}
	// Schema and role are gone.
	var schemaExists, roleExists bool
	_ = raw.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name=$1)`, schema).Scan(&schemaExists)
	_ = raw.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&roleExists)
	if schemaExists {
		t.Fatal("run schema still exists after DropRunScope")
	}
	if roleExists {
		t.Fatal("run role still exists after DropRunScope")
	}
	// Dropping again is a clean no-op (removed=false).
	if removed, err := m.DropRunScope(runID); err != nil || removed {
		t.Fatalf("second DropRunScope = (%v, %v); want (false, nil)", removed, err)
	}
}

// TestPostgres_TxnAtomicityAndIsolation exercises the postgres explicit-txn
// path: rollback discards, commit persists, the txn pins one pool connection
// while an auto-commit read on the SAME scope uses the other (read-committed:
// the uncommitted row is invisible), and another scope can't see the table at
// all during the open txn.
func TestPostgres_TxnAtomicityAndIsolation(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "pgtxn")
	other := agentKey("t1", "pgtxn-other")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Exec(ctx, other, "CREATE TABLE own (x INT)", nil, 0); err != nil {
		t.Fatalf("create other: %v", err)
	}

	id := BuildTxnID("run1", "agent", "pgtxn")
	if err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("exec in txn: %v", err)
	}
	// Auto-commit read on the SAME scope (the other pool connection) does NOT
	// see the uncommitted row (read-committed).
	res, err := m.Query(ctx, key, "SELECT count(*) FROM t", nil)
	if err != nil {
		t.Fatalf("concurrent read: %v", err)
	}
	if got := pgCount(res); got != 0 {
		t.Fatalf("auto-commit read saw the uncommitted row (count=%d); want 0", got)
	}
	// Another scope cannot reach the table at all (isolation holds during a txn).
	if _, err := m.Query(ctx, other, "SELECT count(*) FROM t", nil); err == nil {
		t.Fatal("other scope read the in-txn scope's table; want a no-such-table / permission error")
	}
	if err := m.RollbackTxn(id); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Commit path persists.
	if err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (2)", nil, 0); err != nil {
		t.Fatalf("exec 2: %v", err)
	}
	if err := m.CommitTxn(id); err != nil {
		t.Fatalf("commit: %v", err)
	}
	res2, err := m.Query(ctx, key, "SELECT count(*) FROM t", nil)
	if err != nil {
		t.Fatalf("post-commit read: %v", err)
	}
	if got := pgCount(res2); got != 1 {
		t.Fatalf("after rollback+commit count=%d, want 1", got)
	}
}

// pgCount reads a count(*) result, which pgx returns as int64.
func pgCount(res *QueryResult) int64 {
	switch v := res.Rows[0][0].(type) {
	case int64:
		return v
	case string:
		var n int64
		for _, c := range v {
			n = n*10 + int64(c-'0')
		}
		return n
	default:
		return -1
	}
}

// TestPostgres_GCSweep: a durable scope whose meta last_used is older than the
// cutoff is dropped (schema + role + meta row gone); a fresh durable scope
// survives. Exercises the postgres GC path (the meta table + dropScopePG).
func TestPostgres_GCSweep(t *testing.T) {
	m, raw := pgTestManager(t, Config{ScopeTTLMS: 3600_000}) // GC on → meta table provisioned
	ctx := context.Background()
	stale := agentKey("t1", "gc-stale")
	fresh := agentKey("t1", "gc-fresh")
	for _, k := range []ScopeKey{stale, fresh} {
		if _, err := m.Exec(ctx, k, "CREATE TABLE t (x INT)", nil, 0); err != nil {
			t.Fatalf("create %s: %v", k.ScopeID, err)
		}
	}
	staleSchema, staleRole, _ := pgScopeNames(stale)
	freshSchema, _, _ := pgScopeNames(fresh)
	if _, err := raw.ExecContext(ctx,
		`UPDATE sqlmem_meta.scope_access SET last_used = now() - interval '2 hours' WHERE schema_name = $1`, staleSchema); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	dropped, err := m.backend.sweepStale(time.Now().Add(-1 * time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped=%d, want 1", dropped)
	}
	exists := func(q, arg string) bool {
		var e bool
		_ = raw.QueryRowContext(ctx, q, arg).Scan(&e)
		return e
	}
	if exists(`SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name=$1)`, staleSchema) {
		t.Fatal("stale schema still exists after GC")
	}
	if exists(`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, staleRole) {
		t.Fatal("stale role still exists after GC")
	}
	if exists(`SELECT EXISTS(SELECT 1 FROM sqlmem_meta.scope_access WHERE schema_name=$1)`, staleSchema) {
		t.Fatal("stale meta row still exists after GC")
	}
	if !exists(`SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name=$1)`, freshSchema) {
		t.Fatal("fresh schema was dropped by GC")
	}
}

// TestPostgres_VectorColumn verifies the Phase-3c provisioning + capability: a
// scope role (with sqlmem_ext baked onto its search_path) can CREATE a vector
// column, an HNSW index, and run a cosine KNN — and another scope can't see it.
// Skips when pgvector isn't installed in the test aux DB.
func TestPostgres_VectorColumn(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	if !m.VectorsEnabled() {
		t.Skip("pgvector not installed in the sqlmem_ext schema of the test aux DB")
	}
	ctx := context.Background()
	key := agentKey("t1", "vec")
	other := agentKey("t1", "vec-other")
	if _, err := m.Exec(ctx, key, "CREATE TABLE docs (id int, embedding vector(3))", nil, 0); err != nil {
		t.Fatalf("create vector table: %v", err)
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO docs VALUES (1,'[1,0,0]'),(2,'[0,1,0]'),(3,'[0.9,0.1,0]')", nil, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := m.Exec(ctx, key, "CREATE INDEX ON docs USING hnsw (embedding vector_cosine_ops)", nil, 0); err != nil {
		t.Fatalf("hnsw index: %v", err)
	}
	res, err := m.Query(ctx, key, "SELECT id FROM docs ORDER BY embedding <=> '[1,0,0]'::vector LIMIT 1", nil)
	if err != nil {
		t.Fatalf("knn query: %v", err)
	}
	if got, _ := res.Rows[0][0].(int64); got != 1 {
		t.Fatalf("nearest id = %v, want 1", res.Rows[0][0])
	}
	// Isolation holds with vectors: another scope can't see the table.
	if _, err := m.Exec(ctx, other, "CREATE TABLE own (x int)", nil, 0); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := m.Query(ctx, other, "SELECT id FROM docs", nil); err == nil {
		t.Fatal("other scope saw the vector table; want a no-such-table error")
	}
}

// TestPostgres_QueryTxnSelectIntoDenied regresses the security finding: an
// explicit transaction is read-WRITE, so sql_query inside it loses the
// auto-commit read-only-transaction backstop — a SELECT … INTO (which creates a
// table) must be refused by the validator instead.
func TestPostgres_QueryTxnSelectIntoDenied(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "into")
	if _, err := m.Exec(ctx, key, "CREATE TABLE src (x INT)", nil, 0); err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO src VALUES (1)", nil, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := BuildTxnID("run1", "agent", "into")
	if err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.QueryTxn(ctx, id, "SELECT x INTO sneaky FROM src", nil); err == nil {
		t.Fatal("SELECT … INTO via QueryTxn was allowed; want a validator refusal")
	}
	if err := m.CommitTxn(id); err != nil { // commit: if INTO had run, the table would persist
		t.Fatalf("commit: %v", err)
	}
	if _, err := m.Query(ctx, key, "SELECT 1 FROM sneaky LIMIT 1", nil); err == nil {
		t.Fatal("SELECT … INTO created the table despite the refusal")
	}
}

// TestPostgres_ConcurrentScopeChurnEviction touches MORE distinct scopes than
// the connection-pool LRU cap, concurrently, so eviction fires while other
// pools are mid-op — exercising retire-while-in-use (the last releaseScope
// finalizes) and identity-based release. Run under -race it catches a regressed
// refcount.
func TestPostgres_ConcurrentScopeChurnEviction(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	const n = 50 // > pgScopeConnLRU (32)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			key := agentKey("t1", fmt.Sprintf("churn-%d", k))
			if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
				errs <- err
				return
			}
			if _, err := m.Exec(ctx, key, "INSERT INTO t VALUES ($1)", []any{k}, 0); err != nil {
				errs <- err
				return
			}
			if _, err := m.Query(ctx, key, "SELECT x FROM t", nil); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("churn op error: %v", err)
	}
}

// TestPostgres_ConcurrentFirstTouchSameScope hammers a brand-new scope from
// many goroutines at once — the provisioning DDL must be race-safe (no
// duplicate-object error escapes).
func TestPostgres_ConcurrentFirstTouchSameScope(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "concurrent")
	if _, err := m.Exec(ctx, key, "CREATE TABLE c (x INT)", nil, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := m.Exec(ctx, key, "INSERT INTO c VALUES ($1)", []any{n}, 0); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent insert error: %v", err)
	}
}
