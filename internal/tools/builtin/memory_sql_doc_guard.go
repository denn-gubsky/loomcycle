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
// SQL therefore never names a document table or index, for reads or for writes;
// Document ops are the only way in (owner decision, 2026-10-30).
//
// The statement is read as one stream of tokens, comments dropped, and every
// check reads that stream, so a comment cannot split a name or a call:
//   - every identifier, in each quoting form, and on sqlite every 'string'
//     (sqlite takes one as a table name in FROM, JOIN, INTO, …), is checked
//     against the reserved names; so are the statistics catalogs, which hold
//     sampled column values of every table the role can read;
//   - on postgres, CREATE FUNCTION/PROCEDURE and the core functions that run a
//     query given as text are refused: SQL built from strings has no name to
//     check;
//   - before the first Memory SQL call on a scope, its views, triggers, rules
//     and (postgres) functions are checked for a reserved name or, in a
//     function, for EXECUTE, since this guard never saw them made.
//
// sqlite cannot build SQL from strings, so there this is complete. On postgres
// the engine enforces the boundary as well: the document tables live on their own
// plane, a schema and login role the agent's role cannot use
// (sqlmem.Manager.DocPlane), so this check is defense in depth there and covers
// a scope from before the split until the Document tool's first use moves it.

// documentTableExtras are names Document creates outside docSchemaDDL:
// chunk_assets, whose DDL is built per tier (BLOB or BYTEA) in ensureSchema, and
// the table migrateEdgeStatements builds and renames into chunk_edges.
var documentTableExtras = []string{"chunk_assets", "chunk_edges_statements"}

// documentIndexExtras are the indexes Document's migrations create outside
// docSchemaDDL. Dropping one would let duplicates in or slow every document op.
var documentIndexExtras = []string{
	"chunk_edge_evidence_chunk", "chunk_edge_evidence_edge", "chunk_edges_from_kind",
	"chunk_edges_statement", "chunk_edges_status", "chunks_doc_anchor", "chunks_identity_key",
	"chunks_stable_id", "chunks_validity", "chunks_work_key", "documents_type_status",
}

var (
	createTableNameRe = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	createIndexNameRe = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
)

// statsCatalogs hold sampled values of the columns of every table the role may
// read — the document tables included.
var statsCatalogs = []string{
	"pg_stats", "pg_stats_ext", "pg_stats_ext_exprs", "pg_statistic", "pg_statistic_ext", "pg_statistic_ext_data",
	"sqlite_stat1", "sqlite_stat2", "sqlite_stat3", "sqlite_stat4",
}

// documentTableNames is every table the Document tool creates, lower-cased, in
// docSchemaDDL order: what the reserved set protects, and what moves to the
// document plane of a scope created before it had one.
var documentTableNames = func() []string {
	var tables []string
	for _, ddl := range docSchemaDDL {
		for _, m := range createTableNameRe.FindAllStringSubmatch(ddl, -1) {
			tables = append(tables, strings.ToLower(m[1]))
		}
	}
	return append(tables, documentTableExtras...)
}()

// documentTables is the reserved set, lower-cased: every table and index
// docSchemaDDL creates, each table's postgres primary-key index, the extras of both, and
// the statistics catalogs. TestMemorySQLGuard_CoversEveryDocumentTable holds it
// to what a fully migrated scope actually has, on both tiers.
var documentTables = func() map[string]bool {
	set := map[string]bool{}
	for _, ddl := range docSchemaDDL {
		for _, m := range createIndexNameRe.FindAllStringSubmatch(ddl, -1) {
			set[strings.ToLower(m[1])] = true
		}
	}
	for _, t := range documentTableNames {
		set[t] = true
		set[t+"_pkey"] = true
	}
	for _, n := range documentIndexExtras {
		set[n] = true
	}
	for _, n := range statsCatalogs {
		set[n] = true
	}
	return set
}()

// pgQueryTextFns are the core functions that run a query, or read a table,
// named by a string the guard cannot see through, and set_config, which could
// turn standard_conforming_strings off and so move where a literal ends.
var pgQueryTextFns = map[string]bool{
	"query_to_xml": true, "query_to_xmlschema": true, "query_to_xml_and_xmlschema": true,
	"cursor_to_xml": true, "cursor_to_xmlschema": true,
	"table_to_xml": true, "table_to_xmlschema": true, "table_to_xml_and_xmlschema": true,
	"schema_to_xml": true, "schema_to_xmlschema": true, "schema_to_xml_and_xmlschema": true,
	"database_to_xml": true, "database_to_xmlschema": true, "database_to_xml_and_xmlschema": true,
	"ts_stat": true, "ts_rewrite": true, "set_config": true,
}

// sqlTok is one token of a statement: a bare word, a quoted identifier, a
// string literal, or one punctuation byte. val is lower-cased for the first two.
type sqlTok struct {
	kind byte // 'w' word, 'q' quoted identifier, 's' string, 'p' punctuation
	val  string
}

// refuseDocumentTables returns the reason a Memory SQL statement is refused, or
// "" when it reaches no document table.
func refuseDocumentTables(stmt string, postgres bool) string {
	toks, bad := sqlTokens(stmt, postgres)
	if bad != "" {
		return bad
	}
	for i, t := range toks {
		name := t.kind == 'w' || t.kind == 'q' || (t.kind == 's' && !postgres)
		if name && documentTables[t.val] {
			return fmt.Sprintf("%q is a document table or index, or a statistics catalog: Memory SQL may not read or change it", t.val)
		}
		if !postgres {
			continue
		}
		// A DO block runs a string as code. sqlmem's leading-keyword allow-list
		// already refuses it; this keeps the guard whole on its own.
		if t.kind == 'w' && t.val == "do" && i+1 < len(toks) && toks[i+1].kind == 's' {
			return "Memory SQL cannot run a DO block on this server: it could build SQL that reaches the document tables"
		}
		if t.kind == 'w' && t.val == "create" {
			j := i + 1
			if j+1 < len(toks) && toks[j].val == "or" && toks[j+1].val == "replace" {
				j += 2
			}
			if j < len(toks) && toks[j].kind == 'w' && (toks[j].val == "function" || toks[j].val == "procedure") {
				return "Memory SQL cannot create a function or procedure on this server: its body could reach the document tables"
			}
		}
		if (t.kind == 'w' || t.kind == 'q') && pgQueryTextFns[t.val] && i+1 < len(toks) && toks[i+1].kind == 'p' && toks[i+1].val == "(" {
			return fmt.Sprintf("Memory SQL cannot call %s on this server: it runs SQL given as text, which could reach the document tables", t.val)
		}
	}
	return ""
}

// sqlTokens reads stmt into tokens, dropping comments and whitespace. A string
// literal is read to where the engine ends it — postgres E'…' with its
// backslash escapes — so nothing after it is mistaken for text. The second
// value is set when stmt uses a form the guard cannot read safely.
func sqlTokens(s string, postgres bool) ([]sqlTok, string) {
	var out []sqlTok
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
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
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
			out = append(out, sqlTok{'s', strings.ToLower(v)})
			i = next
		case c == '"':
			v, next := quoted(i, '"')
			out = append(out, sqlTok{'q', strings.ToLower(v)})
			i = next
		case !postgres && c == '`':
			v, next := quoted(i, '`')
			out = append(out, sqlTok{'q', strings.ToLower(v)})
			i = next
		case !postgres && c == '[':
			j := strings.IndexByte(s[i+1:], ']')
			if j < 0 {
				out = append(out, sqlTok{'q', strings.ToLower(s[i+1:])})
				i = len(s)
			} else {
				out = append(out, sqlTok{'q', strings.ToLower(s[i+1 : i+1+j])})
				i += j + 2
			}
		case postgres && c == '$':
			tag, ok := dollarTag(s, i)
			if !ok {
				out = append(out, sqlTok{'p', "$"})
				i++
				continue
			}
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				i = len(s)
			} else {
				i += len(tag) + end + len(tag)
			}
			out = append(out, sqlTok{'s', ""})
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
				out = append(out, sqlTok{'s', ""})
				continue
			}
			out = append(out, sqlTok{'w', strings.ToLower(word)})
			i = j
		case c >= '0' && c <= '9':
			for i < len(s) && isIdent(s[i]) {
				i++
			}
			out = append(out, sqlTok{'s', ""})
		default:
			out = append(out, sqlTok{'p', string(c)})
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

// plantedDocumentObject names a view, trigger, rule or function in the scope
// that reaches a document table, or "" when there is none. A scope found clean
// is remembered in checked: nothing made through Memory SQL afterwards can
// name a document table, so one check per scope is enough.
func plantedDocumentObject(ctx context.Context, m *sqlmem.Manager, checked *sync.Map, key sqlmem.ScopeKey) (string, error) {
	ck := key.Tenant + "\x1f" + key.Scope + "\x1f" + key.ScopeID
	if _, ok := checked.Load(ck); ok {
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
	checked.Store(ck, true)
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
		obj, err := plantedDocumentObject(ctx, m.SqlMem, &m.docGuardChecked, key)
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
