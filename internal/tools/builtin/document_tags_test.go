package builtin

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC BS Phase 1 — the tags facet + document-level type/status/tags.

// --- small extraction helpers ---

func tagsOf(m map[string]any) []string {
	raw, _ := m["tags"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		out = append(out, t.(string))
	}
	return out
}

func tagCounts(m map[string]any) map[string]int {
	raw, _ := m["tags"].([]any)
	out := map[string]int{}
	for _, e := range raw {
		em := e.(map[string]any)
		out[em["tag"].(string)] = int(em["count"].(float64))
	}
	return out
}

func queryDocIDs(m map[string]any) []string {
	raw, _ := m["documents"].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any)["document_id"].(string))
	}
	return out
}

func eqStrings(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func eqSet(got []string, want ...string) bool {
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	return eqStrings(g, w...)
}

// chunkTags reads a chunk's tag names (sorted) via list_tags.
func chunkTags(t *testing.T, d *Document, ctx context.Context, chunkID string) []string {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"list_tags","scope":"user","id":"`+chunkID+`"}`)
	if r.IsError {
		t.Fatalf("list_tags(%s): %s", chunkID, r.Text)
	}
	raw, _ := out["tags"].([]any)
	names := make([]string, 0, len(raw))
	for _, e := range raw {
		names = append(names, e.(map[string]any)["tag"].(string))
	}
	return names
}

// countRows runs a COUNT(*) (or any single-cell SELECT) in the user scope.
func countRows(t *testing.T, d *Document, ctx context.Context, stmt string, args ...any) int {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if len(res.Rows) == 0 {
		return 0
	}
	return asInt(res.Rows[0][0])
}

// --- the tier-portable core (runs on sqlite AND postgres) ---

// TestDocumentTags_CoreBothTiers exercises the whole Phase-1 surface — document
// facets on create/get, chunk add/remove/list, query_chunks tag + tag_prefix, and
// query_documents — on BOTH SQL Memory tiers, so the new DDL, the `?` rebind, the
// tag joins, the tag_prefix LIKE, and the scope-wide UNION count are all proven on
// postgres, not just sqlite. The postgres half is skipped without the aux DSN.
func TestDocumentTags_CoreBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertDocumentTagFacets(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertDocumentTagFacets(t, d, ctx)
	})
}

// pgDocumentOrSkip builds a Document on the postgres SQL Memory tier, or skips.
func pgDocumentOrSkip(t *testing.T) (*Document, context.Context) {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres-tier parity half")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	mgr, err := sqlmem.NewPostgres(context.Background(),
		sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "pg-tags")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	return d, ctx
}

func assertDocumentTagFacets(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()

	// create_document with type/status/tags → get_document returns all three.
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"RFC BS","path":"/bs/one","type":"rfc","status":"draft","tags":["area/docs","priority/high"]}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	doc1 := out["document_id"].(string)
	root1 := out["root_chunk_id"].(string)

	g, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+doc1+`"}`)
	if r.IsError {
		t.Fatalf("get_document: %s", r.Text)
	}
	if g["type"] != "rfc" || g["status"] != "draft" {
		t.Errorf("get_document type/status = %v/%v, want rfc/draft", g["type"], g["status"])
	}
	if got := tagsOf(g); !eqStrings(got, "area/docs", "priority/high") {
		t.Errorf("document tags = %v, want [area/docs priority/high]", got)
	}

	// A chunk created WITH tags.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc1+`","parent_id":"`+root1+`","title":"c1","tags":["area/docs","kind/note"]}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	c1 := out["id"].(string)

	// add_tags is incremental + idempotent (re-adding kind/note is a no-op).
	out, r = docExec(t, d, ctx, `{"op":"add_tags","scope":"user","id":"`+c1+`","tags":["kind/note","urgent"]}`)
	if r.IsError {
		t.Fatalf("add_tags: %s", r.Text)
	}
	if got := tagsOf(out); !eqStrings(got, "area/docs", "kind/note", "urgent") {
		t.Errorf("chunk tags after add = %v", got)
	}
	// remove_tags.
	out, r = docExec(t, d, ctx, `{"op":"remove_tags","scope":"user","id":"`+c1+`","tags":["urgent"]}`)
	if r.IsError {
		t.Fatalf("remove_tags: %s", r.Text)
	}
	if got := tagsOf(out); !eqStrings(got, "area/docs", "kind/note") {
		t.Errorf("chunk tags after remove = %v", got)
	}

	// list_tags: chunk, document, scope-wide (chunk+document combined counts).
	lt, _ := docExec(t, d, ctx, `{"op":"list_tags","scope":"user","id":"`+c1+`"}`)
	if cc := tagCounts(lt); cc["area/docs"] != 1 || cc["kind/note"] != 1 {
		t.Errorf("list_tags(chunk) = %v", cc)
	}
	lt, _ = docExec(t, d, ctx, `{"op":"list_tags","scope":"user","document_id":"`+doc1+`"}`)
	if dc := tagCounts(lt); dc["area/docs"] != 1 || dc["priority/high"] != 1 {
		t.Errorf("list_tags(document) = %v", dc)
	}
	lt, _ = docExec(t, d, ctx, `{"op":"list_tags","scope":"user"}`)
	if sc := tagCounts(lt); sc["area/docs"] != 2 { // on the chunk AND the document
		t.Errorf("scope-wide count(area/docs) = %d, want 2: %v", sc["area/docs"], sc)
	}

	// query_chunks by exact tag.
	q, _ := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","tag":"kind/note"}`)
	if rows, _ := q["chunks"].([]any); len(rows) != 1 {
		t.Errorf("query_chunks tag=kind/note = %d rows, want 1", len(rows))
	}
	// tag_prefix "area" matches the nested "area/docs".
	q, _ = docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","tag_prefix":"area"}`)
	if rows, _ := q["chunks"].([]any); len(rows) != 1 {
		t.Errorf("query_chunks tag_prefix=area = %d rows, want 1", len(rows))
	}
	// tag_prefix "are" must NOT prefix-match "area/docs" (the boundary is a '/').
	q, _ = docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","tag_prefix":"are"}`)
	if rows, _ := q["chunks"].([]any); len(rows) != 0 {
		t.Errorf("query_chunks tag_prefix=are = %d rows, want 0 (must not match area)", len(rows))
	}

	// A second document for the query_documents filters.
	out, r = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Note two","path":"/bs/two","type":"note","status":"published","tags":["priority/high","kind/misc"]}`)
	if r.IsError {
		t.Fatalf("create_document doc2: %s", r.Text)
	}
	doc2 := out["document_id"].(string)

	qd, _ := docExec(t, d, ctx, `{"op":"query_documents","scope":"user","type":"rfc"}`)
	if got := queryDocIDs(qd); !eqSet(got, doc1) {
		t.Errorf("query_documents type=rfc = %v, want [doc1]", got)
	}
	qd, _ = docExec(t, d, ctx, `{"op":"query_documents","scope":"user","status":"published"}`)
	if got := queryDocIDs(qd); !eqSet(got, doc2) {
		t.Errorf("query_documents status=published = %v, want [doc2]", got)
	}
	qd, _ = docExec(t, d, ctx, `{"op":"query_documents","scope":"user","tag":"priority/high"}`)
	if got := queryDocIDs(qd); !eqSet(got, doc1, doc2) {
		t.Errorf("query_documents tag=priority/high = %v, want both", got)
	}
	qd, _ = docExec(t, d, ctx, `{"op":"query_documents","scope":"user","under_path":"/bs"}`)
	if got := queryDocIDs(qd); !eqSet(got, doc1, doc2) {
		t.Errorf("query_documents under_path=/bs = %v, want both", got)
	}
	// Combined filters narrow to doc1.
	qd, _ = docExec(t, d, ctx, `{"op":"query_documents","scope":"user","type":"rfc","tag":"priority/high"}`)
	if got := queryDocIDs(qd); !eqSet(got, doc1) {
		t.Errorf("query_documents type=rfc&tag=priority/high = %v, want [doc1]", got)
	}
}

// --- sqlite-only behavioural tests ---

// TestDocumentTags_ReplaceSetPresence pins the unset-vs-empty semantics of the
// `tags` field on update_chunk: an omitted key leaves the tags untouched, a
// present `[]` clears them, a present non-empty list replaces the set wholesale.
// Fail-before: if update_chunk ignored `tags`, both the replace and the clear
// steps would leave the original [x,y] and the assertions would fail.
func TestDocumentTags_ReplaceSetPresence(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+root+`","title":"c","tags":["x","y"]}`)
	c := out["id"].(string)
	rev := int(out["revision"].(float64))

	// Absent tags (only a body change) → tags untouched.
	out, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c+`","revision":`+strconv.Itoa(rev)+`,"body":"z"}`)
	if r.IsError {
		t.Fatalf("update body: %s", r.Text)
	}
	rev = int(out["revision"].(float64))
	if got := chunkTags(t, d, ctx, c); !eqStrings(got, "x", "y") {
		t.Errorf("absent tags must leave them; got %v", got)
	}

	// Present non-empty → replace.
	out, r = docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c+`","revision":`+strconv.Itoa(rev)+`,"tags":["z"]}`)
	if r.IsError {
		t.Fatalf("update tags replace: %s", r.Text)
	}
	rev = int(out["revision"].(float64))
	if got := chunkTags(t, d, ctx, c); !eqStrings(got, "z") {
		t.Errorf("replace-set must yield [z]; got %v", got)
	}

	// Present empty → clear.
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c+`","revision":`+strconv.Itoa(rev)+`,"tags":[]}`); r.IsError {
		t.Fatalf("update tags clear: %s", r.Text)
	}
	if got := chunkTags(t, d, ctx, c); len(got) != 0 {
		t.Errorf("present-empty tags must clear; got %v", got)
	}
}

// TestDocumentTags_RootFacetMirror pins the root-chunk → documents-row mirror:
// editing a document's ROOT chunk's type/status propagates to the documents row
// that get_document/query_documents read; editing a NON-root chunk does not.
// Fail-before: without the mirror, get_document (which reads the documents row)
// shows no type after the root edit.
func TestDocumentTags_RootFacetMirror(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Doc"}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)

	// Fresh: the documents row has no type yet.
	g, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+doc+`"}`)
	if _, has := g["type"]; has {
		t.Errorf("fresh document should have no type: %v", g)
	}

	// Editing the ROOT chunk mirrors onto the documents row.
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+root+`","revision":1,"type":"rfc","status":"done"}`); r.IsError {
		t.Fatalf("update root: %s", r.Text)
	}
	g, _ = docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+doc+`"}`)
	if g["type"] != "rfc" || g["status"] != "done" {
		t.Errorf("root edit did not mirror onto the documents row: %v", g)
	}
	// query_documents (also reads the documents row) sees it too.
	qd, _ := docExec(t, d, ctx, `{"op":"query_documents","scope":"user","type":"rfc","status":"done"}`)
	if got := queryDocIDs(qd); !eqSet(got, doc) {
		t.Errorf("query_documents after mirror = %v, want [doc]", got)
	}

	// A NON-root chunk's type change must NOT touch the documents row.
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+root+`","title":"c","type":"section"}`)
	c, crev := out["id"].(string), int(out["revision"].(float64))
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c+`","revision":`+strconv.Itoa(crev)+`,"type":"appendix"}`); r.IsError {
		t.Fatalf("update non-root: %s", r.Text)
	}
	g, _ = docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+doc+`"}`)
	if g["type"] != "rfc" {
		t.Errorf("a non-root chunk change leaked into the documents row: %v", g)
	}

	// set_asset also writes chunks.type (='image'); on a root chunk it must mirror
	// too, or an image-rooted document's get_document type goes stale.
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AARgwMDAwMAA8mAf/hZ0ZzAAAAAElFTkSuQmCC"
	if _, r := docExec(t, d, ctx, `{"op":"set_asset","scope":"user","id":"`+root+`","media_type":"image/png","data":"`+png+`"}`); r.IsError {
		t.Fatalf("set_asset on root: %s", r.Text)
	}
	g, _ = docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+doc+`"}`)
	if g["type"] != "image" {
		t.Errorf("set_asset on the root did not mirror type=image onto the documents row: %v", g)
	}
}

// TestDocumentFacets_MigrationBackfill pins migrateDocumentFacets: a scope
// provisioned BEFORE this change (documents has no type/status column) gets the
// columns added on the next ensureSchema AND backfilled from each document's root
// chunk. Fail-before: without the migration the SELECT errors (no such column);
// without the backfill the value reads NULL.
func TestDocumentFacets_MigrationBackfill(t *testing.T) {
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
	d := &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	now := time.Now().UnixNano()

	// Provision the OLD pre-RFC-BS shape directly (documents WITHOUT type/status),
	// before any ensureSchema runs, so the migration has real work to do.
	if err := d.exec(ctx, key, `CREATE TABLE documents (id TEXT PRIMARY KEY, title TEXT NOT NULL, root_chunk_id TEXT NOT NULL, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`); err != nil {
		t.Fatalf("old documents DDL: %v", err)
	}
	if err := d.exec(ctx, key, `CREATE TABLE chunks (id TEXT PRIMARY KEY, document_id TEXT NOT NULL, parent_id TEXT, position INTEGER NOT NULL, type TEXT, status TEXT, title TEXT NOT NULL, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, revision INTEGER NOT NULL DEFAULT 1)`); err != nil {
		t.Fatalf("chunks DDL: %v", err)
	}
	// A document whose ROOT chunk carries type/status but whose documents row does not.
	if err := d.exec(ctx, key, `INSERT INTO documents (id, title, root_chunk_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, "doc1", "Old", "root1", now, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	if err := d.exec(ctx, key, `INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision) VALUES (?, ?, NULL, 0, ?, ?, ?, ?, ?, 1)`, "root1", "doc1", "rfc", "done", "Old", now, now); err != nil {
		t.Fatalf("insert root chunk: %v", err)
	}

	// ensureSchema runs the migration: add the columns + backfill from the root.
	if err := d.ensureSchema(ctx, key); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	res, err := d.query(ctx, key, `SELECT type, status FROM documents WHERE id = ?`, "doc1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "rfc" || asStr(res.Rows[0][1]) != "done" {
		t.Errorf("backfill did not populate type/status from the root chunk: %v", res.Rows)
	}
}

// TestDocumentTags_DeleteCascadeNoOrphans pins the explicit tag-row cascade:
// delete_chunk removes the deleted subtree's chunk_tags, and delete_document
// removes both the remaining chunk_tags and the document_tags — no orphan tag
// rows survive. Fail-before: without the cascade the DELETEs leave rows behind.
func TestDocumentTags_DeleteCascadeNoOrphans(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D","tags":["dtag"]}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+root+`","title":"p","tags":["ptag"]}`)
	p := out["id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+p+`","title":"child","tags":["ctag"]}`)
	child := out["id"].(string)

	// delete_chunk(p) cascades to child → both chunks' tag rows are gone.
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+p+`"}`); r.IsError {
		t.Fatalf("delete_chunk: %s", r.Text)
	}
	if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_tags WHERE chunk_id IN (?, ?)`, p, child); n != 0 {
		t.Errorf("chunk_tags orphaned after delete_chunk cascade: %d", n)
	}

	// delete_document removes the document's own tag + any remaining chunk tags.
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+doc+`"}`); r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}
	if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, doc); n != 0 {
		t.Errorf("document_tags orphaned after delete_document: %d", n)
	}
	if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_tags`); n != 0 {
		t.Errorf("chunk_tags survived delete_document: %d", n)
	}
}

// TestDocumentTags_UpsertPath pins that upsert_chunk honours `tags` on BOTH its
// create and update paths (the update path lives in the entity file and does not
// touch tags, so the dispatch wrapper applies them), and that an upsert onto a
// root chunk mirrors its type/status onto the documents row.
func TestDocumentTags_UpsertPath(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)

	// First upsert CREATES the chunk (create path → tags applied via createChunk).
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`","parent_id":"`+root+`","natural_key":"ent:x","title":"X","tags":["t1"]}`)
	if r.IsError {
		t.Fatalf("upsert create: %s", r.Text)
	}
	if out["created"] != true {
		t.Fatalf("first upsert should create: %v", out)
	}
	c := out["id"].(string)
	if got := chunkTags(t, d, ctx, c); !eqStrings(got, "t1") {
		t.Errorf("upsert-create tags = %v, want [t1]", got)
	}

	// Second upsert UPDATES the same chunk (update path → the wrapper replace-sets).
	out, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`","natural_key":"ent:x","title":"X","tags":["t2","t3"]}`)
	if r.IsError {
		t.Fatalf("upsert update: %s", r.Text)
	}
	if out["created"] != false {
		t.Fatalf("second upsert should update: %v", out)
	}
	if got := chunkTags(t, d, ctx, c); !eqStrings(got, "t2", "t3") {
		t.Errorf("upsert-update replace-set tags = %v, want [t2 t3]", got)
	}

	// Map a natural_key onto the ROOT chunk so an upsert resolves to it, then
	// upsert type/status → the wrapper must mirror onto the documents row.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_memory_meta (chunk_id, natural_key, created_at) VALUES (?, ?, ?)`, root, "ent:root", time.Now().UnixNano()); err != nil {
		t.Fatalf("seed root natural_key: %v", err)
	}
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"ent:root","title":"Root","type":"rfc","status":"final"}`); r.IsError {
		t.Fatalf("upsert root: %s", r.Text)
	}
	g, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+doc+`"}`)
	if g["type"] != "rfc" || g["status"] != "final" {
		t.Errorf("upsert on the root did not mirror onto the documents row: %v", g)
	}
}

// TestDocumentTags_TargetValidation pins that add/remove/list_tags refuse a
// phantom target (no tag rows written against a non-existent chunk/document) and
// that add/remove require a `tags` list.
func TestDocumentTags_TargetValidation(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`) // ensure schema

	if _, r := docExec(t, d, ctx, `{"op":"add_tags","scope":"user","id":"ghost","tags":["x"]}`); !r.IsError {
		t.Errorf("add_tags on a non-existent chunk should refuse")
	}
	if _, r := docExec(t, d, ctx, `{"op":"add_tags","scope":"user","document_id":"ghost","tags":["x"]}`); !r.IsError {
		t.Errorf("add_tags on a non-existent document should refuse")
	}
	if _, r := docExec(t, d, ctx, `{"op":"add_tags","scope":"user","id":"whatever"}`); !r.IsError {
		t.Errorf("add_tags without tags should refuse")
	}
	// No phantom rows were written for the refused targets.
	if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunk_tags`); n != 0 {
		t.Errorf("phantom chunk_tags rows written: %d", n)
	}
	if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM document_tags`); n != 0 {
		t.Errorf("phantom document_tags rows written: %d", n)
	}
}
