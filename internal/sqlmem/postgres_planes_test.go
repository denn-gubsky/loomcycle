package sqlmem

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// postgres_planes_test.go — the document plane: a second schema and login role
// per scope that an agent's SQL cannot reach (scopePlane).

func pgPlaneSchemas(t *testing.T, key ScopeKey) (agent, doc string) {
	t.Helper()
	agent, _, err := pgScopeNames(key.wholeScope())
	if err != nil {
		t.Fatal(err)
	}
	d := key
	d.plane = planeDoc
	doc, _, err = pgScopeNames(d)
	if err != nil {
		t.Fatal(err)
	}
	return agent, doc
}

// The agent's role cannot read or write the document plane, by name or by
// schema-qualified name, and the document role cannot reach the agent plane. The
// engine refuses it: no statement check is involved here.
func TestPostgresPlanes_NeitherRoleReachesTheOtherPlane(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")
	doc := m.DocPlane()
	agentSchema, docSchema := pgPlaneSchemas(t, key)

	if _, err := doc.Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY, body TEXT)`, nil, 0); err != nil {
		t.Fatalf("doc plane create: %v", err)
	}
	if _, err := doc.Exec(ctx, key, `INSERT INTO chunks VALUES ('c1', 'secret')`, nil, 0); err != nil {
		t.Fatalf("doc plane insert: %v", err)
	}
	if _, err := m.Exec(ctx, key, `CREATE TABLE notes (id TEXT)`, nil, 0); err != nil {
		t.Fatalf("agent plane create: %v", err)
	}

	for _, stmt := range []string{
		`SELECT * FROM chunks`,
		`SELECT * FROM "` + docSchema + `".chunks`,
	} {
		if res, err := m.Query(ctx, key, stmt, nil); err == nil {
			t.Errorf("agent plane read %q: %v, want refused by the engine", stmt, res.Rows)
		}
	}
	if _, err := m.Exec(ctx, key, `UPDATE "`+docSchema+`".chunks SET body = 'x'`, nil, 0); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("agent plane write to the doc plane: %v, want permission denied", err)
	}
	if _, err := doc.Query(ctx, key, `SELECT * FROM "`+agentSchema+`".notes`, nil); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("doc plane read of the agent plane: %v, want permission denied", err)
	}
	res, err := doc.Query(ctx, key, `SELECT body FROM chunks`, nil)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "secret" {
		t.Fatalf("doc plane reads its own table: %v %v", res, err)
	}
}

// A scope's quota, drop and existence cover both planes.
func TestPostgresPlanes_TheScopeIsBothPlanes(t *testing.T) {
	m, raw := pgTestManager(t, Config{QuotaBytes: 64 * 1024})
	ctx := context.Background()
	key := agentKey("t1", "writer")
	doc := m.DocPlane()
	agentSchema, docSchema := pgPlaneSchemas(t, key)

	if _, err := doc.Exec(ctx, key, `CREATE TABLE big (b TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Exec(ctx, key, `INSERT INTO big SELECT repeat('x', 2000) || g FROM generate_series(1, 200) g`, nil, 0); err != nil {
		t.Fatal(err)
	}
	// The doc plane alone is past the quota, so the agent plane's write is refused.
	if _, err := m.Exec(ctx, key, `CREATE TABLE notes (id TEXT)`, nil, 0); err == nil ||
		!strings.Contains(err.Error(), "quota") {
		t.Errorf("agent plane write with the doc plane over quota: %v, want the quota error", err)
	}
	if ok, err := m.ScopeExists(ctx, key); err != nil || !ok {
		t.Fatalf("ScopeExists = %v, %v", ok, err)
	}

	removed, err := m.DropScope(ctx, key)
	if err != nil || !removed {
		t.Fatalf("DropScope = %v, %v", removed, err)
	}
	for _, s := range []string{agentSchema, docSchema} {
		var n int
		if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = $1`, s).Scan(&n); err != nil || n != 0 {
			t.Errorf("schema %s after DropScope: %d (%v), want gone", s, n, err)
		}
	}
	var roles int
	_ = raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $2)`,
		"sqlmem_r_"+strings.TrimPrefix(agentSchema, "sqlmem_s_"), "sqlmem_dr_"+strings.TrimPrefix(agentSchema, "sqlmem_s_")).Scan(&roles)
	if roles != 0 {
		t.Errorf("%d scope roles left after DropScope, want 0", roles)
	}
}

// A document table on the agent plane, as a scope from before the split keeps
// it, moves to the doc plane with its rows, and nothing an agent attached to it
// comes along: not its trigger, not a column DEFAULT calling the agent's
// function, not a column the agent added.
func TestPostgresPlanes_AdoptionCopiesRowsAndNothingElse(t *testing.T) {
	m, raw := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")
	doc := m.DocPlane()
	agentSchema, docSchema := pgPlaneSchemas(t, key)

	for _, stmt := range []string{
		`CREATE TABLE chunks (id TEXT PRIMARY KEY, n BIGINT, b BYTEA, body TEXT)`,
		`INSERT INTO chunks VALUES ('c1', 7, '\x00ff', 'one'), ('c2', NULL, NULL, 'two''s')`,
		`CREATE TABLE spied (who TEXT)`,
		`CREATE FUNCTION spy() RETURNS TEXT LANGUAGE plpgsql AS $$ BEGIN INSERT INTO spied VALUES (current_user); RETURN 'x'; END $$`,
		`CREATE FUNCTION spy_trigger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO spied VALUES (current_user); RETURN NEW; END $$`,
		`CREATE TRIGGER planted BEFORE INSERT ON chunks FOR EACH ROW EXECUTE FUNCTION spy_trigger()`,
		`ALTER TABLE chunks ADD COLUMN extra TEXT DEFAULT spy()`,
		`ALTER TABLE chunks ALTER COLUMN body SET DEFAULT spy()`,
	} {
		if _, err := m.Exec(ctx, key, stmt, nil, 0); err != nil {
			t.Fatalf("stage the agent plane: %s: %v", stmt, err)
		}
	}
	if _, err := m.Exec(ctx, key, `DELETE FROM spied`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY, n BIGINT, b BYTEA, body TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}

	held, err := m.AgentPlaneTables(ctx, key, []string{"chunks", "documents"})
	if err != nil || len(held) != 1 || held[0] != "chunks" {
		t.Fatalf("AgentPlaneTables = %v, %v; want [chunks]", held, err)
	}
	if err := doc.AdoptAgentTables(ctx, key, []string{"chunks", "documents"}); err != nil {
		t.Fatalf("AdoptAgentTables: %v", err)
	}

	res, err := doc.Query(ctx, key, `SELECT id, n, encode(b, 'hex'), body FROM chunks ORDER BY id`, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{"c1", int64(7), "00ff", "one"}, {"c2", nil, nil, "two's"}}
	if len(res.Rows) != 2 {
		t.Fatalf("doc rows = %v, want %v", res.Rows, want)
	}
	for i := range want {
		for j := range want[i] {
			if res.Rows[i][j] != want[i][j] {
				t.Errorf("doc row %d col %d = %#v, want %#v", i, j, res.Rows[i][j], want[i][j])
			}
		}
	}
	var left int
	_ = raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = $1 AND c.relname = 'chunks'`, agentSchema).Scan(&left)
	if left != 0 {
		t.Errorf("the agent plane still holds chunks")
	}
	var triggers, foreignDefaults, extra int
	_ = raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = ($1 || '.chunks')::regclass AND NOT tgisinternal`,
		`"`+docSchema+`"`).Scan(&triggers)
	_ = raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_attrdef d JOIN pg_depend dep ON dep.objid = d.oid
	  JOIN pg_proc p ON p.oid = dep.refobjid WHERE d.adrelid = ($1 || '.chunks')::regclass`, `"`+docSchema+`"`).Scan(&foreignDefaults)
	_ = raw.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
	  WHERE table_schema = $1 AND table_name = 'chunks' AND column_name = 'extra'`, docSchema).Scan(&extra)
	if triggers != 0 || foreignDefaults != 0 || extra != 0 {
		t.Errorf("doc table carries triggers=%d function defaults=%d extra column=%d, want none", triggers, foreignDefaults, extra)
	}
	// The doc role writing its table runs nothing of the agent's.
	if _, err := doc.Exec(ctx, key, `INSERT INTO chunks (id, body) VALUES ('c3', 'three')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	spied, err := m.Query(ctx, key, `SELECT count(*) FROM spied`, nil)
	if err != nil || spied.Rows[0][0] != int64(0) {
		t.Errorf("the agent's function ran %v times (%v), want 0", spied.Rows, err)
	}

	// A second move finds nothing; an agent copy beside a populated doc table is
	// discarded rather than merged.
	if err := doc.AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatalf("second AdoptAgentTables: %v", err)
	}
	if _, err := m.Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY, n BIGINT, b BYTEA, body TEXT)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Exec(ctx, key, `INSERT INTO chunks (id, body) VALUES ('stale', 'old')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := doc.AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatalf("AdoptAgentTables over a populated doc table: %v", err)
	}
	res, err = doc.Query(ctx, key, `SELECT count(*) FROM chunks WHERE id = 'stale'`, nil)
	if err != nil || res.Rows[0][0] != int64(0) {
		t.Errorf("the stale agent copy was merged: %v %v", res, err)
	}
	if held, _ := m.AgentPlaneTables(ctx, key, []string{"chunks"}); len(held) != 0 {
		t.Errorf("the stale agent copy was left on the agent plane")
	}
}

// A snapshot of a scope carries both planes, and restoring it into a fresh
// runtime puts the document tables on the agent plane, from where the document
// plane's first use moves them, exactly as for a scope from before the split.
func TestPostgresPlanes_SnapshotCarriesTheDocumentPlane(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "writer")
	doc := m.DocPlane()
	if _, err := doc.Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Exec(ctx, key, `INSERT INTO chunks VALUES ('c1')`, nil, 0); err != nil {
		t.Fatal(err)
	}
	dump, err := m.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range dump.Tables {
		names = append(names, tb.Name)
	}
	if strings.Join(names, ",") != "chunks" {
		t.Fatalf("dump tables = %v, want the doc plane's chunks", names)
	}
	if _, err := m.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	if held, err := m.AgentPlaneTables(ctx, key, []string{"chunks"}); err != nil || len(held) != 1 {
		t.Fatalf("restored to the agent plane: %v, %v", held, err)
	}
	if _, err := doc.Exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY)`, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := doc.AdoptAgentTables(ctx, key, []string{"chunks"}); err != nil {
		t.Fatal(err)
	}
	res, err := doc.Query(ctx, key, `SELECT id FROM chunks`, nil)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "c1" {
		t.Errorf("doc plane after restore and move: %v %v", res, err)
	}
}

// sql is imported for the raw handle pgTestManager returns.
var _ *sql.DB
