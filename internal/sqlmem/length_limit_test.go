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

func TestValidateWidths_FoldsConstantsAndRefusesComputedPostgresWidths(t *testing.T) {
	const limit = 16 << 20
	for _, c := range []struct {
		stmt    string
		d       dialect
		refused bool
	}{
		// Postgres has no engine limit on a value: a width it cannot see as a
		// constant is refused, and a constant is folded first.
		{"SELECT repeat('x', (400000000))", dialectPostgres, true},
		{"SELECT repeat('x', 4e8::int)", dialectPostgres, true},
		{"SELECT repeat('x', CAST(4e8 AS integer))", dialectPostgres, true},
		{"SELECT lpad('', 200000*2000)", dialectPostgres, true},
		{"SELECT rpad('', -(-400000000))", dialectPostgres, true},
		{"SELECT repeat('x', n) FROM t", dialectPostgres, true},
		{"SELECT repeat('x', (SELECT max(n) FROM t))", dialectPostgres, true},
		{"SELECT repeat('x', $1)", dialectPostgres, true},
		{"SELECT repeat('-', 2*(3+4)), lpad(id::text, 6, '0') FROM t", dialectPostgres, false},
		// On SQLite the engine limit holds whatever the statement computes, so
		// only a constant is refused here, folded the same way.
		{"SELECT zeroblob((400000000))", dialectSQLite, true},
		{"SELECT randomblob(20000*20000)", dialectSQLite, true},
		{"SELECT zeroblob(CAST(4e8 AS INTEGER))", dialectSQLite, true},
		{"SELECT zeroblob(n) FROM t", dialectSQLite, false},
	} {
		err := validateWidths(c.stmt, c.d, limit)
		if got := err != nil; got != c.refused {
			t.Errorf("%q: refused=%v (%v), want %v", c.stmt, got, err, c.refused)
		}
	}
}

func TestValidatePostgres_RefusesSettingBypasses(t *testing.T) {
	for _, stmt := range []string{
		`CREATE FUNCTION f() RETURNS int LANGUAGE sql SET work_mem = '64GB' AS $$ SELECT 1 $$`,
		`CREATE OR REPLACE PROCEDURE p() LANGUAGE sql SET work_mem TO '64GB' AS 'SELECT 1'`,
		`ALTER FUNCTION f() SET work_mem = '64GB'`,
		`ALTER ROUTINE f() SET work_mem = '64GB'`,
		`SELECT U&"set_config"('work_mem', '64GB', false)`,
		`SELECT u&"\0073et_config"('work_mem', '64GB', false)`,
	} {
		if err := validateStatementForDialect(stmt, strings.HasPrefix(stmt, "SELECT"), dialectPostgres); err == nil {
			t.Errorf("%q: accepted, want refused", stmt)
		}
	}
	for _, stmt := range []string{
		`CREATE FUNCTION add1(i int) RETURNS int LANGUAGE sql AS $$ SELECT i + 1 $$`,
		`CREATE TABLE settings (name text, value text)`,
		`UPDATE settings SET value = 'x' WHERE name = 'y'`,
	} {
		if err := validateStatementForDialect(stmt, false, dialectPostgres); err != nil {
			t.Errorf("%q: refused (%v), want accepted", stmt, err)
		}
	}
}

func TestPostgres_RowOverValueLimitRefusedBeforeTransfer(t *testing.T) {
	// A width the statement computes from a column: the row is refused by the
	// server, before the driver reads it into the runtime's heap.
	m, _ := pgTestManager(t, Config{MaxResultBytes: 1 << 20})
	ctx := context.Background()
	key := agentKey("t1", "wide")
	if _, err := m.Exec(ctx, key, "CREATE TABLE w (n int)", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO w VALUES (100000000)", nil, 0); err != nil {
		t.Fatal(err)
	}
	var err error
	got := allocatedDuring(func() {
		_, err = m.QueryCapped(ctx, key, "SELECT string_agg(md5(g::text), '') FROM w, generate_series(1, w.n / 32) g", nil)
	})
	var es *ErrStatement
	if !errors.As(err, &es) || !strings.Contains(err.Error(), "limit on one SQL Memory value") {
		t.Fatalf("err = %v (%T), want the value limit refusal", err, err)
	}
	if got > 32<<20 {
		t.Fatalf("refusing it allocated %d MiB in the runtime, want at most 32 MiB", got>>20)
	}
	// A later row over the limit, and inside a transaction, is refused too.
	if _, err := m.BeginTxn(ctx, "txn-w", "root-w", key); err != nil {
		t.Fatal(err)
	}
	_, err = m.QueryTxnCapped(ctx, "txn-w", "SELECT CASE WHEN g = 1 THEN 'small' ELSE repeat('y', 4) || string_agg(md5(h::text), '') END FROM generate_series(1, 2) g, generate_series(1, 100000) h GROUP BY g ORDER BY g", nil)
	if !errors.As(err, &es) || !strings.Contains(err.Error(), "limit on one SQL Memory value") {
		t.Fatalf("in a transaction: err = %v (%T), want the value limit refusal", err, err)
	}
	if _, err := m.RollbackTxn("txn-w"); err != nil {
		t.Fatal(err)
	}
	// An ordinary result keeps its rows, order and column names.
	if _, err := m.Exec(ctx, key, "INSERT INTO w VALUES (1), (2)", nil, 0); err != nil {
		t.Fatal(err)
	}
	res, err := m.QueryCapped(ctx, key, "SELECT n AS a, n * 2 AS a2, 'x' FROM w WHERE n < 10 ORDER BY n DESC;", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Columns, ",") != "a,a2,?column?" || len(res.Rows) != 2 || toInt(res.Rows[0][0]) != 2 {
		t.Fatalf("columns %v rows %v, want a,a2,?column? with 2 first", res.Columns, res.Rows)
	}
}

func TestValidateWidths_NonFiniteWidthsRefused(t *testing.T) {
	const limit = 16 << 20
	for _, c := range []struct {
		stmt string
		d    dialect
	}{
		{"SELECT repeat('x', 1e400 - 1e400)", dialectPostgres},
		{"SELECT repeat('x', 0*1e400)", dialectPostgres},
		{"SELECT repeat('x', -1e400)", dialectPostgres},
		{"SELECT repeat('x', 1/0)", dialectPostgres},
		{"SELECT repeat('x', 99999999999999999999999)", dialectPostgres},
		{"SELECT zeroblob(1e400 - 1e400)", dialectSQLite},
		{"SELECT zeroblob(0*1e400)", dialectSQLite},
		{"SELECT randomblob(99999999999999999999999)", dialectSQLite},
	} {
		if err := validateWidths(c.stmt, c.d, limit); err == nil {
			t.Errorf("%q: accepted, want refused", c.stmt)
		}
	}
}

func TestPgWrappable_RefusesStatementsThatEscapeTheWrapper(t *testing.T) {
	for _, stmt := range []string{
		`SELECT 1) AS z, (SELECT repeat('x', 10)`,
		`SELECT 1) AS z WHERE true UNION ALL SELECT repeat('x', 10) FROM (SELECT 1`,
		`SELECT (1`,
		`SELECT 1; SELECT 2`,
		// An escaped quote in an E'' string: a reader that ignored the escape
		// would take the rest as one string and miss the ')'.
		`SELECT E'a\'' ) AS z, (SELECT 2 --'`,
		`SELECT 'unterminated`,
		`SELECT $$unterminated`,
		`SELECT /* /* nested */ 1`,
		// With standard_conforming_strings off this is one string ending at
		// the last quote, and the ')' after the first is code.
		`SELECT 'a\'' ) AS z, (SELECT 2 --'`,
		`SELECT "a""b`,
	} {
		if err := pgCheckWrappable(stmt); err == nil {
			t.Errorf("%q: accepted, want refused", stmt)
		}
	}
	for _, stmt := range []string{
		`SELECT (1 + 2) * 3`,
		`SELECT ')' AS p, E'\')' AS q, $$)$$ AS r, "a)b" AS s -- )`,
		`SELECT /* ( /* ) */ */ 1;`,
		`SELECT /* /* nested */ ) */ 1`,
		`SELECT E'\') AS z, (SELECT 1'`,
		`WITH x AS (SELECT 1 AS a) SELECT a FROM x`,
		`SELECT a$b$ FROM t`,
		`SELECT $1::int`,
	} {
		if err := pgCheckWrappable(stmt); err != nil {
			t.Errorf("%q: refused (%v), want accepted", stmt, err)
		}
	}
}

func TestPostgres_WrapperEscapeRefused(t *testing.T) {
	m, _ := pgTestManager(t, Config{MaxResultBytes: 1 << 20})
	ctx := context.Background()
	key := agentKey("t1", "esc")
	// Pasted into the wrapper this closes its subquery and opens one of its
	// own, so the row check would apply to a different query than the one run.
	res, err := m.QueryCapped(ctx, key, `SELECT 1) AS z, (SELECT 2`, nil)
	var es *ErrStatement
	if !errors.As(err, &es) {
		t.Fatalf("err = %v (%T), result %v; want a statement refusal", err, err, res)
	}
}

func TestPgWrappable_ReadsPostgresLexicalEdges(t *testing.T) {
	refused := []string{
		// A tag of non-ASCII letters is a dollar quote to postgres: the ')' is
		// code after it, or hidden inside it, only if both read it alike.
		"SELECT $é$x$é$ ) AS z, (SELECT 2",
		// A '$' after a number starts a tag.
		"SELECT 1$a$ ( $a$) AS z, (SELECT 2",
		// A line comment ends at a carriage return too.
		"SELECT 1 -- x\r) AS z, (SELECT 2",
		// Nested block comments: still inside the outer one.
		"SELECT /* /* */ 1",
		// A '$' that is neither a tag nor a parameter.
		"SELECT 1 $ 2",
		"SELECT $1abc",
		// A string straight after a name, or a number run into letters.
		"SELECT ab'x'",
		"SELECT 1e'x'",
		"SELECT 1abc",
	}
	for _, stmt := range refused {
		if err := pgCheckWrappable(stmt); err == nil {
			t.Errorf("%q: accepted, want refused", stmt)
		}
	}
	accepted := []string{
		"SELECT $é$ ) $é$ AS a",
		"SELECT a$b$ FROM t",
		"SELECT 1 -- )\r\n",
		"SELECT /* /* ) */ */ 1",
		"SELECT $1, $2::int, E'\\'', B'01', X'ff', N'x', U&'d\\0061t'",
		"SELECT 1e5, 1.5, .5, 0x1F, 1_000",
	}
	for _, stmt := range accepted {
		if err := pgCheckWrappable(stmt); err != nil {
			t.Errorf("%q: refused (%v), want accepted", stmt, err)
		}
	}
}

func TestValidatePostgres_CarriageReturnEndsALineComment(t *testing.T) {
	// The statement check reads a line comment as ending at \r, as postgres
	// does, so text after it is checked.
	stmt := "SELECT 1 -- x\r, pg_read_file('/etc/passwd')"
	if err := validateStatementForDialect(stmt, true, dialectPostgres); err == nil {
		t.Fatalf("%q: accepted, want the denied function refused", stmt)
	}
}

func TestDollarTagAt_FollowsPostgresRules(t *testing.T) {
	for _, c := range []struct {
		s    string
		i    int
		want string
	}{
		{"$é$", 0, "$é$"},
		{"1$a$", 1, "$a$"},
		{"a$b$", 1, ""},
		{"$1", 0, ""},
		{"$$", 0, "$$"},
	} {
		got, _ := dollarTagAt(c.s, c.i)
		if got != c.want {
			t.Errorf("dollarTagAt(%q, %d) = %q, want %q", c.s, c.i, got, c.want)
		}
	}
}

func TestPostgres_WrapperAliasCollisionStillChecked(t *testing.T) {
	// A column named like the wrapper's alias: postgres resolves the bare name
	// to the column, so the row check must measure the whole row explicitly.
	m, _ := pgTestManager(t, Config{MaxResultBytes: 1 << 20})
	ctx := context.Background()
	key := agentKey("t1", "alias")
	for _, stmt := range []string{
		"SELECT NULL AS loomcycle_q, string_agg(md5(g::text), '') AS big FROM generate_series(1, 100000) g",
		"SELECT 'x' AS LOOMCYCLE_Q, string_agg(md5(g::text), '') AS big FROM generate_series(1, 100000) g",
	} {
		var err error
		got := allocatedDuring(func() { _, err = m.QueryCapped(ctx, key, stmt, nil) })
		if err == nil || !strings.Contains(err.Error(), "limit on one SQL Memory value") {
			t.Fatalf("%q: err = %v, want the value limit refusal", stmt, err)
		}
		if got > 32<<20 {
			t.Fatalf("%q: allocated %d MiB in the runtime", stmt, got>>20)
		}
	}
	res, err := m.QueryCapped(ctx, key, "SELECT 1 a, 2 a", nil)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("duplicate column names: %v %v", res, err)
	}
}

func TestPostgres_ExecReturningRefused(t *testing.T) {
	// sql_exec reports rows affected only, yet the driver reads returned rows
	// whole: a RETURNING clause is refused on postgres.
	m, _ := pgTestManager(t, Config{MaxResultBytes: 1 << 20})
	ctx := context.Background()
	key := agentKey("t1", "ret")
	if _, err := m.Exec(ctx, key, "CREATE TABLE w (n int, note text)", nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"INSERT INTO w (n) VALUES (1) RETURNING n",
		"WITH x AS (SELECT string_agg(md5(g::text), '') s FROM generate_series(1, 100000) g) UPDATE w SET n = 2 FROM x RETURNING x.s",
		"DELETE FROM w returning *",
	} {
		var es *ErrStatement
		if _, err := m.Exec(ctx, key, stmt, nil, 0); !errors.As(err, &es) {
			t.Errorf("%q: err = %v, want a statement refusal", stmt, err)
		}
	}
	if _, err := m.BeginTxn(ctx, "txn-r", "root-r", key); err != nil {
		t.Fatal(err)
	}
	var es *ErrStatement
	if _, err := m.ExecTxn(ctx, "txn-r", "INSERT INTO w (n) VALUES (1) RETURNING n", nil, 0); !errors.As(err, &es) {
		t.Errorf("in a transaction: err = %v, want a statement refusal", err)
	}
	if _, err := m.RollbackTxn("txn-r"); err != nil {
		t.Fatal(err)
	}
	// The word in a string or a name is not the clause.
	if _, err := m.Exec(ctx, key, "INSERT INTO w (n, note) VALUES (1, 'returning soon')", nil, 0); err != nil {
		t.Fatalf("a string holding the word: %v", err)
	}
}

func TestPgWrappable_NoDollarQuoteRightAfterANumber(t *testing.T) {
	// Postgres 14 reads 0x1$a$ and 1_0$a$ as a number then a name, 15+ as a
	// number then a dollar quote: either reading may be the server's.
	for _, stmt := range []string{
		"SELECT 0x1$a$) AS z, (SELECT 1 --$a$",
		"SELECT 1_0$a$) AS z, (SELECT 1 --$a$",
		"SELECT 1$a$ x $a$",
	} {
		if err := pgCheckWrappable(stmt); err == nil {
			t.Errorf("%q: accepted, want refused", stmt)
		}
	}
}
