package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestDeadLink_CollectsEveryUnreachableClass walks the four classes together,
// because they arrive together: an out-of-band chunk delete leaves all of them at
// once, and a reconciliation that cleaned three of four would leave the store
// looking repaired while a read still stumbled on the fourth.
func TestDeadLink_CollectsEveryUnreachableClass(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)
	key, mscope := deadLinkScope(t, d, ctx)

	// A real chunk with a body, an entity sidecar, an asset and an edge — then the
	// chunk row is deleted BEHIND the tool's back, the way a repair script or a
	// restore does it. Every reference survives and nothing can reach any of it.
	live := upsert(t, d, ctx, docID, "survivor", "survivor", "still here", "")
	doomed := upsert(t, d, ctx, docID, "doomed", "doomed", "about to be orphaned", "")
	linkChunks(t, d, ctx, doomed, live)
	setAsset(t, d, ctx, doomed)

	if err := d.exec(ctx, key, `DELETE FROM chunks WHERE id = ?`, doomed); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}

	// Dry run first: the counts must be visible WITHOUT deleting, which is how an
	// operator sizes this before arming it.
	dry, err := d.ReconcileDeadLinks(ctx, key, mscope, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Bodies != 1 || dry.Sidecars != 1 || dry.Edges != 1 || dry.Assets != 1 {
		t.Errorf("dry run should count one of each class, got %+v", dry)
	}
	if got := bodyExists(t, d, ctx, mscope, key, doomed); !got {
		t.Error("a dry run must not delete — the body is gone")
	}

	got, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Total() != 4 {
		t.Errorf("want 4 collected (body+sidecar+edge+asset), got %d: %+v", got.Total(), got)
	}
	if bodyExists(t, d, ctx, mscope, key, doomed) {
		t.Error("the orphaned body survived — no read returns it and no other sweeper reaps it")
	}
	// A second pass finds nothing: the reconciliation is idempotent, so a scope that
	// is clean does not keep reporting work.
	again, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if again.Total() != 0 {
		t.Errorf("a clean scope must report nothing, got %+v", again)
	}
}

// TestDeadLink_LeavesLiveReferencesAlone is the property that makes this safe to run
// unattended. Everything it deletes must be unreachable; touching a live reference
// would make an integrity pass the thing that breaks integrity.
func TestDeadLink_LeavesLiveReferencesAlone(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)
	key, mscope := deadLinkScope(t, d, ctx)

	a := upsert(t, d, ctx, docID, "a", "a", "body a", "")
	b := upsert(t, d, ctx, docID, "b", "b", "body b", "")
	linkChunks(t, d, ctx, a, b)
	setAsset(t, d, ctx, a)

	got, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Total() != 0 {
		t.Fatalf("a healthy scope must lose nothing, got %+v", got)
	}
	for _, id := range []string{a, b} {
		if !bodyExists(t, d, ctx, mscope, key, id) {
			t.Errorf("live body for %s was deleted", id)
		}
	}
	if n, _ := d.countWhere(ctx, key, "chunk_edges", "1=1"); n != 1 {
		t.Errorf("live edge count = %d, want 1", n)
	}
	if n, _ := d.countWhere(ctx, key, "chunk_assets", "1=1"); n != 1 {
		t.Errorf("live asset count = %d, want 1", n)
	}
	if n, _ := d.countWhere(ctx, key, "chunk_memory_meta", "1=1"); n != 2 {
		t.Errorf("live sidecar count = %d, want 2", n)
	}
}

// TestDeadLink_RefusesAScopeWithBodiesAndNoChunks is the guard that separates an
// integrity pass from data loss.
//
// Every decision here rests on the live chunk-id set. If that set is empty for the
// WRONG reason — a scope resolved with the wrong id, a schema not yet provisioned,
// a read that failed and was treated as "nothing there" — then every body in the
// scope looks orphaned and the sweeper deletes the lot. This subsystem has produced
// all three of those causes, so the shape is refused rather than trusted: a scope
// holding bodies but no chunk rows is not a scope whose every chunk was deleted.
func TestDeadLink_RefusesAScopeWithBodiesAndNoChunks(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)
	key, mscope := deadLinkScope(t, d, ctx)
	id := upsert(t, d, ctx, docID, "only", "only", "the only body", "")

	// Empty the chunk table, leaving the body — the shape of a mis-resolved scope.
	if err := d.exec(ctx, key, `DELETE FROM chunks`); err != nil {
		t.Fatalf("empty chunks: %v", err)
	}

	got, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Skipped == "" {
		t.Error("a scope with bodies and no chunk rows must be SKIPPED, not treated as fully deleted")
	}
	if got.Total() != 0 {
		t.Errorf("a skipped scope must delete nothing, got %+v", got)
	}
	if !bodyExists(t, d, ctx, mscope, key, id) {
		t.Error("the body was deleted despite the refusal — this is the data-loss path")
	}
}

// TestDeadLink_AChunkWhoseDocumentIsGoneIsReportedNotDeleted. Different authority
// from a dangling edge: that is unreachable and deleting it restores an invariant,
// whereas a chunk tree whose document row has gone may still be recoverable, and
// destroying it to tidy a count is not this sweeper's call.
func TestDeadLink_AChunkWhoseDocumentIsGoneIsReportedNotDeleted(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)
	key, mscope := deadLinkScope(t, d, ctx)
	id := upsert(t, d, ctx, docID, "stranded", "stranded", "body", "")

	if err := d.exec(ctx, key, `DELETE FROM documents WHERE id = ?`, docID); err != nil {
		t.Fatalf("delete the document row: %v", err)
	}

	got, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.OrphanChunks == 0 {
		t.Error("a chunk whose document is gone must be REPORTED")
	}
	if n, _ := d.countWhere(ctx, key, "chunks", "1=1"); n == 0 {
		t.Error("...and must NOT be deleted — it may still be recoverable")
	}
	if !bodyExists(t, d, ctx, mscope, key, id) {
		t.Error("its body must survive too; the chunk still points at it")
	}
}

// ---- helpers ----

func deadLinkScope(t *testing.T, d *Document, ctx context.Context) (sqlmem.ScopeKey, store.MemoryScope) {
	t.Helper()
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	return key, mscope
}

func bodyExists(t *testing.T, d *Document, ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID string) bool {
	t.Helper()
	for _, tenant := range bodyTenantsFor(key.Tenant) {
		if _, err := d.Store.MemoryGet(ctx, tenant, mscope, key.ScopeID, chunkBodyKey(chunkID)); err == nil {
			return true
		}
	}
	return false
}

func linkChunks(t *testing.T, d *Document, ctx context.Context, from, to string) {
	t.Helper()
	res, err := d.Execute(ctx, entityJSON(map[string]any{
		"op": "link_chunks", "scope": "user", "from_id": from, "to_id": to, "kind": "about",
	}))
	if err != nil || res.IsError {
		t.Fatalf("link_chunks: %v %s", err, res.Text)
	}
}

func setAsset(t *testing.T, d *Document, ctx context.Context, chunkID string) {
	t.Helper()
	// A 1x1 png, base64 — enough to make a real chunk_assets row.
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AARgwMDAwMAA8mAf/hZ0ZzAAAAAElFTkSuQmCC"
	res, err := d.Execute(ctx, entityJSON(map[string]any{
		"op": "set_asset", "scope": "user", "id": chunkID,
		"media_type": "image/png", "data": png,
	}))
	if err != nil || res.IsError {
		t.Fatalf("set_asset: %v %s", err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
}

// TestDeadLink_LiveChunksBeyondTheRowCapKeepTheirBodies is the data-loss regression.
// The live chunk-id set was read through the SQL Memory row cap, so in a scope holding
// more chunks than that cap every chunk past it looked unreachable and its body was
// deleted. Seen live: a benchmark scope crossed 10,000 chunks and the hourly sweep
// reaped the bodies of live chunks, scattered one per document. A cap of 3 against 8
// chunks reproduces it in miniature.
func TestDeadLink_LiveChunksBeyondTheRowCapKeepTheirBodies(t *testing.T) {
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), MaxRows: 3})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	assertRowCapKeepsLiveBodies(t, mgr)
}

// The Postgres tier is where the loss was seen, and it differs from the file tier in the
// two things the fix relies on: placeholder rebinding and the ordering of text ids.
func TestDeadLink_LiveChunksBeyondTheRowCapKeepTheirBodies_Postgres(t *testing.T) {
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres-tier dead-link test")
	}
	mgr, err := sqlmem.NewPostgres(context.Background(), sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 3})
	if err != nil {
		t.Fatalf("sqlmem.NewPostgres: %v", err)
	}
	assertRowCapKeepsLiveBodies(t, mgr)
}

func assertRowCapKeepsLiveBodies(t *testing.T, mgr *sqlmem.Manager) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	t.Cleanup(func() { _ = mgr.Close() })
	// A subject unique to this run, so a shared Postgres never hands the test a scope
	// left behind by an earlier one.
	subject := fmt.Sprintf("deadlink-cap-%d", time.Now().UnixNano())
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: subject, TenantID: "tnt"})
	d := &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}

	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)
	var ids []string
	for i := 0; i < 7; i++ {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":"s%d","body":"section %d"}`, doc, root, i, i))
		if r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
		ids = append(ids, o["id"].(string))
	}
	key, mscope := deadLinkScope(t, d, ctx)

	rep, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Bodies != 0 {
		t.Errorf("a scope with no orphans had %d LIVE bodies collected — the live set was read through the row cap", rep.Bodies)
	}
	for _, id := range ids {
		if !bodyExists(t, d, ctx, mscope, key, id) {
			t.Errorf("live chunk %s lost its body", id)
		}
	}
}

// TestDeadLink_AnOrphanPastTheFirstHundredBodiesIsCollected covers the other listing.
// Bodies were listed with limit 0, which the store reads as 100, so a scope examined
// only its first 100 bodies by key: a real orphan sorting after them was never
// collected, however many sweeps ran.
func TestDeadLink_AnOrphanPastTheFirstHundredBodiesIsCollected(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"D"}`)
	doc, root := out["document_id"].(string), out["root_chunk_id"].(string)
	last := ""
	for i := 0; i < 110; i++ {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":"s%d","body":"section %d"}`, doc, root, i, i))
		if r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
		if id := o["id"].(string); id > last {
			last = id
		}
	}
	key, mscope := deadLinkScope(t, d, ctx)
	// Orphan the chunk whose body key sorts LAST, so it is past the first 100 by key.
	if err := d.exec(ctx, key, `DELETE FROM chunks WHERE id = ?`, last); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}

	rep, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Bodies != 1 {
		t.Errorf("want the one orphaned body collected, got %d", rep.Bodies)
	}
	if bodyExists(t, d, ctx, mscope, key, last) {
		t.Error("the orphan's body survived — the listing never reached it")
	}
}
