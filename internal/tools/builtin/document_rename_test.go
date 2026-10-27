package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A document's name lives twice: on its root chunk (what every schema description
// calls "the document's title") and denormalized on the documents row, which
// get_document, documents_summary, query_documents, export_md and title link
// resolution all read. Renaming the root chunk used to move only the first copy,
// so an agent that renamed a document saw no effect anywhere it looked.

// docTitleSurfaces reads a document's title from every surface that reports one.
func docTitleSurfaces(t *testing.T, d *Document, ctx context.Context, docID string) map[string]string {
	t.Helper()
	got := map[string]string{}

	out, res := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("get_document: %s", res.Text)
	}
	got["get_document"], _ = out["title"].(string)

	out, res = docExec(t, d, ctx, `{"op":"documents_summary","scope":"user","document_ids":["`+docID+`"]}`)
	if res.IsError {
		t.Fatalf("documents_summary: %s", res.Text)
	}
	if docs, _ := out["documents"].([]any); len(docs) == 1 {
		got["documents_summary"], _ = docs[0].(map[string]any)["title"].(string)
	} else {
		t.Fatalf("documents_summary: want one entry, got %s", res.Text)
	}

	out, res = docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`)
	if res.IsError {
		t.Fatalf("query_documents: %s", res.Text)
	}
	docs, _ := out["documents"].([]any)
	for _, e := range docs {
		m := e.(map[string]any)
		if m["document_id"] == docID {
			got["query_documents"], _ = m["title"].(string)
		}
	}

	out, res = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("export_md: %s", res.Text)
	}
	got["export_md"], _ = out["title"].(string)
	return got
}

func assertDocTitle(t *testing.T, d *Document, ctx context.Context, docID, want string) {
	t.Helper()
	for surface, title := range docTitleSurfaces(t, d, ctx, docID) {
		if title != want {
			t.Errorf("%s title = %q, want %q", surface, title, want)
		}
	}
}

func rootRevision(t *testing.T, d *Document, ctx context.Context, chunkID string) int {
	t.Helper()
	out, res := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+chunkID+`"}`)
	if res.IsError {
		t.Fatalf("get_chunk: %s", res.Text)
	}
	return int(out["revision"].(float64))
}

func renameChunk(t *testing.T, d *Document, ctx context.Context, chunkID, title string) {
	t.Helper()
	rev := rootRevision(t, d, ctx, chunkID)
	tj, _ := json.Marshal(title)
	_, res := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+chunkID+`","revision":`+strconv.Itoa(rev)+`,"title":`+string(tj)+`}`)
	if res.IsError {
		t.Fatalf("update_chunk title=%q: %s", title, res.Text)
	}
}

func assertRootRenameRenamesDocument(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	// A decoy created FIRST whose chunk already carries the new name: title link
	// resolution prefers a document with that exact title over any chunk, so it
	// tells a renamed documents row apart from a chunk-title fallback.
	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Decoy"}`)
	if res.IsError {
		t.Fatalf("create_document(decoy): %s", res.Text)
	}
	decoyRoot := out["root_chunk_id"].(string)
	out, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+out["document_id"].(string)+`","parent_id":"`+decoyRoot+`","title":"Launch plan v2"}`)
	if res.IsError {
		t.Fatalf("create_chunk(decoy): %s", res.Text)
	}
	decoyChunk := out["id"].(string)

	out, res = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Launch plan"}`)
	if res.IsError {
		t.Fatalf("create_document: %s", res.Text)
	}
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	path, _ := out["path"].(string)
	if path == "" {
		t.Fatalf("create_document returned no path: %s", res.Text)
	}
	out, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`","title":"Timeline"}`)
	if res.IsError {
		t.Fatalf("create_chunk: %s", res.Text)
	}
	child := out["id"].(string)

	renameChunk(t, d, ctx, root, "Launch plan v2")
	assertDocTitle(t, d, ctx, docID, "Launch plan v2")

	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if got, ok := d.resolveLinkTarget(ctx, key, "Launch plan v2"); !ok || got != root {
		t.Errorf("link to the new title resolved to %q (ok=%v), want the renamed root %q (decoy chunk is %q)", got, ok, root, decoyChunk)
	}
	if got, ok := d.resolveLinkTarget(ctx, key, "Launch plan"); ok {
		t.Errorf("link to the old title still resolves (to %q); nothing carries that name any more", got)
	}

	// The Path-tree name is not the title: a rename leaves it where it was.
	out, res = docExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"`+path+`"}`)
	if res.IsError || out["document_id"] != docID {
		t.Errorf("get_document by the original path %q after a rename = %s", path, res.Text)
	}

	// Renaming a non-root chunk is not a document rename.
	renameChunk(t, d, ctx, child, "Schedule")
	assertDocTitle(t, d, ctx, docID, "Launch plan v2")

	// A blank root title keeps the document's last name: documents.title is NOT
	// NULL and every listing shows it, so it is never blanked.
	renameChunk(t, d, ctx, root, "")
	assertDocTitle(t, d, ctx, docID, "Launch plan v2")
}

func TestDocument_RenameRootChunkRenamesDocument(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertRootRenameRenamesDocument(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := renamePostgresFixture(t)
		assertRootRenameRenamesDocument(t, d, ctx)
	})
}

// A root chunk can carry a natural key (a homed subject's document is rooted at
// its subject chunk), so upsert_chunk can rename a document too.
func TestDocument_UpsertOnKeyedRootRenamesDocument(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Ada"}`)
	if res.IsError {
		t.Fatalf("create_document: %s", res.Text)
	}
	docID := out["document_id"].(string)
	root := out["root_chunk_id"].(string)
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.writeChunkMeta(ctx, key, root, docInput{NaturalKey: "person:ada"}); err != nil {
		t.Fatalf("writeChunkMeta: %v", err)
	}

	// Title only: no type or status, which is what used to gate the mirror.
	out, res = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada Lovelace"}`)
	if res.IsError {
		t.Fatalf("upsert_chunk: %s", res.Text)
	}
	if out["id"] != root || out["created"] != false {
		t.Fatalf("upsert_chunk did not update the keyed root: %s", res.Text)
	}
	assertDocTitle(t, d, ctx, docID, "Ada Lovelace")
}

// Rows renamed before the mirror existed are repaired when the scope's schema is
// provisioned: the backfill copies a non-blank root title onto the documents row.
func TestDocument_SchemaOpenBackfillsDriftedTitle(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertSchemaOpenBackfillsTitle(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := renamePostgresFixture(t)
		assertSchemaOpenBackfillsTitle(t, d, ctx)
	})
}

func assertSchemaOpenBackfillsTitle(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	mk := func(title string) (string, string) {
		out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"`+title+`"}`)
		if res.IsError {
			t.Fatalf("create_document: %s", res.Text)
		}
		return out["document_id"].(string), out["root_chunk_id"].(string)
	}
	drifted, driftedRoot := mk("Before")
	blank, blankRoot := mk("Keeps its name")
	inStep, _ := mk("Untouched")

	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	// The drift as the old code left it: the root chunk renamed, the row not.
	if err := d.exec(ctx, key, `UPDATE chunks SET title = ? WHERE id = ?`, "After", driftedRoot); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(ctx, key, `UPDATE chunks SET title = ? WHERE id = ?`, "  ", blankRoot); err != nil {
		t.Fatal(err)
	}
	assertDocTitle(t, d, ctx, drifted, "Before") // the drifted state is real

	// Twice: the second provisioning must find nothing to change.
	for i := 0; i < 2; i++ {
		d.SqlMem.ForgetEnsured(key)
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatalf("ensureSchema (pass %d): %v", i+1, err)
		}
		assertDocTitle(t, d, ctx, drifted, "After")
		assertDocTitle(t, d, ctx, blank, "Keeps its name")
		assertDocTitle(t, d, ctx, inStep, "Untouched")
	}
}

// renamePostgresFixture runs the Document structure tier on POSTGRES. Cleanup
// drops only the scope this test provisioned (DropScope removes its schema and its
// derived role) rather than every sqlmem role in the cluster — roles are
// cluster-global, so a blanket drop could reach another database's scopes.
func renamePostgresFixture(t *testing.T) (*Document, context.Context) {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres half")
	}
	mgr, err := sqlmem.NewPostgres(context.Background(),
		sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() }) // runs after the DropScope below (LIFO)
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-rename-pg")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u-rename-pg", TenantID: "tnt-rename-pg"})
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if _, err := mgr.DropScope(context.Background(), key); err != nil {
		t.Fatalf("DropScope (stale): %v", err)
	}
	t.Cleanup(func() {
		if _, err := mgr.DropScope(context.Background(), key); err != nil {
			t.Errorf("DropScope: %v", err)
		}
	})
	return d, ctx
}
