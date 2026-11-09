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

// pgCheckWrappable refuses a statement that wrapValueLimit could not paste
// into its subquery and still run as written: parentheses must balance and
// never close more than they opened, and nothing may follow a ';'. Otherwise a
// statement such as "SELECT 1) AS z, (SELECT …" closes the wrapper's subquery
// and the row check applies to a different query than the one the validator
// approved.
//
// It reads the raw statement with postgres's own lexical rules (scan.l), not
// the shared comment stripper: '…' strings with a doubled quote, E'…' strings
// with backslash escapes, $tag$…$tag$ bodies (tags of letters, '_' and bytes
// >= 0x80; a '$' inside a name is not one, one after a number is), $N
// parameters, "…" identifiers, -- line comments ending at \n or \r and nested
// /* */ comments. It fails closed: anything left open at the end, and any
// construct it cannot be sure postgres reads the same way (a stray '$', a
// number or name run straight into a string), is refused.
//
// A session can turn standard_conforming_strings off (a function body may
// EXECUTE 'SET …'), and then a plain '…' string takes backslash escapes too.
// The statement is read both ways and must pass both.
func pgCheckWrappable(statement string) error {
	if err := pgCheckWrappableAs(statement, false); err != nil {
		return err
	}
	return pgCheckWrappableAs(statement, true)
}

func pgCheckWrappableAs(statement string, backslashStrings bool) error {
	s := statement
	depth := 0
	unterminated := refuse("the statement ends inside a string, quoted name or comment")
	unclear := func(what string) error {
		return refuse("%s is not read here the way postgres may read it; write it plainly", what)
	}
	escapedString := false // the next '…' is an E'…' string
	for i := 0; i < len(s); i++ {
		c := s[i]
		prefixed := escapedString
		escapedString = false
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			// A line comment ends at \n or \r, as postgres ends it.
			if k := strings.IndexAny(s[i:], "\r\n"); k >= 0 {
				i += k
			} else {
				i = len(s)
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// Block comments nest, as in postgres.
			nest := 1
			for i += 2; nest > 0; i++ {
				if i+1 >= len(s) {
					return unterminated
				}
				switch {
				case s[i] == '/' && s[i+1] == '*':
					nest++
					i++
				case s[i] == '*' && s[i+1] == '/':
					nest--
					i++
				}
			}
			i--
		case c == '\'':
			escapes := backslashStrings || prefixed
			j := i + 1
			for ; ; j++ {
				if j >= len(s) {
					return unterminated
				}
				if escapes && s[j] == '\\' {
					j++
					continue
				}
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						j++
						continue
					}
					break
				}
			}
			i = j
		case c == '"':
			j := i + 1
			for ; ; j++ {
				if j >= len(s) {
					return unterminated
				}
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						j++
						continue
					}
					break
				}
			}
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			// A number: digits, '_', '.', an exponent, or a 0x/0o/0b prefix.
			// One run straight into a name or a string is read differently by
			// postgres versions (an error, or two tokens): refused.
			j := i + 1
			if c == '0' && j < len(s) && strings.IndexByte("xXoObB", s[j]) >= 0 {
				for j++; j < len(s) && (isHexDigit(s[j]) || s[j] == '_'); j++ {
				}
			} else {
				for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '_' || s[j] == '.') {
					j++
				}
				if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
					k := j + 1
					if k < len(s) && (s[k] == '+' || s[k] == '-') {
						k++
					}
					if k < len(s) && s[k] >= '0' && s[k] <= '9' {
						for j = k; j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '_'); j++ {
						}
					}
				}
			}
			// And a '$' straight after a number: postgres 15+ opens a dollar
			// quote there, 14 reads the rest as a name.
			if j < len(s) && (pgIdentStart(s[j]) || s[j] == '\'' || s[j] == '"' || s[j] == '$') {
				return unclear("a number run into a name or string (" + s[i:min(len(s), j+1)] + ")")
			}
			i = j - 1
		case pgIdentStart(c):
			j := i + 1
			for j < len(s) && pgIdentCont(s[j]) {
				j++
			}
			if j < len(s) && s[j] == '\'' {
				// Only a one-letter prefix may run into a string: E'…' takes
				// backslash escapes, B'…' X'…' N'…' do not.
				switch w := s[i:j]; w {
				case "e", "E":
					escapedString = true
				case "b", "B", "x", "X", "n", "N":
				default:
					return unclear("a string straight after the name " + w)
				}
			}
			i = j - 1
		case c == '$':
			if i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
				// A parameter, $N.
				j := i + 1
				for j < len(s) && s[j] >= '0' && s[j] <= '9' {
					j++
				}
				if j < len(s) && pgIdentCont(s[j]) {
					return unclear("a parameter run into a name (" + s[i:min(len(s), j+1)] + ")")
				}
				i = j - 1
				continue
			}
			tag, ok := dollarTagAt(s, i)
			if !ok {
				return unclear("a '$' that is neither a parameter nor a dollar quote")
			}
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				return unterminated
			}
			i += len(tag) + end + len(tag) - 1
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth < 0 {
				return refuse("the statement closes a parenthesis it did not open")
			}
		case c == ';':
			if strings.TrimSpace(s[i+1:]) != "" {
				return refuse("only one SQL statement per call is allowed, with nothing after its ';'")
			}
		}
	}
	if depth != 0 {
		return refuse("the statement leaves a parenthesis open")
	}
	return nil
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// wrapValueLimit runs an agent's read-only statement inside a query that makes
// the server refuse any row whose text form is over limit bytes, before the
// row is sent: postgres has no per-value limit, and the driver reads a whole
// row into the runtime's heap before the result cap can measure it.
//
// The statement must pass pgCheckWrappable first.
//
// The row is measured as ROW(loomcycle_q.*), never the bare alias: postgres
// resolves a bare loomcycle_q to a COLUMN of that name before the whole-row
// alias, and the statement may name one.
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
	return fmt.Sprintf("SELECT loomcycle_q.* FROM (%s\n) AS loomcycle_q WHERE CASE WHEN octet_length(ROW(loomcycle_q.*)::text) > %d "+
		"THEN CAST('%s:' || octet_length(ROW(loomcycle_q.*)::text) AS integer) > 0 ELSE true END",
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

// pgHasReturning reports a RETURNING clause in a write: sql_exec reports only
// the rows it affected, yet the driver reads every returned row whole into the
// runtime's heap, with no result cap on that path. A quoted name or a string
// holding the word is not the clause.
func pgHasReturning(statement string) bool {
	for _, t := range widthTokens(stripComments(strings.ReplaceAll(statement, "\r", "\n"), true), true) {
		if t.kind == tokIdent && !t.quoted && strings.EqualFold(t.text, "returning") {
			return true
		}
	}
	return false
}
