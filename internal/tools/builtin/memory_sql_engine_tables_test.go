package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// On the sqlite tier the Document plane shares the scope's file, and
// sqlite_dbpage reads and writes that file page by page, with no table name for
// the document-table guard to match. A read hands Memory SQL the raw text of
// every chunk; a write flipped a proposed statement to asserted with no review.
func TestMemorySQL_SqliteDbpageIsRefused(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "Paged")
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatalf("propose_statement: %s", r.Text)
		}
		mustMemSQL(t, m, ctx, "sql_exec", `CREATE TABLE notes (id INT, body TEXT)`)
		sqlite := d.SqlMem.Tier() != "postgres"

		// The chunk body stmtDoc wrote; finding it on a page is the leak.
		const bodyText = "Article 12"
		cases := []struct{ op, stmt string }{
			{"sql_query", `SELECT count(*) FROM sqlite_dbpage WHERE instr(data, '` + bodyText + `') > 0`},
			{"sql_query", `SELECT data FROM sqlite_dbpage('main')`},
			{"sql_query", `SELECT data FROM main.sqlite_dbpage`},
			{"sql_query", `SELECT data FROM "SQLITE_DBPAGE"`},
			{"sql_query", `SELECT data FROM main./**/sqlite_dbpage`},
			{"sql_exec", `INSERT INTO notes SELECT pgno, data FROM sqlite_dbpage`},
			{"sql_exec", `UPDATE sqlite_dbpage SET data = CAST(replace(data, 'proposed', 'asserted') AS BLOB) WHERE instr(data, 'proposed') > 0`},
			{"sql_exec", `CREATE VIEW pages AS SELECT data FROM sqlite_dbpage`},
			{"sql_exec", `CREATE VIRTUAL TABLE pages USING sqlite_dbpage`},
		}
		if sqlite {
			cases = append(cases, []struct{ op, stmt string }{
				{"sql_query", `SELECT data FROM 'sqlite_dbpage'`},
				{"sql_query", `SELECT data FROM [sqlite_dbpage]`},
				{"sql_query", "SELECT data FROM `sqlite_dbpage`"},
				{"sql_query", `SELECT data FROM main.'sqlite_dbpage'`},
				{"sql_exec", `UPDATE 'sqlite_dbpage' SET data = CAST(replace(data, 'proposed', 'asserted') AS BLOB) WHERE instr(data, 'proposed') > 0`},
			}...)
		}
		for _, c := range cases {
			text, isErr := memSQL(t, m, ctx, c.op, c.stmt)
			if !isErr || !strings.Contains(text, "document_tables_reserved") {
				t.Errorf("%s %q: want document_tables_reserved, got error=%v %s", c.op, c.stmt, isErr, clip(text))
			}
		}

		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT status FROM chunk_edges WHERE id = ?`, out["id"])
		if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "proposed" {
			t.Fatalf("statement after the refused page writes = %v, %v; want it still proposed", res, err)
		}
		if !sqlite {
			return
		}
		res, err = d.SqlMem.Query(ctx, key, `SELECT count(*) FROM notes WHERE instr(body, '`+bodyText+`') > 0`, nil)
		if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != "0" {
			t.Fatalf("pages copied into the agent's own table holding a chunk body = %v, %v; want none", res, err)
		}
	})
}

// The other ways below the tables: dbstat (per-page statistics of every table),
// the schema catalog, the statistics catalogs, and the pragma_* table-valued
// functions. A bare PRAGMA, ATTACH and VACUUM never reach the engine: the
// statement floor refuses them by their leading keyword.
func TestMemorySQL_DbstatAndPragmaFunctionsAreRefused(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		stmtDoc(t, d, ctx, "Catalogued")
		mustMemSQL(t, m, ctx, "sql_exec", `CREATE TABLE notes (id INT, body TEXT)`)
		sqlite := d.SqlMem.Tier() != "postgres"

		reserved := []struct{ op, stmt string }{
			{"sql_query", `SELECT name, pgsize FROM dbstat WHERE name = 'notes'`},
			{"sql_query", `SELECT * FROM dbstat('main')`},
			{"sql_query", `SELECT * FROM main.dbstat`},
			{"sql_query", `SELECT * FROM sqlite_dbstat`},
			{"sql_exec", `CREATE VIRTUAL TABLE st USING dbstat`},
			{"sql_query", `SELECT name, sql FROM sqlite_master`},
			{"sql_query", `SELECT name, sql FROM sqlite_schema WHERE type = 'table'`},
			{"sql_query", `SELECT * FROM temp.sqlite_master`},
			{"sql_query", `SELECT * FROM sqlite_temp_master`},
			{"sql_query", `SELECT * FROM sqlite_temp_schema`},
			{"sql_query", `SELECT * FROM sqlite_stat1`},
			{"sql_query", `SELECT * FROM sqlite_stat4`},
			{"sql_query", `SELECT * FROM pragma_table_info('notes')`},
			{"sql_query", `SELECT * FROM pragma_page_count`},
			{"sql_query", `SELECT * FROM pragma_database_list`},
			{"sql_query", `SELECT * FROM main.pragma_index_list('notes')`},
			{"sql_query", `SELECT * FROM "PRAGMA_TABLE_INFO"('notes')`},
			{"sql_query", `SELECT id FROM notes WHERE id IN (SELECT cid FROM pragma_table_info('notes'))`},
			{"sql_exec", `CREATE VIEW cols AS SELECT * FROM pragma_table_info('notes')`},
			{"sql_exec", `INSERT INTO notes SELECT pageno, name FROM dbstat`},
		}
		if sqlite {
			reserved = append(reserved, []struct{ op, stmt string }{
				{"sql_query", `SELECT * FROM 'dbstat'`},
				{"sql_query", `SELECT * FROM [sqlite_master]`},
				{"sql_query", "SELECT * FROM `sqlite_schema`"},
				{"sql_query", `SELECT * FROM 'pragma_page_count'`},
				{"sql_query", `SELECT * FROM 'pragma_table_info'('notes')`},
				{"sql_query", `SELECT * FROM main.'pragma_table_info'('notes')`},
			}...)
		}
		for _, c := range reserved {
			text, isErr := memSQL(t, m, ctx, c.op, c.stmt)
			if !isErr || !strings.Contains(text, "document_tables_reserved") {
				t.Errorf("%s %q: want document_tables_reserved, got error=%v %s", c.op, c.stmt, isErr, clip(text))
			}
		}

		floor := []struct{ op, stmt string }{
			{"sql_exec", `PRAGMA writable_schema = ON`},
			{"sql_query", `PRAGMA page_count`},
			{"sql_query", `PRAGMA database_list`},
			{"sql_query", `PRAGMA table_info(chunks)`},
			{"sql_exec", `ATTACH DATABASE 'other.db' AS other`},
			{"sql_exec", `VACUUM`},
		}
		for _, c := range floor {
			if text, isErr := memSQL(t, m, ctx, c.op, c.stmt); !isErr {
				t.Errorf("%s %q ran: %s", c.op, c.stmt, clip(text))
			}
		}

		// The agent's own tables and ordinary functions are untouched.
		mustMemSQL(t, m, ctx, "sql_exec", `INSERT INTO notes VALUES (1, 'pragma notes on the master schedule')`)
		mustMemSQL(t, m, ctx, "sql_query", `SELECT id, body FROM notes WHERE body LIKE '%master%'`)
		if sqlite {
			mustMemSQL(t, m, ctx, "sql_query", `SELECT sqlite_version()`)
			mustMemSQL(t, m, ctx, "sql_query", `SELECT value FROM json_each('[1,2]')`)
		}
	})
}

// A view made before the guard knew these names keeps the scope refused to
// Memory SQL until it is dropped. sqlite_dbpage and dbstat cannot be read
// through a view at all (the engine marks them direct-only), but the schema
// catalog and the pragma_* functions can, so a view over sqlite_master would
// hand Memory SQL every document table's definition.
func TestMemorySQL_PlantedCatalogViewBlocksUntilDropped(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		if d.SqlMem.Tier() == "postgres" {
			t.Skip("sqlite_master exists on the sqlite tier only")
		}
		stmtDoc(t, d, ctx, "Planted catalog")
		key, _, _ := d.resolveScope(ctx, "user")
		// Straight to the engine, as an agent could before the guard named it.
		if _, err := d.SqlMem.Exec(ctx, key, `CREATE VIEW schemav AS SELECT name, sql FROM sqlite_master`, nil, 0); err != nil {
			t.Fatalf("plant schemav: %v", err)
		}
		text, isErr := memSQL(t, m, ctx, "sql_query", `SELECT * FROM schemav`)
		if !isErr || !strings.Contains(text, "document_tables_reserved") || !strings.Contains(text, "schemav") {
			t.Fatalf("Memory SQL beside planted schemav: error=%v %s; want refused naming it", isErr, clip(text))
		}
		mustMemSQL(t, m, ctx, "sql_exec", `DROP VIEW schemav`)
		mustMemSQL(t, m, ctx, "sql_query", `SELECT 1`)
	})
}

// A virtual table over sqlite_dbpage or dbstat, made before the guard named
// them, is a table, not a view: reading it is reading the pages directly, which
// defensive mode does not stop. The planted-object check covers it, so the
// scope is refused to Memory SQL until it is dropped.
func TestMemorySQL_PlantedEngineVirtualTableBlocksUntilDropped(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		if d.SqlMem.Tier() == "postgres" {
			t.Skip("sqlite_dbpage and dbstat exist on the sqlite tier only")
		}
		stmtDoc(t, d, ctx, "Planted vtab")
		key, _, _ := d.resolveScope(ctx, "user")
		for _, p := range []struct{ create, name, read, drop string }{
			{`CREATE VIRTUAL TABLE pg USING sqlite_dbpage`, "pg", `SELECT count(*) FROM pg WHERE instr(data, 'Article 12') > 0`, `DROP TABLE pg`},
			{`CREATE VIRTUAL TABLE st USING dbstat`, "st", `SELECT name, pgsize FROM st`, `DROP TABLE st`},
		} {
			// Straight to the engine, as an agent could before the guard named it.
			if _, err := d.SqlMem.Exec(ctx, key, p.create, nil, 0); err != nil {
				t.Fatalf("plant %s: %v", p.name, err)
			}
			text, isErr := memSQL(t, m, ctx, "sql_query", p.read)
			if !isErr || !strings.Contains(text, "document_tables_reserved") || !strings.Contains(text, p.name) {
				t.Fatalf("Memory SQL reading planted %s: error=%v %s; want refused naming it", p.name, isErr, clip(text))
			}
			mustMemSQL(t, m, ctx, "sql_exec", p.drop)
		}
		mustMemSQL(t, m, ctx, "sql_query", `SELECT 1`)
		// An agent's own virtual table names nothing reserved and stays usable.
		mustMemSQL(t, m, ctx, "sql_exec", `CREATE VIRTUAL TABLE notes_fts USING fts5(body)`)
		mustMemSQL(t, m, ctx, "sql_exec", `INSERT INTO notes_fts (body) VALUES ('alpha')`)
		fresh := &Memory{Store: d.Store, SqlMem: d.SqlMem}
		mustMemSQL(t, fresh, ctx, "sql_query", `SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'alpha'`)
	})
}

// An agent refused for reading the schema catalog, typically to list its own
// tables, is told how to do without it, not to rename tables it never named.
func TestMemorySQL_EngineTableRefusalHasItsOwnHint(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		stmtDoc(t, d, ctx, "Hinted")
		hint := func(stmt string) string {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"op": "sql_query", "scope": "user", "statement": stmt})
			res, err := m.Execute(ctx, body)
			if err != nil || !res.IsError || res.Error == nil {
				t.Fatalf("%q: want a classified refusal, got %+v, %v", stmt, res, err)
			}
			return res.Error.Description
		}
		if h := hint(`SELECT name FROM sqlite_master WHERE type = 'table'`); !strings.Contains(h, "Keep track of the tables") || strings.Contains(h, "under other names") {
			t.Errorf("engine-table refusal hint = %q; want advice on tracking the agent's own tables", h)
		}
		if h := hint(`SELECT * FROM chunks`); !strings.Contains(h, "Document tool") {
			t.Errorf("document-table refusal hint = %q; want it to point at the Document tool", h)
		}
	})
}

// clip keeps a failure readable: a page read returns whole pages of the file.
func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
