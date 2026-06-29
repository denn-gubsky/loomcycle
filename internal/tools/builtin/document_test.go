package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func documentFixture(t *testing.T) (*Document, context.Context, store.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	return &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}, ctx, s
}

func docExec(t *testing.T, d *Document, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		_ = json.Unmarshal([]byte(res.Text), &out)
	}
	return out, res
}

func TestDocument_EndToEnd(t *testing.T) {
	d, ctx, s := documentFixture(t)

	// create_document with a Path-tree name.
	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"Launch plan","path":"/docs/launch"}`)
	if res.IsError {
		t.Fatalf("create_document: %q", res.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	if docID == "" || root == "" || out["path"] != "/docs/launch" {
		t.Fatalf("create_document out = %s", res.Text)
	}
	// The Path-tree dirent exists (agent scope).
	if _, derr := s.DirentGet(context.Background(), "tnt", "agent", "doc-agent", "/docs/", "launch"); derr != nil {
		t.Errorf("document dirent not registered: %v", derr)
	}

	// create a chunk under the root.
	out, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"agent","document_id":"`+docID+`","parent_id":"`+root+`","type":"publication","title":"Day 1","body":"# hi","status":"draft"}`)
	if res.IsError {
		t.Fatalf("create_chunk: %q", res.Text)
	}
	chunkID, _ := out["id"].(string)
	if out["title"] != "Day 1" || out["type"] != "publication" || out["body"] != "# hi" || int(out["revision"].(float64)) != 1 {
		t.Errorf("chunk out = %s", res.Text)
	}

	// query by type+status.
	out, res = docExec(t, d, ctx, `{"op":"query_chunks","scope":"agent","type":"publication","status":"draft"}`)
	if res.IsError {
		t.Fatalf("query_chunks: %q", res.Text)
	}
	if rows, _ := out["chunks"].([]any); len(rows) != 1 {
		t.Errorf("query by type/status = %s", res.Text)
	}

	// query by under_path (the Path glue).
	out, res = docExec(t, d, ctx, `{"op":"query_chunks","scope":"agent","under_path":"/docs"}`)
	if res.IsError {
		t.Fatalf("query under_path: %q", res.Text)
	}
	if rows, _ := out["chunks"].([]any); len(rows) != 2 { // root + Day 1
		t.Errorf("under_path /docs = %d chunks, want 2: %s", len(rows), res.Text)
	}

	// get_document by path.
	out, res = docExec(t, d, ctx, `{"op":"get_document","scope":"agent","path":"/docs/launch"}`)
	if res.IsError || out["document_id"] != docID {
		t.Errorf("get_document by path = %s", res.Text)
	}
	_ = chunkID
}

func TestDocument_UpdateRevisionConflict(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"c","body":"x"}`)
	cid := out["id"].(string)

	// Correct revision (1) succeeds, bumps to 2.
	out, res := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cid+`","revision":1,"body":"y"}`)
	if res.IsError || int(out["revision"].(float64)) != 2 || out["body"] != "y" {
		t.Fatalf("update rev1: %s", res.Text)
	}
	// Stale revision (1 again) conflicts.
	_, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cid+`","revision":1,"body":"z"}`)
	if !res.IsError || !strings.Contains(res.Text, "revision conflict") {
		t.Errorf("stale revision should conflict; got %q", res.Text)
	}
}

func TestDocument_DeleteChunkCascade(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"parent","body":"p"}`)
	parent := out["id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+parent+`","title":"child","body":"c"}`)
	child := out["id"].(string)

	out, res := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+parent+`"}`)
	if res.IsError || int(out["cascade_deleted_descendants"].(float64)) != 1 {
		t.Fatalf("delete cascade: %s", res.Text)
	}
	// Both parent + child gone (rows AND bodies).
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+parent+`"}`); !r.IsError {
		t.Errorf("parent should be gone")
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+child+`"}`); !r.IsError {
		t.Errorf("child should be gone (cascade)")
	}
}

func TestDocument_EdgesAndTypes(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	mk := func(title string) string {
		o, _ := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"`+title+`","body":""}`)
		return o["id"].(string)
	}
	a, b := mk("a"), mk("b")
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+a+`","to_id":"`+b+`","kind":"promotes"}`); r.IsError {
		t.Fatalf("link: %q", r.Text)
	}
	// Verify via the sql escape hatch.
	out, res := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT from_id, to_id, kind FROM chunk_edges"}`)
	if res.IsError {
		t.Fatalf("sql edge query: %q", res.Text)
	}
	if rows, _ := out["rows"].([]any); len(rows) != 1 {
		t.Errorf("expected 1 edge, got %s", res.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"unlink_chunks","scope":"user","from_id":"`+a+`","to_id":"`+b+`","kind":"promotes"}`); r.IsError {
		t.Fatalf("unlink: %q", r.Text)
	}
	// types
	if _, r := docExec(t, d, ctx, `{"op":"define_type","scope":"user","name":"publication","fields":[{"name":"platform","type":"string"}]}`); r.IsError {
		t.Fatalf("define_type: %q", r.Text)
	}
	out, res = docExec(t, d, ctx, `{"op":"list_types","scope":"user"}`)
	if res.IsError {
		t.Fatalf("list_types: %q", res.Text)
	}
	if ts, _ := out["types"].([]any); len(ts) != 1 {
		t.Errorf("expected 1 type, got %s", res.Text)
	}
}

// Tenant + scope isolation: a doc in tenant "tnt" agent scope is invisible to
// another tenant (separate SQL Memory scope DB), and to the user scope.
func TestDocument_ScopeTenantIsolation(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"secret"}`)
	docID := out["document_id"].(string)

	// Different tenant → its agent-scope DB doesn't have the doc.
	ctxB := tools.WithAgentName(context.Background(), "doc-agent")
	ctxB = tools.WithRunIdentity(ctxB, tools.RunIdentityValue{AgentID: "a", TenantID: "other"})
	if _, r := docExec(t, d, ctxB, `{"op":"get_document","scope":"agent","id":"`+docID+`"}`); !r.IsError {
		t.Errorf("cross-tenant get_document must 404; got %q", r.Text)
	}
	// Same tenant, user scope → different DB, doesn't have the agent-scope doc.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`); !r.IsError {
		t.Errorf("cross-scope get_document must 404; got %q", r.Text)
	}
	// The owner sees it.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"agent","id":"`+docID+`"}`); r.IsError {
		t.Errorf("owner should see its doc: %q", r.Text)
	}
}

func TestDocument_TenantScopeRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"x"}`); !r.IsError || !strings.Contains(r.Text, "not yet supported") {
		t.Errorf("scope=tenant should be refused; got %q", r.Text)
	}
}

func TestDocument_SqlEscapeHatchGated(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`) // ensure schema exists
	// ATTACH must be refused by the SQL Memory validator.
	if _, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"ATTACH DATABASE 'x' AS y"}`); !r.IsError {
		t.Errorf("ATTACH via the sql escape hatch must be refused; got %q", r.Text)
	}
}

func TestDocument_RequiresSqlMem(t *testing.T) {
	d := &Document{} // no Store, no SqlMem
	res, _ := d.Execute(context.Background(), json.RawMessage(`{"op":"create_document","title":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "SQL Memory") {
		t.Errorf("Document without SQL Memory should refuse; got %q", res.Text)
	}
}

// Regression (review #1): open mode (empty tenant) must work — SQL Memory
// rejects an empty tenant, so the tool canonicalizes ""→"default" for SQL
// (sqlScopeTenant) while dirents use the raw tenant.
func TestDocument_OpenModeEmptyTenant(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	d := &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}
	// No TenantID (open / single-token mode).
	ctx := tools.WithAgentName(context.Background(), "a1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1"})

	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"open","path":"/o/x"}`)
	if res.IsError {
		t.Fatalf("create_document in open mode failed: %q", res.Text)
	}
	docID := out["document_id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`); r.IsError {
		t.Errorf("get_document in open mode failed: %q", r.Text)
	}
	// The dirent is at the RAW (empty) tenant — get_document by path resolves it.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"/o/x"}`); r.IsError {
		t.Errorf("get_document by path in open mode failed: %q", r.Text)
	}
}

// Regression (review #2): moving a chunk into its own subtree must be refused
// (a parent_id cycle would make delete_chunk's descendant walk non-terminating).
func TestDocument_MoveChunkCycleRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	mk := func(parent, title string) string {
		o, _ := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+parent+`","title":"`+title+`","body":""}`)
		return o["id"].(string)
	}
	a := mk(root, "a")
	b := mk(a, "b") // b is a descendant of a
	// move a under b → cycle → refuse.
	if _, r := docExec(t, d, ctx, `{"op":"move_chunk","scope":"user","id":"`+a+`","new_parent_id":"`+b+`"}`); !r.IsError {
		t.Fatalf("move into own subtree must be refused; got %q", r.Text)
	}
	// move a under itself → refuse.
	if _, r := docExec(t, d, ctx, `{"op":"move_chunk","scope":"user","id":"`+a+`","new_parent_id":"`+a+`"}`); !r.IsError {
		t.Errorf("move under self must be refused")
	}
	// a is intact, and delete_chunk(a) terminates (no hang) and cascades b.
	// (Can't delete the root chunk directly now — that's delete_document.)
	out, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+a+`"}`)
	if r.IsError {
		t.Fatalf("delete after refused move: %q", r.Text)
	}
	if int(out["cascade_deleted_descendants"].(float64)) != 1 { // b
		t.Errorf("cascade count = %v, want 1", out["cascade_deleted_descendants"])
	}
}

// Regression (review #4): delete_document BY ID must also remove the Path-tree
// dirent (no dangling name).
func TestDocument_DeleteByIDCleansDirent(t *testing.T) {
	d, ctx, s := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"D","path":"/docs/d"}`)
	docID := out["document_id"].(string)
	// dirent exists.
	if _, derr := s.DirentGet(context.Background(), "tnt", "agent", "doc-agent", "/docs/", "d"); derr != nil {
		t.Fatalf("dirent missing pre-delete: %v", derr)
	}
	// delete by ID (no path).
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"agent","id":"`+docID+`"}`); r.IsError {
		t.Fatalf("delete_document: %q", r.Text)
	}
	// dirent is gone (no dangling name).
	if _, derr := s.DirentGet(context.Background(), "tnt", "agent", "doc-agent", "/docs/", "d"); derr == nil {
		t.Errorf("dirent should be removed after delete-by-id")
	}
}

// TestDocument_Postgres exercises the Document tool's SQL against the POSTGRES
// SQL Memory tier (the sqlite contract runs everywhere; this catches
// sqlite-vs-postgres parity bugs in the DDL / `?` rebind / type coercion — the
// class that bit Plan 1's DirentMove). Gated on the aux DSN; runs in CI's
// go-postgres job. (Dirents + bodies stay sqlite — only the structure tier is
// PG here, which is the parity surface under test.)
func TestDocument_Postgres(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the Document postgres-tier parity test")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d := &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "pg-doc")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})

	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"PG"}`)
	if res.IsError {
		t.Fatalf("create_document(pg): %q", res.Text)
	}
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	// root-level append (parent_id IS NULL branch) + a parented chunk.
	out, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","type":"pub","status":"draft","title":"c1","body":"b"}`)
	if res.IsError {
		t.Fatalf("create_chunk(pg): %q", res.Text)
	}
	c1 := out["id"].(string)
	if int(out["revision"].(float64)) != 1 {
		t.Errorf("pg revision = %v", out["revision"])
	}
	// query by type/status (BIGINT/INTEGER columns + ? rebind).
	out, res = docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","type":"pub","status":"draft"}`)
	if res.IsError {
		t.Fatalf("query(pg): %q", res.Text)
	}
	if rows, _ := out["chunks"].([]any); len(rows) != 1 {
		t.Errorf("pg query by type = %s", res.Text)
	}
	// export_md (Markdown render) on pg — exercises the documents/chunks SELECTs
	// and the chunk_edges `from_id IN (SELECT ...)` subquery + ? rebind.
	out, res = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`"}`)
	if res.IsError || !strings.Contains(out["markdown"].(string), "## c1") {
		t.Errorf("pg export_md = %s", res.Text)
	}
	// import_md (Markdown round-trip) on pg — exercises createDocument/createChunk
	// + the `UPDATE chunks SET type, status` and root_chunk_id SELECT rebind.
	{
		mdReq, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": out["markdown"]})
		io, ir := docExec(t, d, ctx, string(mdReq))
		if ir.IsError || io["document_id"] == "" || io["document_id"] == docID {
			t.Errorf("pg import_md = %s", ir.Text)
		}
	}
	// update revision (atomic bump + RowsAffected gate on pg).
	out, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c1+`","revision":1,"status":"published"}`)
	if res.IsError || int(out["revision"].(float64)) != 2 {
		t.Fatalf("update(pg): %s", res.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c1+`","revision":1,"body":"x"}`); !r.IsError {
		t.Errorf("pg stale revision should conflict")
	}
	// delete_chunk cascade (BFS + txn) on pg: c1 is a leaf, cascade 0.
	out, res = docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+c1+`"}`)
	if res.IsError || int(out["cascade_deleted_descendants"].(float64)) != 0 {
		t.Errorf("pg delete_chunk = %s", res.Text)
	}
	// delete_document (transactional, bidirectional edge sweep) on pg.
	out, res = docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+docID+`"}`)
	if res.IsError {
		t.Errorf("pg delete_document = %s", res.Text)
	}
}

// RFC AM Phase 2: export_md renders the chunk hierarchy to Markdown — heading
// level = depth, round-trippable metadata + edges as HTML comments by default,
// clean Markdown with include_metadata:false.
func TestDocument_ExportMD(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Plan"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"Intro","type":"section","status":"draft","body":"hello **world**"}`)
	intro := out["id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+intro+`","title":"Detail","body":"deep"}`)
	detail := out["id"].(string)
	docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+intro+`","to_id":"`+detail+`","kind":"references"}`)

	// Default export: metadata-rich, round-trippable.
	out, res := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("export_md: %q", res.Text)
	}
	if out["title"] != "Plan" {
		t.Errorf("title = %v", out["title"])
	}
	md, _ := out["markdown"].(string)
	// Heading level reflects depth: root H1, Intro H2, Detail H3.
	for _, want := range []string{"# Plan", "## Intro", "### Detail", "hello **world**", "deep"} {
		if !strings.Contains(md, want) {
			t.Errorf("export_md missing %q:\n%s", want, md)
		}
	}
	// Metadata comment carries the chunk id + type/status.
	if !strings.Contains(md, "<!-- loom: ") || !strings.Contains(md, intro) || !strings.Contains(md, `"type":"section"`) {
		t.Errorf("export_md missing metadata comment:\n%s", md)
	}
	// Edges trailer present (the graph edge, not the parent-child hierarchy).
	if !strings.Contains(md, "<!-- loom-edges:") || !strings.Contains(md, intro+" -> "+detail+" [references]") {
		t.Errorf("export_md missing edges block:\n%s", md)
	}

	// Clean export: no loom comments, no edges, but the content + headings stay.
	out, res = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`","include_metadata":false}`)
	if res.IsError {
		t.Fatalf("export_md clean: %q", res.Text)
	}
	md, _ = out["markdown"].(string)
	if strings.Contains(md, "<!-- loom") {
		t.Errorf("clean export_md must have no loom comments:\n%s", md)
	}
	if !strings.Contains(md, "## Intro") || !strings.Contains(md, "hello **world**") {
		t.Errorf("clean export_md missing content:\n%s", md)
	}
}

// RFC AM review hardening: a newline in a chunk title must not split the
// heading line, and sibling/nesting order must be deterministic.
func TestDocument_ExportMD_HeadingAndOrder(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Doc"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	// A title with an embedded newline (pos 0 under root).
	docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"line one\nline two","body":"b"}`)
	// Siblings A (pos 1), B (pos 2); A gets children A1, A2.
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"A","body":""}`)
	a := out["id"].(string)
	docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"B","body":""}`)
	docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+a+`","title":"A1","body":""}`)
	docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+a+`","title":"A2","body":""}`)

	out, res := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`","include_metadata":false}`)
	if res.IsError {
		t.Fatalf("export_md: %q", res.Text)
	}
	md := out["markdown"].(string)
	// Fail-before: the old heading wrote the raw title, splitting it across lines.
	if strings.Contains(md, "line one\nline two") {
		t.Errorf("newline in title split the heading line:\n%s", md)
	}
	if !strings.Contains(md, "## line one line two") {
		t.Errorf("expected a single-line heading for the newline title:\n%s", md)
	}
	// Deterministic depth-first order: A, then its children A1, A2, then B.
	iA := strings.Index(md, "## A\n")
	iA1 := strings.Index(md, "### A1")
	iA2 := strings.Index(md, "### A2")
	iB := strings.Index(md, "## B")
	if !(iA >= 0 && iA < iA1 && iA1 < iA2 && iA2 < iB) {
		t.Errorf("sibling/nesting order wrong (A=%d A1=%d A2=%d B=%d):\n%s", iA, iA1, iA2, iB, md)
	}
}

// RFC AM Phase 3: import_md is the deterministic inverse of export_md —
// export → import (new doc) → re-export must be structurally identical, and the
// graph edge must survive (remapped to the new chunk ids).
func TestDocument_ImportMD_RoundTrip(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Plan"}`)
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"A","type":"section","status":"draft","body":"alpha **body**"}`)
	a := out["id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+a+`","title":"A1","body":"nested"}`)
	a1 := out["id"].(string)
	docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"B","body":""}`)
	docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+a+`","to_id":"`+a1+`","kind":"references"}`)

	// Metadata-rich export → import into a NEW document.
	out, res := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("export_md: %q", res.Text)
	}
	md1 := out["markdown"].(string)
	reqB, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md1})
	out, res = docExec(t, d, ctx, string(reqB))
	if res.IsError {
		t.Fatalf("import_md: %q", res.Text)
	}
	newDoc := out["document_id"].(string)
	if newDoc == "" || newDoc == docID {
		t.Fatalf("import_md (no document_id) must create a NEW document: %s", res.Text)
	}
	if int(out["chunks_created"].(float64)) != 4 {
		t.Errorf("chunks_created = %v, want 4 (root + A + A1 + B): %s", out["chunks_created"], res.Text)
	}

	// Clean re-export of the new doc must equal a clean export of the original.
	out, _ = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`","include_metadata":false}`)
	md0 := out["markdown"].(string)
	out, res = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+newDoc+`","include_metadata":false}`)
	if res.IsError {
		t.Fatalf("re-export: %q", res.Text)
	}
	if md2 := out["markdown"].(string); md2 != md0 {
		t.Errorf("round-trip structural mismatch:\n--- original ---\n%s\n--- reimported ---\n%s", md0, md2)
	}
	// The graph edge was recreated (remapped) — the metadata-rich re-export has it.
	out, _ = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+newDoc+`"}`)
	if !strings.Contains(out["markdown"].(string), "[references]") {
		t.Errorf("edge not recreated on import:\n%s", out["markdown"])
	}
}

// Hardening: link_chunks must refuse an edge to a non-existent chunk (no
// born-dangling edges).
func TestDocument_LinkChunksValidatesEndpoints(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	root := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+out["document_id"].(string)+`","parent_id":"`+root+`","title":"a","body":""}`)
	a := out["id"].(string)
	// to_id doesn't exist → refuse.
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+a+`","to_id":"nope","kind":"promotes"}`); !r.IsError || !strings.Contains(r.Text, "no such chunk") {
		t.Errorf("link to a non-existent chunk should refuse; got %q", r.Text)
	}
	// from_id doesn't exist → refuse.
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"ghost","to_id":"`+a+`","kind":"promotes"}`); !r.IsError {
		t.Errorf("link from a non-existent chunk should refuse; got %q", r.Text)
	}
}

// Hardening: deleting a document's root chunk is refused (would orphan the doc).
func TestDocument_DeleteRootChunkRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	root := out["root_chunk_id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+root+`"}`); !r.IsError || !strings.Contains(r.Text, "root chunk") {
		t.Errorf("deleting the root chunk should be refused; got %q", r.Text)
	}
	// The document is intact.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+out["document_id"].(string)+`"}`); r.IsError {
		t.Errorf("document should survive a refused root-chunk delete")
	}
}

// Hardening: delete_document removes INCOMING cross-document edges too (the
// bidirectional cleanup), so no dangling edge points at a deleted chunk.
func TestDocument_DeleteDocumentCleansIncomingCrossDocEdges(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// doc1 with chunk A.
	o1, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D1"}`)
	doc1, r1 := o1["document_id"].(string), o1["root_chunk_id"].(string)
	oa, _ := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc1+`","parent_id":"`+r1+`","title":"a","body":""}`)
	a := oa["id"].(string)
	// doc2 with chunk B.
	o2, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D2"}`)
	doc2, r2 := o2["document_id"].(string), o2["root_chunk_id"].(string)
	ob, _ := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc2+`","parent_id":"`+r2+`","title":"b","body":""}`)
	b := ob["id"].(string)
	// B (doc2) → A (doc1): an INCOMING edge into doc1. Both exist → allowed.
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+b+`","to_id":"`+a+`","kind":"targets"}`); r.IsError {
		t.Fatalf("cross-doc link: %q", r.Text)
	}
	// Delete doc1. The B→A edge (from_id=B is in doc2, to_id=A is in doc1) must
	// be cleaned by the bidirectional sweep — not left dangling.
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+doc1+`"}`); r.IsError {
		t.Fatalf("delete_document: %q", r.Text)
	}
	out, res := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT from_id, to_id FROM chunk_edges"}`)
	if res.IsError {
		t.Fatalf("edge query: %q", res.Text)
	}
	if rows, _ := out["rows"].([]any); len(rows) != 0 {
		t.Errorf("incoming cross-doc edge left dangling after delete_document: %s", res.Text)
	}
}

// Hardening: delete_chunk fails CLOSED when a BFS frontier level exceeds the
// SQL Memory row cap — a truncated enumeration would orphan the unseen
// children's rows under a deleted parent, so we refuse rather than half-delete.
func TestDocument_DeleteChunkRefusesTruncatedSubtree(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// MaxRows:1 → any parent with ≥2 children truncates the frontier query.
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), MaxRows: 1})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	d := &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}

	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)
	// A parent with two children under it → frontier level of 2 > cap of 1.
	op, _ := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+root+`","title":"p","body":""}`)
	parent := op["id"].(string)
	for _, name := range []string{"c1", "c2"} {
		docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+parent+`","title":"`+name+`","body":""}`)
	}
	// delete_chunk(parent) must refuse (fail closed), not orphan a child.
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+parent+`"}`); !r.IsError || !strings.Contains(r.Text, "too wide") {
		t.Fatalf("delete_chunk over a truncated subtree should refuse; got err=%v text=%q", r.IsError, r.Text)
	}
	// The parent and both children must still exist (txn rolled back).
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+parent+`"}`); r.IsError {
		t.Errorf("parent must survive the refused delete; got %q", r.Text)
	}
}

// TestDocument_DefaultPathWhenOmitted pins fix A (RFC AK): create_document
// WITHOUT a path now defaults to /documents/<title-slug> and registers a dirent,
// so the document is never orphaned from the Path tree (was reachable only by id
// → invisible to every human login). Fail-before: the old handler skipped the
// dirent entirely when path was empty.
func TestDocument_DefaultPathWhenOmitted(t *testing.T) {
	d, ctx, s := documentFixture(t)
	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"Launch Plan: Q3!"}`)
	if res.IsError {
		t.Fatalf("create_document: %q", res.Text)
	}
	docID, _ := out["document_id"].(string)
	if docID == "" {
		t.Fatalf("no document_id: %s", res.Text)
	}
	if out["path"] != "/documents/Launch-Plan-Q3" {
		t.Fatalf("default path = %v, want /documents/Launch-Plan-Q3", out["path"])
	}
	// The dirent exists (agent scope) — the doc is now in the Path tree.
	if _, derr := s.DirentGet(context.Background(), "tnt", "agent", "doc-agent", "/documents/", "Launch-Plan-Q3"); derr != nil {
		t.Errorf("default-path dirent not registered: %v", derr)
	}
	// Reachable by the defaulted path.
	out, res = docExec(t, d, ctx, `{"op":"get_document","scope":"agent","path":"/documents/Launch-Plan-Q3"}`)
	if res.IsError || out["document_id"] != docID {
		t.Errorf("get_document by default path = %s", res.Text)
	}
}

func TestDocDefaultPathSegment(t *testing.T) {
	cases := []struct{ title, id, want string }{
		{"Launch Plan: Q3!", "d1", "Launch-Plan-Q3"},
		{"simple", "d2", "simple"},
		{"with.dots_and-dashes", "d3", "with.dots_and-dashes"},
		{"!!!", "d_fallback", "d_fallback"}, // no usable chars → doc id
		{"", "d_empty", "d_empty"},          // empty title → doc id
		{"   ", "d_spaces", "d_spaces"},     // only spaces → doc id
	}
	for _, c := range cases {
		if got := docDefaultPathSegment(c.title, c.id); got != c.want {
			t.Errorf("docDefaultPathSegment(%q,%q) = %q, want %q", c.title, c.id, got, c.want)
		}
	}
}
