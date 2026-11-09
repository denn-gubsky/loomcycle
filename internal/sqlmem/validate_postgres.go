package sqlmem

import (
	"fmt"
	"regexp"
	"strings"
)

// validate_postgres.go — the postgres-dialect escape denies layered on the
// shared SQL floor. For the postgres tier the PRIMARY isolation is
// engine-enforced least privilege: a per-scope NOLOGIN role with USAGE only on
// its own schema, run inside a transaction the runtime pins with SET LOCAL ROLE
// / search_path / statement_timeout (a read-only transaction for sql_query, so
// any write — including SELECT … INTO — is refused by the engine). These
// statement-level denies are DEFENSE-IN-DEPTH: they give a clear model-facing
// error and guard a mis-provisioned role.
//
// Most leading-keyword escapes are ALREADY blocked by the shared closed
// allow-sets: sql_exec accepts only create/drop/alter/insert/update/delete/
// replace/with, and sql_query only select/with — so COPY, SET, RESET, GRANT,
// REVOKE, DO, CALL, LOAD, TRUNCATE, VACUUM, etc. never reach here. What the
// allow-sets do NOT catch (because their leading keyword IS allowed) are:
//   - CREATE EXTENSION / LANGUAGE / SERVER / FOREIGN … / PUBLICATION /
//     SUBSCRIPTION and ALTER SYSTEM / ROLE / DATABASE / USER / GROUP /
//     TABLESPACE — code-load, connect-out, or privilege/config changes whose
//     leading CREATE/ALTER is otherwise legal for tables.
//   - a denied server-side FUNCTION nested inside an allowed SELECT
//     (pg_read_file, lo_import, dblink, …) — leading SELECT is legal, the call
//     is the escape.

// pgDangerousDDLRe matches CREATE/ALTER forms whose leading keyword is in the
// exec allow-set but which reach outside table storage. Anchored at the start
// of the (comment-stripped, trimmed) statement.
var pgDangerousDDLRe = regexp.MustCompile(
	`(?i)^\s*(?:create\s+(?:or\s+replace\s+)?(?:extension|language|server|foreign|publication|subscription)|alter\s+(?:system|role|database|user|group|tablespace))\b`,
)

// pgServerFnNames is the alternation of denied server-side function names
// (file I/O, large-object I/O, cross-database links).
const pgServerFnNames = `pg_read_file|pg_read_binary_file|pg_stat_file|pg_ls_dir|pg_ls_logdir|pg_ls_waldir|pg_ls_tmpdir|lo_import|lo_export|lo_get|lo_put|lo_from_bytea|dblink|dblink_connect|dblink_exec`

// pgServerFnRe matches a call to a denied server-side function anywhere in the
// statement. Applied to maskStringLiterals output so a value mentioning the
// name is not a false positive. It covers BOTH the bare name (word-boundary
// anchored, so a column like my_pg_read_file_flag is NOT matched) AND the
// DOUBLE-QUOTED identifier form: Postgres resolves a quoted identifier in call
// position as the function name, so `SELECT "pg_read_file"('x')` (or the
// schema-qualified `"pg_catalog"."pg_read_file"('x')`) is a genuine call —
// mirrors the sqlite loadExtensionRe handling of quoted forms (an avoidable
// asymmetry otherwise, and a live escape against a mis-provisioned role).
var pgServerFnRe = regexp.MustCompile(
	`(?i)(?:\b(?:` + pgServerFnNames + `)\b|"(?:` + pgServerFnNames + `)")\s*\(`,
)

// pgSetConfigRe matches a set_config(...) call: the one way a statement the
// allow-sets admit can change a setting for the session (SET and RESET never
// get through), such as raising the work_mem baked onto the scope role.
var pgSetConfigRe = regexp.MustCompile(`(?i)(?:\bset_config\b|"set_config")\s*\(`)

// pgUnicodeIdentRe matches a U&"…" identifier, whose \XXXX escapes spell any
// name (u&"\0073et_config" is set_config) past the name checks above.
var pgUnicodeIdentRe = regexp.MustCompile(`(?i)\bu&"`)

// routineSetsSetting reports a CREATE [OR REPLACE] FUNCTION/PROCEDURE or an
// ALTER FUNCTION/PROCEDURE/ROUTINE that carries a SET clause: the routine then
// runs with that setting changed. Bodies and string literals are tokens of
// their own (widthTokens), so a SET inside a body is not this clause.
//
// This is best effort, not the bound: a body can still change a setting itself
// (plpgsql EXECUTE 'SET …'), as any session of the role can. work_mem baked
// onto the scope role is a default the role can change; the bounds the role
// cannot change are statement_timeout's deadline (applied by the runtime, not
// only the role setting) and temp_file_limit (a superuser setting).
func routineSetsSetting(trimmed string) bool {
	toks := widthTokens(trimmed, true)
	if len(toks) < 2 || toks[0].kind != tokIdent {
		return false
	}
	lead := strings.ToLower(toks[0].text)
	if lead != "create" && lead != "alter" {
		return false
	}
	routine := false
	for _, t := range toks[1:min(len(toks), 5)] {
		switch strings.ToLower(t.text) {
		case "function", "procedure", "routine":
			routine = true
		}
	}
	if !routine {
		return false
	}
	for _, t := range toks {
		if t.kind == tokIdent && strings.EqualFold(t.text, "set") {
			return true
		}
	}
	return false
}

// pgSelectIntoRe matches a `SELECT … INTO <table>` (which CREATES a table — a
// WRITE) in a read-only statement. Anchored as whitespace-INTO-whitespace so a
// double-quoted column literally named "into" (`SELECT "into" FROM t`) is NOT a
// false positive. The auto-commit sql_query path also opens a read-only
// transaction (defense-in-depth), but an EXPLICIT transaction is read-WRITE, so
// the validator is the only layer that catches a write-via-sql_query inside a
// txn — hence this rule lives here (it hardens both paths).
var pgSelectIntoRe = regexp.MustCompile(`(?i)\sinto\s`)

// postgresStatementDenies applies the postgres-dialect escape denies to one
// already-shared-validated statement (trimmed = comment-stripped + trimmed).
func postgresStatementDenies(trimmed string, readOnly bool) error {
	if pgDangerousDDLRe.MatchString(trimmed) {
		return refuse("this CREATE/ALTER form is denied on the postgres tier — extensions, languages, foreign servers, replication, and system/role/database changes can escape the scope")
	}
	masked := maskStringLiterals(trimmed)
	if pgServerFnRe.MatchString(masked) {
		return refuse("this statement calls a denied server-side function (file / large-object I/O or dblink) — it can read host files or connect to another database")
	}
	if pgSetConfigRe.MatchString(masked) {
		return refuse("set_config is denied — the scope's settings (memory, timeouts, search path) are fixed by the runtime")
	}
	if pgUnicodeIdentRe.MatchString(masked) {
		return refuse(`U&"…" identifiers are denied — write the name plainly`)
	}
	if routineSetsSetting(trimmed) {
		return refuse("a function or procedure with a SET clause is denied — the scope's settings are fixed by the runtime")
	}
	// SELECT … INTO creates a table; refuse it on the read-only op (it is a
	// write the read-only-transaction backstop would catch on auto-commit but
	// NOT inside an explicit read-write transaction).
	if readOnly && pgSelectIntoRe.MatchString(masked) {
		return refuse("SELECT … INTO creates a table — sql_query is read-only; use sql_exec (or CREATE TABLE AS) to write")
	}
	return nil
}

// pgValueLimitMarker names the error wrapValueLimit raises, so the runtime can
// tell it from any other.
const pgValueLimitMarker = "loomcycle-sqlmem-value-limit"

// wrapValueLimit runs an agent's read-only statement inside a query that makes
// the server refuse any row whose text form is over limit bytes, before the
// row is sent: postgres has no per-value limit, and the driver reads a whole
// row into the runtime's heap before the result cap can measure it.
//
// The refusal is a cast of a marker string to integer, made to depend on the
// row so the planner cannot fold it into an error at plan time. The outer
// query keeps the statement's columns (q.*), names and, as postgres runs a
// plain subquery scan, its order. A trailing ';' is dropped, and the closing
// parenthesis goes on a line of its own so a trailing line comment cannot
// swallow it.
func wrapValueLimit(statement string, limit int) string {
	stmt := strings.TrimSpace(statement)
	for strings.HasSuffix(stmt, ";") {
		stmt = strings.TrimSpace(strings.TrimSuffix(stmt, ";"))
	}
	return fmt.Sprintf("SELECT loomcycle_q.* FROM (%s\n) AS loomcycle_q WHERE CASE WHEN octet_length(loomcycle_q::text) > %d "+
		"THEN CAST('%s:' || octet_length(loomcycle_q::text) AS integer) > 0 ELSE true END",
		stmt, limit, pgValueLimitMarker)
}

// valueLimitErr turns wrapValueLimit's refusal into a statement error the model
// can act on; any other error is returned as it is.
func valueLimitErr(err error, limit int) error {
	if err != nil && strings.Contains(err.Error(), pgValueLimitMarker) {
		return refuse("a row of the result is over the %d-byte limit on one SQL Memory value; select less of it (substr, length) or fewer columns", limit)
	}
	return err
}
