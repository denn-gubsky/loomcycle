package builtin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Memory's SQL ops and the Document tool reach the same per-scope database, so
// without this guard an agent holding sql_scopes could rewrite what Document
// keeps there: mark its own proposed statement asserted, which only an operator
// may do, or read chunks and statements past Document's access rules. Memory
// SQL therefore never names a document table, for reads or for writes; Document
// ops are the only way in (owner decision, 2026-10-30).
//
// Not matching the text is not enough on its own, because a table can be named
// without its name appearing: on sqlite, by a view or trigger planted earlier;
// on postgres, also by a function that builds SQL from strings. So:
//   - every identifier and every string literal of the statement is checked,
//     in each quoting form the engine accepts as a table name (sqlite takes
//     'name' as an identifier in FROM);
//   - on postgres, creating a function or procedure and calling the core
//     functions that run a query given as text are refused;
//   - before the first Memory SQL call on a scope, its views, triggers, rules
//     and (postgres) functions are checked for a document table or, in a
//     function, for EXECUTE, since this guard never saw them made.
//
// sqlite cannot build SQL from strings, so there this is complete. On postgres
// it holds as long as no other route to dynamic SQL exists in the core
// functions; the engine-level fix (Document's tables owned by a role Memory's
// SQL does not log in as) is the follow-up that makes it not depend on that.

// documentTableExtras are names Document creates outside docSchemaDDL:
// chunk_assets, whose DDL is built per tier (BLOB or BYTEA) in ensureSchema, and
// the table migrateEdgeStatements builds and renames into chunk_edges.
var documentTableExtras = []string{"chunk_assets", "chunk_edges_statements"}

var createTableNameRe = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)

// documentTables is the reserved set, lower-cased: every table docSchemaDDL
// creates, plus the extras. TestMemorySQLGuard_CoversEveryDocumentTable holds it
// to the tables a fully migrated scope actually has, on both tiers.
var documentTables = func() map[string]bool {
	set := map[string]bool{}
	for _, ddl := range docSchemaDDL {
		for _, m := range createTableNameRe.FindAllStringSubmatch(ddl, -1) {
			set[strings.ToLower(m[1])] = true
		}
	}
	for _, n := range documentTableExtras {
		set[n] = true
	}
	return set
}()

// pgCreateRoutineRe matches CREATE [OR REPLACE] FUNCTION|PROCEDURE: a body can
// build a table name from strings and EXECUTE it.
var pgCreateRoutineRe = regexp.MustCompile(`(?i)^\s*create\s+(?:or\s+replace\s+)?(?:function|procedure)\b`)

// pgQueryTextFnNames are the core functions that run a query, or read a table,
// named by a string the guard cannot see through — and set_config, which could
// turn standard_conforming_strings off and so change where a literal ends.
const pgQueryTextFnNames = `query_to_xml|query_to_xmlschema|query_to_xml_and_xmlschema|` +
	`cursor_to_xml|cursor_to_xmlschema|table_to_xml|table_to_xmlschema|table_to_xml_and_xmlschema|` +
	`schema_to_xml|schema_to_xmlschema|schema_to_xml_and_xmlschema|` +
	`database_to_xml|database_to_xmlschema|database_to_xml_and_xmlschema|ts_stat|ts_rewrite|set_config`

var pgQueryTextFnRe = regexp.MustCompile(`(?i)(?:\b(?:` + pgQueryTextFnNames + `)\b|"(?:` + pgQueryTextFnNames + `)")\s*\(`)

// refuseDocumentTables returns the reason a Memory SQL statement is refused, or
// "" when it names no document table.
func refuseDocumentTables(stmt string, postgres bool) string {
	if postgres {
		if pgCreateRoutineRe.MatchString(stmt) {
			return "Memory SQL cannot create a function or procedure on this server: its body could reach the document tables"
		}
		if pgQueryTextFnRe.MatchString(stmt) {
			return "Memory SQL cannot call this function on this server (query_to_xml, table_to_xml, ts_stat, set_config, …): it could reach the document tables"
		}
	}
	names, bad := sqlNames(stmt, postgres)
	if bad != "" {
		return bad
	}
	for _, n := range names {
		if documentTables[n] {
			return fmt.Sprintf("%q is a document table: Memory SQL may not read or change it", n)
		}
	}
	return ""
}

// sqliteTablePosition are the tokens after which sqlite reads a 'quoted string'
// as a table name (SELECT * FROM 'chunks' works there).
var sqliteTablePosition = map[string]bool{
	"from": true, "join": true, "into": true, "update": true, "table": true, "on": true,
	"references": true, ".": true,
}

// sqliteClauses are the keywords that start a clause; a comma is a table
// position only inside FROM (FROM a, 'chunks'), never in a VALUES or WHERE list.
var sqliteClauses = map[string]bool{
	"select": true, "from": true, "where": true, "group": true, "order": true, "having": true,
	"limit": true, "values": true, "set": true, "returning": true, "on": true, "using": true,
	"union": true, "intersect": true, "except": true, "window": true,
}

// sqlNames returns the names stmt may use for a table, lower-cased: every
// identifier, in each quoting form, and on sqlite a string literal in a table
// position. Postgres never reads a string as a table name except through the
// functions refused above, so there a literal is only skipped — correctly,
// escapes included, so its end is where the engine ends it. The second value
// is set when stmt uses a form the guard cannot read safely.
func sqlNames(s string, postgres bool) ([]string, string) {
	var out []string
	prev, clause := "", ""
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	// quoted reads a quoted run opened at s[i] and closed by end, where a doubled
	// end is an escaped one. It returns the value and the index past the close.
	quoted := func(i int, end byte) (string, int) {
		var b strings.Builder
		j := i + 1
		for j < len(s) {
			if s[j] == end {
				if j+1 < len(s) && s[j+1] == end {
					b.WriteByte(end)
					j += 2
					continue
				}
				return b.String(), j + 1
			}
			b.WriteByte(s[j])
			j++
		}
		return b.String(), j
	}
	name := func(v string) {
		out = append(out, strings.ToLower(v))
		prev = "name"
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// postgres nests block comments; sqlite ends at the first */.
			depth := 0
			for i < len(s) {
				if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if i+1 < len(s) && s[i] == '*' && s[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 || !postgres {
						break
					}
					continue
				}
				i++
			}
		case c == '\'':
			v, next := quoted(i, '\'')
			if !postgres && (sqliteTablePosition[prev] || (prev == "," && clause == "from")) {
				name(v)
			} else {
				prev = "literal"
			}
			i = next
		case c == '"':
			v, next := quoted(i, '"')
			name(v)
			i = next
		case !postgres && c == '`':
			v, next := quoted(i, '`')
			name(v)
			i = next
		case !postgres && c == '[':
			j := strings.IndexByte(s[i+1:], ']')
			if j < 0 {
				name(s[i+1:])
				i = len(s)
			} else {
				name(s[i+1 : i+1+j])
				i += j + 2
			}
		case postgres && c == '$':
			tag, ok := dollarTag(s, i)
			if !ok {
				i++
				continue
			}
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				i = len(s)
			} else {
				i += len(tag) + end + len(tag)
			}
			prev = "literal"
		case isIdent(c) && !(c >= '0' && c <= '9'):
			j := i
			for j < len(s) && isIdent(s[j]) {
				j++
			}
			word := s[i:j]
			if postgres && j < len(s) && s[j] == '&' && strings.EqualFold(word, "u") {
				// U&"…" / U&'…' spells a name with escapes the guard does not decode.
				return nil, `Memory SQL cannot use U&"…" or U&'…' escapes on this server`
			}
			if postgres && j < len(s) && s[j] == '\'' && strings.EqualFold(word, "e") {
				i = pgEscapeStringEnd(s, j)
				prev = "literal"
				continue
			}
			name(word)
			prev = strings.ToLower(word)
			if sqliteClauses[prev] {
				clause = prev
			}
			i = j
		case c >= '0' && c <= '9':
			for i < len(s) && isIdent(s[i]) {
				i++
			}
			prev = "literal"
		default:
			prev = string(c)
			i++
		}
	}
	return out, ""
}

// dollarTag reports the $tag$ opening at s[i], if any.
func dollarTag(s string, i int) (string, bool) {
	j := i + 1
	for j < len(s) && (s[j] == '_' || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= '0' && s[j] <= '9' && j > i+1)) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[i : j+1], true
	}
	return "", false
}

// pgEscapeStringEnd returns the index past the E'…' literal whose quote is at
// s[i]: a backslash escapes the next byte, and a doubled quote is one quote.
func pgEscapeStringEnd(s string, i int) int {
	j := i + 1
	for j < len(s) {
		switch {
		case s[j] == '\\' && j+1 < len(s):
			j += 2
		case s[j] == '\'' && j+1 < len(s) && s[j+1] == '\'':
			j += 2
		case s[j] == '\'':
			return j + 1
		default:
			j++
		}
	}
	return j
}

// pgDynamicSQLRe finds EXECUTE in a function body: SQL built from strings,
// which no name check can see through.
var pgDynamicSQLRe = regexp.MustCompile(`(?i)\bexecute\b`)

// memorySQLScopeChecked remembers the scopes whose views, triggers and rules
// were found clean. Nothing made through Memory SQL afterwards can name a
// document table, so one check per scope per process is enough.
var memorySQLScopeChecked sync.Map

// plantedDocumentObject names a view, trigger or rule in the scope whose
// definition names a document table, or "" when there is none.
func plantedDocumentObject(ctx context.Context, m *sqlmem.Manager, key sqlmem.ScopeKey) (string, error) {
	ck := fmt.Sprintf("%p\x1f%s\x1f%s\x1f%s", m, key.Tenant, key.Scope, key.ScopeID)
	if _, ok := memorySQLScopeChecked.Load(ck); ok {
		return "", nil
	}
	postgres := m.Tier() == "postgres"
	q := `SELECT type, name, COALESCE(sql, '') FROM sqlite_master WHERE type IN ('view', 'trigger')`
	if postgres {
		q = `SELECT 'view', viewname, definition FROM pg_views WHERE schemaname = current_schema()
			UNION ALL SELECT 'rule', rulename, definition FROM pg_rules WHERE schemaname = current_schema()
			UNION ALL SELECT 'trigger', t.tgname, pg_get_triggerdef(t.oid) FROM pg_trigger t
				JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = current_schema() AND NOT t.tgisinternal
			UNION ALL SELECT 'function', p.proname, COALESCE(p.prosrc, '') FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = current_schema()`
	}
	res, err := m.Query(ctx, key, q, nil)
	if err != nil {
		return "", err
	}
	for _, row := range res.Rows {
		if len(row) < 3 {
			continue
		}
		def, _ := row[2].(string)
		if refuseDocumentTables(def, postgres) != "" || (row[0] == "function" && pgDynamicSQLRe.MatchString(def)) {
			return fmt.Sprintf("%v %v", row[0], row[1]), nil
		}
	}
	memorySQLScopeChecked.Store(ck, true)
	return "", nil
}

var leadingDropRe = regexp.MustCompile(`(?i)^\s*drop\b`)

// guardDocumentTables refuses a Memory SQL statement that reaches a document
// table, and audits the refusal like any failed statement. A DROP skips the
// planted-object check: it has passed the name check, so it cannot touch a
// document table, and it is how an agent removes the object the check found.
func (m *Memory) guardDocumentTables(ctx context.Context, op, scope, scopeID string, key sqlmem.ScopeKey, stmt string) (tools.Result, bool) {
	why := refuseDocumentTables(stmt, m.SqlMem.Tier() == "postgres")
	if why == "" && !leadingDropRe.MatchString(stmt) {
		obj, err := plantedDocumentObject(ctx, m.SqlMem, key)
		if err != nil {
			m.auditSql(ctx, op, scope, scopeID, stmt, 0, 0, err)
			return errFrom(fmt.Sprintf("%s: %s", op, err), err), true
		}
		if obj != "" {
			why = fmt.Sprintf("this scope's %s reaches a document table; drop it before using Memory SQL here", obj)
		}
	}
	if why == "" {
		return tools.Result{}, false
	}
	m.auditSql(ctx, op, scope, scopeID, stmt, 0, 0, errors.New(why))
	return errBusiness(op+": document_tables_reserved: "+why,
		"Read and change documents, chunks and statements through the Document tool; keep this agent's own tables under other names."), true
}
