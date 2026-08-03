// Package sqlmem implements RFC AA SQL Memory: a runtime-hosted, per-scope
// sqlite database that AUTHORIZED agents run arbitrary SQL against, isolated
// from the main loomcycle store.
//
// The security model is NOT injection defence (agents are authorized to run
// SQL) — it is preventing an agent from ESCAPING its per-scope sqlite file
// (reading/writing arbitrary host files via ATTACH / VACUUM INTO, or running
// arbitrary code via load_extension) and from exhausting resources.
//
// The DEFAULT sqlite driver loomcycle links — modernc.org/sqlite (pure-Go) —
// exposes NO sqlite3_set_authorizer, so there is no runtime statement
// interception. The PRIMARY, driver-agnostic defence is therefore this
// Go-layer parsed statement validator, run BEFORE any agent SQL reaches the
// driver. Per-scope file isolation (a separate .db per scope) is the backstop:
// even a missed escape can only ever touch that one scope's file. (The cgo
// `mattn/go-sqlite3` vec build additionally exposes RegisterAuthorizer — a
// future hardening upgrade, not the Phase-1 floor.)
package sqlmem

import (
	"fmt"
	"regexp"
	"strings"
)

// ErrStatement is returned by validateStatement when an agent statement is
// refused by the security floor. The message is model-facing (surfaced as
// is_error so the agent can self-correct).
type ErrStatement struct{ Reason string }

func (e *ErrStatement) Error() string { return e.Reason }

func refuse(format string, a ...any) error { return &ErrStatement{Reason: fmt.Sprintf(format, a...)} }

// dialect selects the engine-specific deny rules layered on top of the shared
// floor. The shared checks (comment-strip, one-statement, leading-keyword
// allow/deny, read-only SELECT/WITH) hold for both; each dialect adds the
// escapes peculiar to its engine (sqlite: ATTACH/load_extension/VACUUM;
// postgres: COPY/SET/dangerous server functions — see validate_postgres.go).
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// loadExtensionRe matches a load_extension(...) function call anywhere in a
// statement (it can be nested inside an otherwise-allowed SELECT/INSERT), with
// optional whitespace before the paren. It covers the BARE name AND sqlite's
// three quoted-identifier forms — "load_extension", [load_extension],
// `load_extension` — because sqlite resolves a quoted identifier in call
// position as the function name, so `SELECT "load_extension"('x')` is a genuine
// call. (Caught here even though the default modernc driver already disables
// extension loading: this validator is the driver-AGNOSTIC floor, so it must
// hold for a future cgo / mattn vec build that enables extensions — otherwise
// the bypass would become a live RCE on a driver swap.) Word-boundary anchored
// on the bare form so a column like `my_load_extension_flag` is NOT matched.
const sqlBacktick = "`"

var loadExtensionRe = regexp.MustCompile(
	`(?i)(?:\bload_extension\b|"load_extension"|\[load_extension\]|` + sqlBacktick + `load_extension` + sqlBacktick + `)\s*\(`,
)

// leadingKeywordRe captures the first SQL keyword (letters/underscore) after
// comment-stripping + trimming.
var leadingKeywordRe = regexp.MustCompile(`^\s*([a-zA-Z_]+)`)

// dataModifyingRe matches a data-modifying / DDL keyword as a whole word —
// used to keep sql_query (read-only) from smuggling writes via a CTE
// (`WITH x AS (...) DELETE FROM ...`, which sqlite permits).
var dataModifyingRe = regexp.MustCompile(`(?i)\b(insert|update|delete|replace|create|drop|alter|truncate)\b`)

// execLeadingAllowed is the closed allow-set of leading keywords for sql_exec
// (DDL + DML). Transactions (BEGIN/COMMIT/…) are excluded — Phase 1 is one
// auto-committed statement per call.
var execLeadingAllowed = map[string]bool{
	"create": true, "drop": true, "alter": true,
	"insert": true, "update": true, "delete": true, "replace": true,
	"with": true, // a CTE that ends in INSERT/UPDATE/DELETE is a valid write
}

// deniedLeading is the set of statement-leading keywords that can ONLY appear
// at the top of a statement and are always refused: they reach outside the
// scope file (ATTACH/DETACH), copy it out (VACUUM [INTO]), or mutate engine
// state (PRAGMA — the runtime sets the safe pragmas at open; agents never need
// them). Reattach/transactions are likewise out of scope for Phase 1.
var deniedLeading = map[string]string{
	"attach":    "ATTACH is denied — it can read/write arbitrary host files outside this scope",
	"detach":    "DETACH is denied",
	"vacuum":    "VACUUM is denied — VACUUM INTO can copy this database to an arbitrary path",
	"pragma":    "PRAGMA is denied — the runtime sets the safe pragmas; agents cannot change engine state",
	"begin":     "explicit transactions are not supported (each call is one auto-committed statement)",
	"commit":    "explicit transactions are not supported (each call is one auto-committed statement)",
	"rollback":  "explicit transactions are not supported (each call is one auto-committed statement)",
	"savepoint": "explicit transactions are not supported",
	"release":   "explicit transactions are not supported",
}

// validateStatement enforces the SQL security floor for the sqlite dialect
// (the file-backed default). It is the entry point used by the validator's
// own tests; the Manager facade calls validateStatementForDialect with the
// configured backend's dialect.
func validateStatement(raw string, readOnly bool) error {
	return validateStatementForDialect(raw, readOnly, dialectSQLite)
}

// validateStatementForDialect enforces the SQL security floor on one
// agent-authored statement. readOnly (sql_query) additionally requires a
// read-only SELECT/WITH-read statement. The dialect adds engine-specific
// escape denies on top of the shared floor. Returns *ErrStatement on refusal.
//
// Steps: strip comments → reject empty → reject multi-statement → reject
// load_extension anywhere → check the leading keyword against the deny-set and
// the per-op allow-set → apply the dialect's extra denies.
func validateStatementForDialect(raw string, readOnly bool, d dialect) error {
	// Dollar quoting is a POSTGRES string form, and the two scanners below must know
	// about it on that tier: without it, a legitimate `VALUES ($$x; y$$)` was refused
	// as multi-statement, and a `$$--$$` hid a real separator from the guard (safe
	// today only because pgx's extended protocol rejects multi-statement strings —
	// an implicit dependency this removes).
	dollarQuotes := d == dialectPostgres
	stripped := stripComments(raw, dollarQuotes)
	trimmed := strings.TrimSpace(stripped)
	if trimmed == "" {
		return refuse("empty SQL statement")
	}

	// One statement per call (Phase 1). A single trailing ';' is fine; any
	// content after a ';' is a second statement and is refused — both to keep
	// the auto-commit-one-statement contract and to block `SELECT 1; ATTACH …`.
	// modernc executes a second ';'-separated statement, so this guard is
	// LOAD-BEARING (not belt-and-braces). NOTE: it is ALSO what blocks
	// CREATE TRIGGER (a trigger body has a mandatory inner ';'), which closes
	// the deferred-payload-via-trigger escape (a trigger that ATTACHes on later
	// fire). Anyone relaxing the multi-statement rule to support triggers MUST
	// add a trigger-body scan first.
	if idx := indexOfStatementSeparator(trimmed, dollarQuotes); idx >= 0 {
		rest := strings.TrimSpace(trimmed[idx+1:])
		if rest != "" {
			return refuse("only one SQL statement per call is allowed (found a ';' separating multiple statements)")
		}
	}

	// load_extension(...) can be nested inside an otherwise-allowed statement
	// → scan the whole comment-stripped text. Single-quoted string LITERALS are
	// masked first: a value like 'see load_extension(x) in the docs' is data,
	// not a call (sqlite never treats a '…' literal as a function name), so it
	// must not false-positive. Identifier-quoted forms ("load_extension" etc.)
	// are kept verbatim — they ARE catchable function names (handled by the
	// regex alternatives).
	if loadExtensionRe.MatchString(maskStringLiterals(trimmed)) {
		return refuse("load_extension is denied — loading a sqlite extension runs arbitrary native code")
	}

	m := leadingKeywordRe.FindStringSubmatch(trimmed)
	if m == nil {
		return refuse("could not parse a leading SQL keyword")
	}
	kw := strings.ToLower(m[1])

	if reason, denied := deniedLeading[kw]; denied {
		return refuse("%s", reason)
	}

	// Dialect-specific escapes the shared allow-sets don't catch (their leading
	// keyword is otherwise legal). Applied before the read-only/exec split so it
	// guards both ops.
	if d == dialectPostgres {
		if err := postgresStatementDenies(trimmed, readOnly); err != nil {
			return err
		}
	}

	if readOnly {
		// sql_query is read-only: SELECT, or a WITH whose body is a SELECT
		// (NOT a data-modifying CTE).
		if kw != "select" && kw != "with" {
			return refuse("sql_query only runs read-only statements (SELECT / WITH … SELECT); use sql_exec for writes")
		}
		if kw == "with" && dataModifyingRe.MatchString(trimmed) {
			return refuse("sql_query is read-only — this WITH statement contains a data-modifying clause; use sql_exec")
		}
		return nil
	}

	// sql_exec: DDL/DML only, from the closed allow-set.
	if !execLeadingAllowed[kw] {
		return refuse("sql_exec only runs DDL/DML (CREATE/DROP/ALTER/INSERT/UPDATE/DELETE/REPLACE/WITH); %q is not allowed", kw)
	}
	return nil
}

// stripComments removes -- line comments and /* */ block comments WITHOUT
// removing comment-like sequences inside string/identifier literals (a `--`
// inside '...' or "..." is data, not a comment). Conservative: when in doubt it
// keeps text (the leading-keyword + deny checks still apply to whatever
// remains).
func stripComments(s string, dollarQuotes bool) string {
	var b strings.Builder
	b.Grow(len(s))
	const (
		normal = iota
		inSingle
		inDouble
		inBracket // [identifier]
		inBacktick
		inLine  // -- ...
		inBlock // /* ... */
	)
	state := normal
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case normal:
			// A dollar-quoted body is data: copy it through verbatim so a `--` or a
			// `/*` inside it is never mistaken for a comment. Postgres tier only.
			if dollarQuotes && c == '$' {
				if tag, ok := dollarTagAt(s, i); ok {
					if end := strings.Index(s[i+len(tag):], tag); end >= 0 {
						stop := i + len(tag) + end + len(tag)
						b.WriteString(s[i:stop])
						i = stop - 1
						continue
					}
				}
			}
			switch {
			case c == '-' && i+1 < len(s) && s[i+1] == '-':
				state = inLine
				i++
			case c == '/' && i+1 < len(s) && s[i+1] == '*':
				state = inBlock
				i++
			case c == '\'':
				state = inSingle
				b.WriteByte(c)
			case c == '"':
				state = inDouble
				b.WriteByte(c)
			case c == '[':
				state = inBracket
				b.WriteByte(c)
			case c == '`':
				state = inBacktick
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		case inLine:
			if c == '\n' {
				state = normal
				b.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				state = normal
				i++
				b.WriteByte(' ') // a block comment is a token separator
			}
		case inSingle:
			b.WriteByte(c)
			if c == '\'' {
				state = normal
			}
		case inDouble:
			b.WriteByte(c)
			if c == '"' {
				state = normal
			}
		case inBracket:
			b.WriteByte(c)
			if c == ']' {
				state = normal
			}
		case inBacktick:
			b.WriteByte(c)
			if c == '`' {
				state = normal
			}
		}
	}
	return b.String()
}

// maskStringLiterals blanks the CONTENT of single-quoted string literals
// (keeping the quotes) so a function-name/keyword scan can't false-positive on
// a value. A ” escaped-quote is handled by the open/close toggle (the doubled
// quote re-enters the string state). Operates on already comment-stripped text.
// Only single quotes are masked: '…' is ALWAYS a string in sqlite, whereas
// "…"/[…]/`…` are identifiers (a quoted FUNCTION name we want the scan to see).
func maskStringLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				inStr = false
				b.WriteByte(c)
			}
			continue // drop string content
		}
		if c == '\'' {
			inStr = true
		}
		b.WriteByte(c)
	}
	return b.String()
}

// indexOfStatementSeparator returns the index of the first ';' that is NOT
// inside a string/identifier literal, or -1. Used to detect multi-statement
// input. Mirrors stripComments' literal-awareness so a ';' inside '…' is data.
// dollarTagAt reads a postgres dollar-quote delimiter starting at s[i] (which must
// be '$'), returning the full delimiter (e.g. "$$" or "$tag$") and true.
//
// The tag rule is postgres's: $ then an optional identifier that does NOT start
// with a digit, then $. That exclusion is what keeps a POSITIONAL PARAMETER ($1,
// $2 — which the scope tier uses for every bind) from being mistaken for the start
// of a quoted string, which would swallow the rest of the statement.
func dollarTagAt(s string, i int) (string, bool) {
	if i >= len(s) || s[i] != '$' {
		return "", false
	}
	j := i + 1
	for j < len(s) {
		c := s[j]
		switch {
		case c == '$':
			return s[i : j+1], true
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j++
		case c >= '0' && c <= '9' && j > i+1:
			// A digit is legal INSIDE a tag but not as its first character.
			j++
		default:
			return "", false
		}
	}
	return "", false
}

func indexOfStatementSeparator(s string, dollarQuotes bool) int {
	const (
		normal = iota
		inSingle
		inDouble
		inBracket
		inBacktick
	)
	state := normal
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case normal:
			// Postgres dollar quoting, on that tier only: sqlite has no $tag$ string
			// form ($name is a bind parameter there), so scanning for it would
			// mis-tokenize valid sqlite.
			if dollarQuotes && c == '$' {
				if tag, ok := dollarTagAt(s, i); ok {
					if end := strings.Index(s[i+len(tag):], tag); end >= 0 {
						i += len(tag) + end + len(tag) - 1
						continue
					}
					// Unterminated: the engine will reject the statement, and treating
					// the remainder as quoted would hide a real separator from this scan.
					// Fall through and keep scanning.
				}
			}
			switch c {
			case ';':
				return i
			case '\'':
				state = inSingle
			case '"':
				state = inDouble
			case '[':
				state = inBracket
			case '`':
				state = inBacktick
			}
		case inSingle:
			if c == '\'' {
				state = normal
			}
		case inDouble:
			if c == '"' {
				state = normal
			}
		case inBracket:
			if c == ']' {
				state = normal
			}
		case inBacktick:
			if c == '`' {
				state = normal
			}
		}
	}
	return -1
}
