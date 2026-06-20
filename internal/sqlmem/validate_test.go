package sqlmem

import (
	"errors"
	"testing"
)

// TestValidateStatement_EscapeBlocked is the RFC AA security floor: the escape
// vectors (ATTACH / load_extension / writable_schema PRAGMA / VACUUM INTO) and
// multi-statement smuggling are all refused, on BOTH ops. This is the
// driver-agnostic primary defence (modernc has no authorizer).
func TestValidateStatement_EscapeBlocked(t *testing.T) {
	denied := []struct {
		name string
		sql  string
	}{
		{"attach", `ATTACH DATABASE '/var/lib/loomcycle/loomcycle.db' AS main2`},
		{"attach-lower", `attach database 'x.db' as y`},
		{"detach", `DETACH DATABASE x`},
		{"vacuum-into", `VACUUM INTO '/tmp/exfil.db'`},
		{"vacuum-bare", `VACUUM`},
		{"pragma-writable-schema", `PRAGMA writable_schema=ON`},
		{"pragma-any", `PRAGMA table_info(secrets)`},
		{"load-extension-select", `SELECT load_extension('/tmp/evil.so')`},
		{"load-extension-spaced", `SELECT load_extension ('x')`},
		{"load-extension-exec", `INSERT INTO t VALUES (load_extension('x'))`},
		// M-1: sqlite resolves a quoted identifier in call position as the
		// function name, so these are genuine load_extension calls — the
		// driver-agnostic floor must catch them (latent RCE on a vec build).
		{"load-extension-dquoted", `SELECT "load_extension"('/tmp/evil.so')`},
		{"load-extension-bracket", `SELECT [load_extension]('/tmp/evil.so')`},
		{"load-extension-backtick", "SELECT `load_extension`('/tmp/evil.so')"},
		{"multi-stmt-attach", `SELECT 1; ATTACH DATABASE 'x' AS y`},
		{"multi-stmt-drop", `CREATE TABLE t(a); DROP TABLE other`},
		{"begin-txn", `BEGIN`},
		{"commit-txn", `COMMIT`},
		// Phase 3b runtime-issues SAVEPOINT/RELEASE/ROLLBACK TO; an agent must
		// never issue them raw (it would desync the runtime's savepoint stack).
		{"rollback-txn", `ROLLBACK`},
		{"rollback-to", `ROLLBACK TO SAVEPOINT sp1`},
		{"savepoint", `SAVEPOINT sp1`},
		{"release", `RELEASE SAVEPOINT sp1`},
		{"comment-hidden-attach", `/* harmless */ ATTACH DATABASE 'x' AS y`},
		{"line-comment-attach", "-- note\nATTACH DATABASE 'x' AS y"},
	}
	for _, tc := range denied {
		t.Run("exec/"+tc.name, func(t *testing.T) {
			if err := validateStatement(tc.sql, false); err == nil {
				t.Errorf("sql_exec MUST refuse %q", tc.sql)
			} else if !errors.As(err, new(*ErrStatement)) {
				t.Errorf("refusal should be *ErrStatement, got %T", err)
			}
		})
		t.Run("query/"+tc.name, func(t *testing.T) {
			if err := validateStatement(tc.sql, true); err == nil {
				t.Errorf("sql_query MUST refuse %q", tc.sql)
			}
		})
	}
}

// TestValidateStatement_AllowsLegitimate confirms the floor doesn't over-block:
// ordinary DDL/DML on sql_exec and SELECT/CTE-read on sql_query pass.
func TestValidateStatement_AllowsLegitimate(t *testing.T) {
	exec := []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`,
		`CREATE INDEX idx_notes_body ON notes(body)`,
		`INSERT INTO notes (body) VALUES ('hello')`,
		`UPDATE notes SET body = 'x' WHERE id = 1`,
		`DELETE FROM notes WHERE id = 1`,
		`DROP TABLE notes`,
		`ALTER TABLE notes ADD COLUMN tag TEXT`,
		"WITH new(b) AS (VALUES ('a'),('b')) INSERT INTO notes(body) SELECT b FROM new",
		`INSERT INTO notes (body) VALUES ('a'); `, // trailing ';' + whitespace is fine
	}
	for _, s := range exec {
		if err := validateStatement(s, false); err != nil {
			t.Errorf("sql_exec should allow %q, got: %v", s, err)
		}
	}

	query := []string{
		`SELECT id, body FROM notes WHERE body LIKE 'h%' ORDER BY id LIMIT 10`,
		`select count(*) from notes`,
		`WITH recent AS (SELECT * FROM notes ORDER BY id DESC LIMIT 5) SELECT body FROM recent`,
		`SELECT body FROM notes -- inline comment is fine`,
		// A value MENTIONING load_extension(...) is data, not a call — must
		// not false-positive (the function scan masks single-quoted strings).
		`SELECT body FROM notes WHERE body = 'how to use load_extension(x)'`,
		`SELECT * FROM notes WHERE note LIKE '%attach database%'`,
	}
	// And the same string-literal values are fine to WRITE (no false positive
	// from the keyword/function scans on quoted data).
	if err := validateStatement(`INSERT INTO notes (body) VALUES ('see load_extension(x) docs')`, false); err != nil {
		t.Errorf("storing text that mentions load_extension must be allowed, got: %v", err)
	}
	for _, s := range query {
		if err := validateStatement(s, true); err != nil {
			t.Errorf("sql_query should allow %q, got: %v", s, err)
		}
	}
}

// TestValidateStatement_QueryIsReadOnly locks the read-only contract: sql_query
// refuses writes — including a data-modifying CTE that smuggles a DELETE.
func TestValidateStatement_QueryIsReadOnly(t *testing.T) {
	writes := []string{
		`INSERT INTO notes (body) VALUES ('x')`,
		`UPDATE notes SET body='x'`,
		`DELETE FROM notes`,
		`CREATE TABLE t(a)`,
		`DROP TABLE notes`,
		`WITH x AS (SELECT 1) DELETE FROM notes`, // CTE-smuggled write
	}
	for _, s := range writes {
		if err := validateStatement(s, true); err == nil {
			t.Errorf("sql_query MUST refuse the write %q", s)
		}
		// …but the same statements are fine on sql_exec (except the ones the
		// deny-set/allow-set reject for other reasons — these are all writes).
	}
}

// TestStripComments_LiteralAware confirms a comment marker INSIDE a string
// literal is preserved (it's data), so a value like '-- x' or '/* y */' isn't
// silently truncated.
func TestStripComments_LiteralAware(t *testing.T) {
	cases := map[string]string{
		`SELECT '-- not a comment'`:    `SELECT '-- not a comment'`,
		`SELECT '/* not a comment */'`: `SELECT '/* not a comment */'`,
		`SELECT 1 -- real comment`:     `SELECT 1 `,
		`SELECT 1 /* real */ + 2`:      `SELECT 1   + 2`,
		`INSERT INTO t VALUES ('a;b')`: `INSERT INTO t VALUES ('a;b')`,
	}
	for in, want := range cases {
		if got := stripComments(in); got != want {
			t.Errorf("stripComments(%q) = %q, want %q", in, got, want)
		}
	}
	// A ';' inside a string literal is NOT a statement separator.
	if validateStatement(`INSERT INTO t VALUES ('a;b')`, false) != nil {
		t.Errorf("a ';' inside a string literal must not trip the multi-statement guard")
	}
}
