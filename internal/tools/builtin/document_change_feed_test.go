package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/cdc"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// captureFixture is documentFixture with the change feed on (the CDC decorator
// in the write path, as LOOMCYCLE_MEMORY_CHANGES_ENABLED wires it), granted the
// tenant scope too.
func captureFixture(t *testing.T) (*Document, context.Context, *cdc.Store) {
	t.Helper()
	d, ctx, raw := documentFixture(t)
	c := cdc.Wrap(raw, nil)
	d.Store = c
	return d, grantedTenantCtx(ctx), c
}

// chunkDocuments maps every live chunk of a scope to its document.
func chunkDocuments(t *testing.T, d *Document, ctx context.Context, scope string, into map[string]string) {
	t.Helper()
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT id, document_id FROM chunks`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		into[asStr(r[0])] = asStr(r[1])
	}
}

// TestDocumentChangeFeed_EveryWriterStampsItsDocument walks the census of
// chunk-body writers (internal/store/cdc/writer_census_test.go lists them) on a
// store that captures changes, and holds every document change row to the
// document its chunk belongs to: creates, updates (whose caller does not name
// the document), imports, copies, statements, deletes of a chunk and of a
// document, the retention prune, and a fact body removed by key.
func TestDocumentChangeFeed_EveryWriterStampsItsDocument(t *testing.T) {
	d, ctx, c := captureFixture(t)
	s := seedCopyDocs(t, d, ctx) // create_document, create_chunk, update_chunk, link_chunks
	imp := copyExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":"# Notes\n\nintro\n\n## Part\n\nmore text"}`)
	impDoc := asStr(imp["document_id"])
	copyExec(t, d, ctx, fmt.Sprintf(`{"op":"import_md","scope":"user","document_id":%q,"markdown":"## Appended\n\nlater text"}`, impDoc))
	copied := copyExec(t, d, ctx, fmt.Sprintf(`{"op":"copy_documents","scope":"user","to_scope":"tenant","document_ids":[%q],"dry_run":false}`, s.guideDoc))
	if copiedID(t, copied, s.guideDoc) == "" {
		t.Fatal("copy_documents made no copy")
	}
	if _, r := docExec(t, d, ctx, `{"op":"unlink_chunks","scope":"user","from_id":"`+s.planChunk+`","to_id":"`+s.guideChunk+`","kind":"documented_by"}`); r.IsError {
		t.Fatalf("unlink_chunks: %s", r.Text)
	}

	want := map[string]string{}
	chunkDocuments(t, d, ctx, "user", want)
	chunkDocuments(t, d, ctx, "tenant", want)

	// The deletes, each recorded before its chunk row goes.
	extra := copyExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+s.planDoc+`","title":"Extra","body":"to delete"}`)
	extraID := asStr(extra["id"])
	pruned := copyExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+s.planDoc+`","title":"Pruned","body":"to prune"}`)
	prunedID := asStr(pruned["id"])
	want[extraID], want[prunedID] = s.planDoc, s.planDoc
	chunkDocuments(t, d, ctx, "user", want) // the import's chunks, before delete_document
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"user","id":"`+extraID+`"}`); r.IsError {
		t.Fatalf("delete_chunk: %s", r.Text)
	}
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.cascadeDeleteChunks(ctx, key, mscope, []string{prunedID}); err != nil {
		t.Fatalf("cascadeDeleteChunks: %v", err)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"user","id":"`+impDoc+`"}`); r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}

	rows, err := c.GetMemoryChangesSince(context.Background(), "tnt", 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[store.MemoryChangeType]int{}
	for _, r := range rows {
		if r.Type != store.DocumentChangeUpdated && r.Type != store.DocumentChangeDeleted && r.Type != store.DocumentEdgesChanged {
			continue
		}
		seen[r.Type]++
		if r.DocumentID == "" {
			t.Errorf("%s of chunk %s (scope %s) names no document", r.Type, r.ChunkID, r.Scope)
			continue
		}
		if w, ok := want[r.ChunkID]; ok && r.DocumentID != w {
			t.Errorf("%s of chunk %s filed under %s, want %s", r.Type, r.ChunkID, r.DocumentID, w)
		}
	}
	for _, typ := range []store.MemoryChangeType{store.DocumentChangeUpdated, store.DocumentChangeDeleted, store.DocumentEdgesChanged} {
		if seen[typ] == 0 {
			t.Errorf("no %s rows at all — the walk did not reach that writer", typ)
		}
	}

	// A consolidated fact's body row removed by key (the Memory tool's and the
	// memory route's delete) is placed through ChunkBodyChangeCtx.
	stamped := d.ChunkBodyChangeCtx(ctx, tools.RunIdentity(ctx).TenantID, store.MemoryScopeUser, "u1", "doc.chunk:"+s.planChunk)
	if got := store.ChangeDocumentFrom(stamped); got != s.planDoc {
		t.Errorf("ChunkBodyChangeCtx placed the plan's chunk under %q, want %s", got, s.planDoc)
	}
	if got := store.ChangeDocumentFrom(d.ChunkBodyChangeCtx(ctx, "tnt", store.MemoryScopeUser, "u1", "memory/fact")); got != "" {
		t.Errorf("a non-chunk key was placed under %q, want nothing", got)
	}
}

// TestDocumentChangeFeed_StatementAcrossDocumentsAppendsTwoRows pins RFC FV §2:
// a link inside one document is one edges row, a link across two documents is
// two — each naming its own document's endpoint.
func TestDocumentChangeFeed_StatementAcrossDocumentsAppendsTwoRows(t *testing.T) {
	d, ctx, c := captureFixture(t)
	s := seedCopyDocs(t, d, ctx) // links the plan's part to the guide's part
	inner := copyExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+s.planDoc+`","title":"Inner","body":"x"}`)
	head, _ := c.MemoryChangesHead(context.Background(), "tnt")
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+s.planChunk+`","to_id":"`+asStr(inner["id"])+`","kind":"cites"}`); r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}
	rows, _ := c.GetMemoryChangesSince(context.Background(), "tnt", head, 100)
	if len(rows) != 1 || rows[0].Type != store.DocumentEdgesChanged || rows[0].DocumentID != s.planDoc || rows[0].ChunkID != s.planChunk {
		t.Errorf("a link inside one document wrote %+v, want one edges row for the plan", rows)
	}

	all, _ := c.GetMemoryChangesSince(context.Background(), "tnt", 0, 1000)
	ends := map[string]string{}
	for _, r := range all {
		if r.Type == store.DocumentEdgesChanged && r.Seq <= head {
			ends[r.DocumentID] = r.ChunkID
		}
	}
	if ends[s.planDoc] != s.planChunk || ends[s.guideDoc] != s.guideChunk || len(ends) != 2 {
		t.Errorf("the cross-document link wrote edges rows %v, want the plan's part under the plan and the guide's under the guide", ends)
	}
}

// TestDocumentChangeFeed_NothingIsAppendedWithCaptureOff pins that the
// Document tool's own appends go through the decorator only: with the feed off
// (a plain store) the table stays empty, as RFC CD Part C promises.
func TestDocumentChangeFeed_NothingIsAppendedWithCaptureOff(t *testing.T) {
	d, ctx, raw := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	seedCopyDocs(t, d, ctx)
	if rows, err := raw.GetMemoryChangesSince(context.Background(), "tnt", 0, 100); err != nil || len(rows) != 0 {
		t.Errorf("capture off wrote %d change rows (err %v), want none", len(rows), err)
	}
}

// TestDocumentChangeFeed_MetadataChangesAreAnnounced: an op that changes a
// chunk or a document without rewriting a chunk body (a retitle, a status, a
// move, a reorder, a new Path name) writes no keyed row for the decorator to
// see, so the Document tool announces it itself — a viewer that labels by title
// and colours by status would otherwise never hear of it.
func TestDocumentChangeFeed_MetadataChangesAreAnnounced(t *testing.T) {
	d, ctx, c := captureFixture(t)
	s := seedCopyDocs(t, d, ctx)
	sibling := asStr(copyExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+s.planDoc+`","title":"Sibling","body":"x"}`)["id"])
	rev := func(id string) int {
		out, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
		if r.IsError {
			t.Fatalf("get_chunk: %s", r.Text)
		}
		return int(out["revision"].(float64))
	}
	for _, step := range []struct {
		name  string
		body  func() string // built when the step runs: a revision moves
		chunk string
	}{
		{"retitle", func() string {
			return fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%d,"title":"Renamed"}`, s.planChunk, rev(s.planChunk))
		}, s.planChunk},
		{"status", func() string {
			return fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%d,"status":"done"}`, s.planChunk, rev(s.planChunk))
		}, s.planChunk},
		{"move", func() string {
			return fmt.Sprintf(`{"op":"move_chunk","scope":"user","id":%q,"new_parent_id":%q}`, s.planChunk, sibling)
		}, s.planChunk},
		{"reorder", func() string {
			return fmt.Sprintf(`{"op":"reorder_chunk","scope":"user","id":%q,"direction":"up"}`, sibling)
		}, sibling},
		{"set_path", func() string {
			return fmt.Sprintf(`{"op":"set_path","scope":"user","id":%q,"path":"/kb/moved-plan"}`, s.planDoc)
		}, ""},
	} {
		head, _ := c.MemoryChangesHead(context.Background(), "tnt")
		if _, r := docExec(t, d, ctx, step.body()); r.IsError {
			t.Fatalf("%s: %s", step.name, r.Text)
		}
		rows, _ := c.GetMemoryChangesSince(context.Background(), "tnt", head, 100)
		found := false
		for _, r := range rows {
			if r.Type == store.DocumentChangeUpdated && r.DocumentID == s.planDoc && (step.chunk == "" || r.ChunkID == step.chunk) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no document.chunk.updated row for the plan (got %+v)", step.name, rows)
		}
	}
	// A failed op announces nothing.
	head, _ := c.MemoryChangesHead(context.Background(), "tnt")
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+s.planChunk+`","revision":1,"title":"stale"}`); !r.IsError {
		t.Fatal("a stale revision was accepted")
	}
	if rows, _ := c.GetMemoryChangesSince(context.Background(), "tnt", head, 100); len(rows) != 0 {
		t.Errorf("a refused update wrote %d change rows, want none", len(rows))
	}
}
