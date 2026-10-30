package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// Memory SQL and Document share one database per scope. These tests run Memory
// SQL against the very scope a Document wrote, on both tiers.

func guardBothTiers(t *testing.T, fn func(t *testing.T, d *Document, m *Memory, ctx context.Context)) {
	t.Helper()
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ctx = withSqlScopes(ctx, "user")
		m := &Memory{Store: d.Store, SqlMem: d.SqlMem}
		docKey, _, _ := d.resolveScope(ctx, "user")
		memKey := sqlmem.ScopeKey{Tenant: sqlScopeTenant(ctx), Scope: "user", ScopeID: "u1"}
		if docKey != memKey {
			t.Fatalf("Document's user scope %+v is not Memory's %+v: these tests would not share a database", docKey, memKey)
		}
		fn(t, d, m, ctx)
	})
}

func memSQL(t *testing.T, m *Memory, ctx context.Context, op, stmt string) (string, bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"op": op, "scope": "user", "statement": stmt})
	res, err := m.Execute(ctx, body)
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	return res.Text, res.IsError
}

func mustMemSQL(t *testing.T, m *Memory, ctx context.Context, op, stmt string) {
	t.Helper()
	if text, isErr := memSQL(t, m, ctx, op, stmt); isErr {
		t.Fatalf("%s %q refused: %s", op, stmt, text)
	}
}

func TestMemorySQLGuard_DocumentTablesAreRefused(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "Guarded")
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatalf("propose_statement: %s", r.Text)
		}
		mustMemSQL(t, m, ctx, "sql_exec", `CREATE TABLE notes (id INT, body TEXT)`)
		postgres := d.SqlMem.Tier() == "postgres"

		cases := []struct{ op, stmt string }{
			{"sql_exec", `UPDATE chunk_edges SET status = 'asserted'`},
			{"sql_exec", `INSERT INTO chunk_edges (id) VALUES ('x')`},
			{"sql_exec", `DELETE FROM document_leases`},
			{"sql_exec", `CREATE VIEW leak AS SELECT * FROM chunk_edges`},
			{"sql_exec", `DROP TABLE chunk_tags`},
			{"sql_exec", `ALTER TABLE chunks ADD COLUMN x INT`},
			{"sql_query", `SELECT * FROM chunks`},
			{"sql_query", `SELECT * FROM "CHUNK_EDGES"`},
			{"sql_query", `SELECT * FROM/**/chunks`},
			{"sql_query", "SELECT * FROM -- a comment\n documents"},
			{"sql_query", `WITH x AS (SELECT id FROM documents) SELECT * FROM x`},
			{"sql_query", `SELECT id FROM notes WHERE id IN (SELECT 1 FROM chunk_edge_evidence)`},
			{"sql_exec", `DROP INDEX chunks_stable_id`},
		}
		if postgres {
			cases = append(cases, []struct{ op, stmt string }{
				{"sql_query", `SELECT query_to_xml('select * from chunk' || '_edges', true, false, '')`},
				{"sql_query", `SELECT schema_to_xml(current_schema(), true, false, '')`},
				{"sql_query", `SELECT "table_to_xml"('chunks'::regclass, true, false, '')`},
				{"sql_query", `SELECT set_config('standard_conforming_strings', 'off', false)`},
				{"sql_exec", `CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$`},
				{"sql_exec", `CREATE OR REPLACE PROCEDURE p() LANGUAGE sql AS $$ SELECT 1 $$`},
				{"sql_query", `SELECT * FROM U&"chunk\005fedges"`},
				{"sql_query", `SELECT * FROM u&"chunk!005fedges" UESCAPE '!'`},
				{"sql_query", `SELECT U&'\0063hunks' AS a FROM notes`},
				{"sql_exec", `DO $$ BEGIN EXECUTE 'UPDATE chunk' || '_edges SET status = ''asserted'''; END $$`},
				{"sql_query", `SELECT E'\'' AS a, id FROM notes, chunks`},
				{"sql_query", `SELECT $q$ text $q$ AS a, id FROM chunks`},
				{"sql_query", `SELECT 1 /* outer /* nested */ still a comment */ FROM chunks`},
				{"sql_exec", `/* lead */ CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$`},
				{"sql_query", `SELECT query_to_xml/**/('select 1', true, false, '')`},
				{"sql_query", `SELECT pg_catalog . ts_stat ('select 1')`},
				{"sql_query", `SELECT most_common_vals FROM pg_stats`},
			}...)
		} else {
			cases = append(cases, []struct{ op, stmt string }{
				{"sql_query", `SELECT * FROM 'chunks'`},
				{"sql_query", `SELECT * FROM notes, 'chunks'`},
				{"sql_query", `SELECT * FROM [chunks]`},
				{"sql_query", "SELECT * FROM `chunks`"},
				{"sql_query", `SELECT * FROM main.chunk_edges`},
				{"sql_query", `SELECT * FROM main.'chunk_edges'`},
				{"sql_exec", `UPDATE 'chunk_edges' SET status = 'asserted'`},
				{"sql_exec", `INSERT INTO 'chunk_edges' (id) VALUES ('x')`},
				{"sql_exec", `CREATE TRIGGER tr AFTER INSERT ON notes BEGIN UPDATE chunk_edges SET status = 'asserted'; END`},
				{"sql_query", `SELECT * FROM (SELECT 1 WHERE 1), 'chunks'`},
				{"sql_query", `SELECT * FROM notes LEFT OUTER JOIN 'chunk_edges' ON 1`},
				{"sql_query", `SELECT * FROM sqlite_stat1`},
			}...)
		}
		for _, c := range cases {
			text, isErr := memSQL(t, m, ctx, c.op, c.stmt)
			if !isErr || !strings.Contains(text, "document_tables_reserved") {
				t.Errorf("%s %q: want document_tables_reserved, got error=%v %s", c.op, c.stmt, isErr, text)
			}
		}

		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT status FROM chunk_edges WHERE id = ?`, out["id"])
		if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "proposed" {
			t.Fatalf("statement after the refused writes = %v, %v; want it still proposed", res, err)
		}
	})
}

// An agent's own tables work as before, beside the document tables.
func TestMemorySQLGuard_AgentTablesStillWork(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		stmtDoc(t, d, ctx, "Beside")
		for _, s := range []string{
			`CREATE TABLE notes (id INT, kind TEXT, body TEXT)`,
			`INSERT INTO notes VALUES (1, 'document', 'a chunk of text')`,
			`UPDATE notes SET kind = 'edges' WHERE id = 1`,
			`CREATE VIEW recent AS SELECT * FROM notes`,
			`CREATE INDEX notes_kind ON notes (kind)`,
		} {
			mustMemSQL(t, m, ctx, "sql_exec", s)
		}
		mustMemSQL(t, m, ctx, "sql_query", `SELECT * FROM recent WHERE kind IN ('edges', 'document') -- chunks`)
		mustMemSQL(t, m, ctx, "sql_exec", `DROP VIEW recent`)
		mustMemSQL(t, m, ctx, "sql_exec", `DROP TABLE notes`)
		// The Document tool's own raw read is untouched.
		if _, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT count(*) FROM chunks"}`); r.IsError {
			t.Fatalf("Document query_chunks: %s", r.Text)
		}
	})
}

// A view, trigger or function made before the guard existed is not seen by the
// name check, so the scope is refused to Memory SQL until it is dropped.
func TestMemorySQLGuard_PlantedObjectBlocksUntilDropped(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		stmtDoc(t, d, ctx, "Planted")
		key, _, _ := d.resolveScope(ctx, "user")
		planted := []struct{ create, name, drop string }{
			{`CREATE VIEW leak AS SELECT * FROM chunk_edges`, "leak", `DROP VIEW leak`},
		}
		if d.SqlMem.Tier() == "postgres" {
			planted = append(planted, struct{ create, name, drop string }{
				`CREATE FUNCTION leakfn() RETURNS void LANGUAGE plpgsql AS $$ BEGIN EXECUTE 'select 1'; END $$`,
				"leakfn", `DROP FUNCTION leakfn()`})
		}
		for _, p := range planted {
			// Straight to the engine, as an agent could before this guard.
			if _, err := d.SqlMem.Exec(ctx, key, p.create, nil, 0); err != nil {
				t.Fatalf("plant %s: %v", p.name, err)
			}
			text, isErr := memSQL(t, m, ctx, "sql_query", `SELECT 1`)
			if !isErr || !strings.Contains(text, "document_tables_reserved") || !strings.Contains(text, p.name) {
				t.Fatalf("Memory SQL beside planted %s: error=%v %s; want refused naming it", p.name, isErr, text)
			}
			mustMemSQL(t, m, ctx, "sql_exec", p.drop)
		}
		mustMemSQL(t, m, ctx, "sql_query", `SELECT 1`)
	})
}

// The reserved set covers every table and index a fully migrated document scope
// has, so one a later migration adds cannot slip past the guard.
func TestMemorySQLGuard_CoversEveryDocumentTable(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, _ := d.resolveScope(ctx, "user")
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatal(err)
		}
		q := `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`
		if d.SqlMem.Tier() == "postgres" {
			q = `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()`
		}
		res, err := d.SqlMem.Query(ctx, key, q, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) < 10 {
			t.Fatalf("the scope has %d tables; the catalog query found too few to check", len(res.Rows))
		}
		for _, row := range res.Rows {
			name, _ := row[0].(string)
			if !documentTables[strings.ToLower(name)] {
				t.Errorf("document table %q is not in the reserved set: add it to docSchemaDDL or documentTableExtras", name)
			}
		}
		// sqlite's own autoindexes cannot be dropped, so they need no name.
		q = `SELECT name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_autoindex_%'`
		if d.SqlMem.Tier() == "postgres" {
			q = `SELECT indexname FROM pg_indexes WHERE schemaname = current_schema()`
		}
		res, err = d.SqlMem.Query(ctx, key, q, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) < 10 {
			t.Fatalf("the scope has %d indexes; the catalog query found too few to check", len(res.Rows))
		}
		for _, row := range res.Rows {
			name, _ := row[0].(string)
			if !documentTables[strings.ToLower(name)] {
				t.Errorf("document index %q is not in the reserved set: create it in docSchemaDDL or add it to documentIndexExtras", name)
			}
		}
	})
}

// A scope found clean is remembered by the Memory tool that checked it, never
// process-wide: a scope restored with a planted view is checked again by the
// tool serving it. (The process-wide cache this replaced was keyed by the
// manager's address, which a later manager can reuse.)
func TestMemorySQLGuard_CleanScopeIsRememberedPerTool(t *testing.T) {
	guardBothTiers(t, func(t *testing.T, d *Document, m *Memory, ctx context.Context) {
		stmtDoc(t, d, ctx, "Remembered")
		mustMemSQL(t, m, ctx, "sql_query", `SELECT 1`)
		key, _, _ := d.resolveScope(ctx, "user")
		if _, err := d.SqlMem.Exec(ctx, key, `CREATE VIEW leak AS SELECT * FROM chunk_edges`, nil, 0); err != nil {
			t.Fatal(err)
		}
		fresh := &Memory{Store: d.Store, SqlMem: d.SqlMem}
		text, isErr := memSQL(t, fresh, ctx, "sql_query", `SELECT 1`)
		if !isErr || !strings.Contains(text, "leak") {
			t.Fatalf("a fresh tool over the same scope: error=%v %s; want refused naming the planted view", isErr, text)
		}
	})
}
