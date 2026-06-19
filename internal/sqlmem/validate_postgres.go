package sqlmem

import "regexp"

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

// postgresStatementDenies applies the postgres-dialect escape denies to one
// already-shared-validated statement (trimmed = comment-stripped + trimmed).
// readOnly is accepted for symmetry but the read-only transaction the backend
// opens for sql_query is the authoritative write backstop (SELECT … INTO and
// any other write fail at the engine), so there is no separate INTO rule here.
func postgresStatementDenies(trimmed string, readOnly bool) error {
	if pgDangerousDDLRe.MatchString(trimmed) {
		return refuse("this CREATE/ALTER form is denied on the postgres tier — extensions, languages, foreign servers, replication, and system/role/database changes can escape the scope")
	}
	if pgServerFnRe.MatchString(maskStringLiterals(trimmed)) {
		return refuse("this statement calls a denied server-side function (file / large-object I/O or dblink) — it can read host files or connect to another database")
	}
	return nil
}
