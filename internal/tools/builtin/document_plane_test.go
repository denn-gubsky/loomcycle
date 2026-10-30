package builtin

import (
	"fmt"
	"strings"
	"testing"
)

// document_plane_test.go — on postgres the Document tool's tables live on a plane
// of their own (sqlmem.Manager.DocPlane): a schema and login role per scope that
// an agent's SQL Memory role cannot use.

// An agent's own SQL, run straight through the engine with no statement check in
// the way, can neither read the Document tool's tables nor accept its own
// proposed statement.
func TestDocumentPlane_AgentSQLCannotReachTheDocumentTables(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "Plane")
	out, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","evidence":[{"chunk_id":%q,"quote":"comply with Article 12"}]}`, a, b, a))
	if r.IsError {
		t.Fatalf("propose_statement: %s", r.Text)
	}
	stmtID, _ := out["id"].(string)
	key, _, _ := d.resolveScope(ctx, "user")

	for _, q := range []string{`SELECT count(*) FROM chunks`, `SELECT title FROM chunks`, `SELECT status FROM chunk_edges`} {
		if res, err := d.SqlMem.Query(ctx, key, q, nil); err == nil {
			t.Errorf("agent plane %q answered %v, want refused", q, res.Rows)
		}
	}
	if _, err := d.SqlMem.Exec(ctx, key, `UPDATE chunk_edges SET status = 'asserted'`, nil, 0); err == nil {
		t.Errorf("agent plane UPDATE chunk_edges succeeded, want refused")
	}
	got, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, stmtID))
	if r.IsError {
		t.Fatalf("get_edge: %s", r.Text)
	}
	if s, _ := got["statement"].(map[string]any); s["status"] != "proposed" {
		t.Errorf("statement status = %v, want still proposed", s["status"])
	}
}

// A scope whose document tables are on the agent plane, as every postgres scope
// from before the split has them and as a restored snapshot leaves them, is moved
// by the Document tool's first use: its documents read back, the agent plane no
// longer holds them, and a trigger an agent planted on them is gone.
func TestDocumentPlane_PreSplitScopeIsMovedOnFirstUse(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Old Scope"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := out["document_id"].(string)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Part","body":"kept text"}`, docID)); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	key, _, _ := d.resolveScope(ctx, "user")

	// The pre-split layout: a snapshot restored into a fresh scope lands on the
	// agent plane.
	dump, err := d.SqlMem.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SqlMem.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := d.SqlMem.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE spied (who TEXT)`,
		`CREATE FUNCTION spy() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO spied VALUES (current_user); RETURN NEW; END $$`,
		`CREATE TRIGGER planted BEFORE INSERT ON chunks FOR EACH ROW EXECUTE FUNCTION spy()`,
	} {
		if _, err := d.SqlMem.Exec(ctx, key, stmt, nil, 0); err != nil {
			t.Fatalf("plant: %s: %v", stmt, err)
		}
	}
	d.SqlMem.ForgetEnsured(key)

	got, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","scope":"user","id":%q}`, docID))
	if r.IsError {
		t.Fatalf("export_md after the move: %s", r.Text)
	}
	if md, _ := got["markdown"].(string); !strings.Contains(md, "kept text") {
		t.Errorf("export_md = %q, want the restored chunk's text", md)
	}
	if res, err := d.SqlMem.Query(ctx, key, `SELECT count(*) FROM chunks`, nil); err == nil {
		t.Errorf("the agent plane still holds chunks: %v", res.Rows)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"New","body":"after the move"}`, docID)); r.IsError {
		t.Fatalf("create_chunk after the move: %s", r.Text)
	}
	spied, err := d.SqlMem.Query(ctx, key, `SELECT count(*) FROM spied`, nil)
	if err != nil || spied.Rows[0][0] != int64(0) {
		t.Errorf("the planted trigger ran %v times (%v), want 0", spied.Rows, err)
	}
	trig, err := d.docSQL().Query(ctx, key, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
	  JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE n.nspname = current_schema() AND c.relname = 'chunks' AND NOT t.tgisinternal`, nil)
	if err != nil || trig.Rows[0][0] != int64(0) {
		t.Errorf("triggers on the document plane's chunks: %v (%v), want none", trig.Rows, err)
	}
}
