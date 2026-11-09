package sqlmem

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// The engine-side value limit on an agent's own SQL. The result byte cap
// measures a row only once the driver has handed it over, so before it a
// SELECT zeroblob(400000000) made the engine build 400 MB and the driver copy
// it into the Go heap, only for the row to be refused. An agent's statement
// now runs with SQLITE_LIMIT_LENGTH at twice the result cap: the engine refuses
// the value before building it.

// allocatedDuring reports the Go heap bytes handed out while fn ran
// (cumulative, so an upper bound on the peak).
func allocatedDuring(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

const testValueLimit = 2 * DefaultMaxResultBytes

func wantTooBig(t *testing.T, label string, fn func() error) {
	t.Helper()
	var err error
	got := allocatedDuring(func() { err = fn() })
	if err == nil {
		t.Fatalf("%s: no error, want the value refused", label)
	}
	if got > 32<<20 {
		t.Fatalf("%s: refusing it allocated %d MiB, want at most 32 MiB (err: %v)", label, got>>20, err)
	}
	var es *ErrStatement
	if !errors.As(err, &es) && !strings.Contains(err.Error(), "too big") {
		t.Fatalf("%s: err = %v, want the validator's refusal or the engine's \"too big\"", label, err)
	}
	t.Logf("%s: refused after %d MiB: %v", label, got>>20, err)
}

func TestSQLite_AgentValueOverLengthLimitRefusedUnbuilt(t *testing.T) {
	m := newTestManager(t, Config{MaxRows: 10000})
	ctx := context.Background()
	key := agentKey("t1", "a1")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (b BLOB)", nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		label string
		run   func() error
	}{
		{"zeroblob literal", func() error {
			_, err := m.QueryCapped(ctx, key, "SELECT zeroblob(400000000)", nil)
			return err
		}},
		{"zeroblob expression", func() error {
			_, err := m.QueryCapped(ctx, key, "SELECT zeroblob(4*100000000)", nil)
			return err
		}},
		{"randomblob bound", func() error {
			_, err := m.QueryCapped(ctx, key, "SELECT randomblob(?)", []any{400000000})
			return err
		}},
		{"printf width", func() error {
			_, err := m.QueryCapped(ctx, key, "SELECT printf('%.*c', 2*200000000, 'x')", nil)
			return err
		}},
		{"randomblob write", func() error {
			_, err := m.Exec(ctx, key, "INSERT INTO t (b) VALUES (randomblob(4*100000000))", nil, 0)
			return err
		}},
	} {
		wantTooBig(t, c.label, c.run)
	}

	// Inside an explicit transaction too.
	if _, err := m.BeginTxn(ctx, "txn-1", "root-1", key); err != nil {
		t.Fatal(err)
	}
	wantTooBig(t, "zeroblob in a transaction", func() error {
		_, err := m.QueryTxnCapped(ctx, "txn-1", "SELECT zeroblob(4*100000000)", nil)
		return err
	})
	wantTooBig(t, "randomblob write in a transaction", func() error {
		_, err := m.ExecTxn(ctx, "txn-1", "INSERT INTO t (b) VALUES (randomblob(4*100000000))", nil, 0)
		return err
	})
	if _, err := m.RollbackTxn("txn-1"); err != nil {
		t.Fatal(err)
	}

	// A value under the limit is still built.
	res, err := m.QueryCapped(ctx, key, "SELECT length(zeroblob(1000000))", nil)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("a 1 MB value under the limit: %v %v", res, err)
	}
}

func TestSQLite_DocPlaneLimitIsRestoredForRuntimeReads(t *testing.T) {
	// The document file's one connection serves both the agent's query_chunks
	// sql and the runtime's own statements, some of which read one large value
	// (an image chunk's bytes): the limit holds for the agent's statement only.
	m := newTestManager(t, Config{MaxRows: 10000})
	doc := m.DocPlane()
	ctx := context.Background()
	key := agentKey("t1", "a1")
	if _, err := doc.Exec(ctx, key, "CREATE TABLE big (b BLOB)", nil, 0); err != nil {
		t.Fatal(err)
	}
	value := make([]byte, testValueLimit+(4<<20))
	if _, err := doc.Exec(ctx, key, "INSERT INTO big (b) VALUES (?)", []any{value}, 0); err != nil {
		t.Fatalf("the runtime's own write of a value over the agent limit: %v", err)
	}
	wantTooBig(t, "query_chunks sql zeroblob", func() error {
		_, err := doc.QueryCapped(ctx, key, "SELECT zeroblob(4*100000000)", nil)
		return err
	})
	res, err := doc.Query(ctx, key, "SELECT b FROM big", nil)
	if err != nil {
		t.Fatalf("the runtime's read after an agent statement: %v", err)
	}
	var n int
	switch v := res.Rows[0][0].(type) {
	case []byte:
		n = len(v)
	case string:
		n = len(v)
	}
	if n != len(value) {
		t.Fatalf("read back %T of %d bytes, want %d", res.Rows[0][0], n, len(value))
	}
}

func TestValidateWidths_RefusesLiteralWidthsOverTheLimit(t *testing.T) {
	const limit = 16 << 20
	for _, c := range []struct {
		stmt    string
		d       dialect
		refused bool
	}{
		{"SELECT zeroblob(400000000)", dialectSQLite, true},
		{"SELECT hex(ZEROBLOB ( 400000000 ))", dialectSQLite, true},
		{"SELECT randomblob(0x20000000)", dialectSQLite, true},
		{"SELECT printf('%400000000d', 1)", dialectSQLite, true},
		{"SELECT format('%.*c', 400000000, 'x')", dialectSQLite, true},
		{"SELECT zeroblob(1000)", dialectSQLite, false},
		{"SELECT printf('%5d of %d', 1, 400000000)", dialectSQLite, false},
		{"SELECT 'zeroblob(400000000)'", dialectSQLite, false},
		{"SELECT zeroblob(n) FROM t", dialectSQLite, false},
		{"SELECT repeat('x', 400000000)", dialectPostgres, true},
		{"SELECT lpad('x', 400000000)", dialectPostgres, true},
		{"SELECT rpad('x', 400000000, 'y')", dialectPostgres, true},
		{"SELECT repeat('x', 10)", dialectPostgres, false},
	} {
		err := validateWidths(c.stmt, c.d, limit)
		if got := err != nil; got != c.refused {
			t.Errorf("%q: refused=%v (%v), want %v", c.stmt, got, err, c.refused)
		}
	}
}

func TestPostgres_ScopeRoleCarriesMemoryBounds(t *testing.T) {
	// A sort or hash an agent's query runs is held to work_mem in RAM and to
	// temp_file_limit on disk, both baked onto the scope's login role.
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "a1")
	if _, err := m.QueryCapped(ctx, key, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	_, role, err := pgScopeNames(key)
	if err != nil {
		t.Fatal(err)
	}
	var cfg string
	if err := raw.QueryRowContext(ctx, `SELECT array_to_string(setconfig, ',') FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole WHERE r.rolname = $1`, role).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "work_mem="+pgScopeWorkMem) {
		t.Fatalf("role settings %q, want work_mem=%s", cfg, pgScopeWorkMem)
	}
	// temp_file_limit needs a superuser, or SET granted on it, to bake onto a
	// role; the CI admin has neither, so its absence is reported, not fatal.
	if !strings.Contains(cfg, "temp_file_limit="+pgScopeTempFileLimit) {
		t.Logf("temp_file_limit not on the role (the admin may not set it): %q", cfg)
	} else {
		res, err := m.QueryCapped(ctx, key, "SELECT current_setting('temp_file_limit')", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Rows[0][0]; got != pgScopeTempFileLimit {
			t.Fatalf("an agent's session runs with temp_file_limit %v, want %s", got, pgScopeTempFileLimit)
		}
	}
	res, err := m.QueryCapped(ctx, key, "SELECT current_setting('work_mem')", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != pgScopeWorkMem {
		t.Fatalf("an agent's session runs with work_mem %v, want %s", got, pgScopeWorkMem)
	}
}

func TestValidatePostgres_RefusesSetConfig(t *testing.T) {
	for _, stmt := range []string{
		"SELECT set_config('work_mem', '64GB', false)",
		`SELECT "set_config"('work_mem', '64GB', true)`,
		"SELECT pg_catalog.set_config('work_mem','1TB',false)",
	} {
		if err := validateStatementForDialect(stmt, true, dialectPostgres); err == nil {
			t.Errorf("%q: accepted, want refused", stmt)
		}
	}
	if err := validateStatementForDialect("SELECT 'set_config(x)' AS note", true, dialectPostgres); err != nil {
		t.Errorf("a literal naming set_config was refused: %v", err)
	}
}
