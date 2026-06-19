package sqlmem

import "testing"

// TestValidatePostgres_DeniesEscapes asserts the postgres-dialect validator
// refuses the escapes the shared closed allow-sets don't already cover
// (dangerous CREATE/ALTER DDL and nested server-side functions), and that the
// shared allow-sets still block the leading-keyword escapes. Pure unit — no
// database required.
func TestValidatePostgres_DeniesEscapes(t *testing.T) {
	denied := []struct {
		name     string
		stmt     string
		readOnly bool
	}{
		// Dangerous CREATE/ALTER whose leading keyword IS in the exec allow-set.
		{"create extension", "CREATE EXTENSION dblink", false},
		{"create extension if not exists", "CREATE EXTENSION IF NOT EXISTS plpython3u", false},
		{"create or replace language", "CREATE OR REPLACE LANGUAGE plpython3u", false},
		{"create foreign data wrapper", "CREATE FOREIGN DATA WRAPPER w", false},
		{"create server", "CREATE SERVER s FOREIGN DATA WRAPPER w", false},
		{"create subscription", "CREATE SUBSCRIPTION sub CONNECTION 'x' PUBLICATION p", false},
		{"create publication", "CREATE PUBLICATION p FOR ALL TABLES", false},
		{"alter system", "ALTER SYSTEM SET wal_level = minimal", false},
		{"alter role", "ALTER ROLE postgres SUPERUSER", false},
		{"alter database", "ALTER DATABASE x OWNER TO y", false},
		// Server-side functions nested inside an allowed SELECT.
		{"pg_read_file", "SELECT pg_read_file('/etc/passwd')", true},
		{"pg_ls_dir", "SELECT pg_ls_dir('/')", true},
		{"lo_import", "SELECT lo_import('/etc/passwd')", true},
		{"lo_export in exec", "SELECT lo_export(1, '/tmp/x')", true},
		{"dblink", "SELECT * FROM dblink('host=other', 'SELECT 1') AS t(x int)", true},
		// Double-quoted identifier call forms — Postgres resolves a quoted
		// identifier in call position as the function name (M-2 bypass).
		{"quoted pg_read_file", `SELECT "pg_read_file"('/etc/passwd')`, true},
		{"schema-qualified quoted pg_read_file", `SELECT "pg_catalog"."pg_read_file"('/etc/passwd')`, true},
		{"quoted lo_import", `SELECT "lo_import"('/etc/passwd')`, true},
		{"quoted dblink", `SELECT * FROM "dblink"('host=x', 'SELECT 1') AS t(x int)`, true},
		// SELECT … INTO creates a table — a write through the read-only op.
		{"select into", "SELECT * INTO newtab FROM t", true},
		{"select x into temp", "SELECT x INTO TEMP scratch FROM t", true},
		// Leading-keyword escapes — blocked by the shared closed allow-sets.
		{"copy from program (exec)", "COPY t FROM PROGRAM 'id'", false},
		{"copy to file (exec)", "COPY t TO '/tmp/x'", false},
		{"set role (exec)", "SET ROLE postgres", false},
		{"set search_path (exec)", "SET search_path TO public", false},
		{"reset role (exec)", "RESET ROLE", false},
		{"grant (exec)", "GRANT ALL ON SCHEMA public TO bob", false},
		{"do block (exec)", "DO $$ BEGIN PERFORM 1; END $$", false},
		{"copy in query", "COPY t FROM PROGRAM 'id'", true},
	}
	for _, tc := range denied {
		t.Run("deny/"+tc.name, func(t *testing.T) {
			err := validateStatementForDialect(tc.stmt, tc.readOnly, dialectPostgres)
			if err == nil {
				t.Fatalf("statement was allowed; want refusal: %q", tc.stmt)
			}
			if _, ok := err.(*ErrStatement); !ok {
				t.Fatalf("error = %T (%v); want *ErrStatement", err, err)
			}
		})
	}

	allowed := []struct {
		name     string
		stmt     string
		readOnly bool
	}{
		{"create table", "CREATE TABLE notes (id INT, body TEXT)", false},
		{"insert", "INSERT INTO notes VALUES (1, 'hi')", false},
		{"update", "UPDATE notes SET body='x' WHERE id=1", false},
		{"delete", "DELETE FROM notes WHERE id=1", false},
		{"alter table", "ALTER TABLE notes ADD COLUMN tag TEXT", false},
		{"drop table", "DROP TABLE notes", false},
		{"create index", "CREATE INDEX idx ON notes (id)", false},
		{"with insert", "WITH x AS (SELECT 1 AS n) INSERT INTO notes (id) SELECT n FROM x", false},
		{"select", "SELECT id, body FROM notes ORDER BY id", true},
		{"with select", "WITH x AS (SELECT 1 AS n) SELECT n FROM x", true},
		// A literal mentioning a denied function name is DATA, not a call.
		{"fn-name in string literal", "INSERT INTO notes VALUES (1, 'see pg_read_file(x) in docs')", false},
		// A column named like a denied function (no call paren) is fine.
		{"fn-name-like column", "SELECT pg_read_file_note FROM notes", true},
		// A double-quoted column literally named "into" is NOT a SELECT … INTO.
		{"quoted into column", `SELECT "into" FROM notes`, true},
		// "into" as part of a longer identifier must not trip the INTO rule.
		{"into-substring column", "SELECT migrated_into FROM notes", true},
	}
	for _, tc := range allowed {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			if err := validateStatementForDialect(tc.stmt, tc.readOnly, dialectPostgres); err != nil {
				t.Fatalf("statement was refused; want allowed: %q -> %v", tc.stmt, err)
			}
		})
	}
}

// TestValidatePostgres_StringLiteralNotAFunctionCall guards the false-positive
// path: a denied function name appearing only inside a single-quoted string is
// masked before the function scan, so it must not trip the deny.
func TestValidatePostgres_StringLiteralNotAFunctionCall(t *testing.T) {
	ok := "INSERT INTO t (note) VALUES ('call lo_import(''/etc/passwd'') to break out')"
	if err := validateStatementForDialect(ok, false, dialectPostgres); err != nil {
		t.Fatalf("string-literal mention was refused; want allowed: %v", err)
	}
	bad := "SELECT lo_import('/etc/passwd')"
	if err := validateStatementForDialect(bad, true, dialectPostgres); err == nil {
		t.Fatal("real lo_import call was allowed; want refusal")
	}
}
