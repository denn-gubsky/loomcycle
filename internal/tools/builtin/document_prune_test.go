package builtin

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// pruneFixture builds a document with three chunks: one live, one retired, one
// retired-but-evidential.
func pruneFixture(t *testing.T) (*Document, contextT, map[string]string) {
	t.Helper()
	d, ctx, docID, root := entityFixture(t)
	ids := map[string]string{}
	mk := func(label, key, extra string) string {
		out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"`+label+`","natural_key":"`+key+`"`+extra+`}`)
		if r.IsError {
			t.Fatalf("upsert %s: %s", label, r.Text)
		}
		ids[label] = asStr(out["id"])
		return ids[label]
	}
	mk("live", "k:live", "")
	// Retired long ago: invalid_at in the distant past.
	mk("retired", "k:retired", `,"valid_at":1000,"invalid_at":2000,"body":"text that must not survive"`)
	// Retired equally long ago, but EVIDENTIAL.
	mk("evidence", "k:evidence", `,"valid_at":1000,"invalid_at":2000,"class":"evidential"`)
	return d, ctx, ids
}

// TestPruneRetiredChunks_EvidentialIsExemptAtAnyAge is the safety property this
// family turns on. Derived material was distilled from something else and can be
// re-derived; evidential material IS the something else, so ageing it out loses
// what everything else was derived FROM. Mirrors the pinned-session exemption.
func TestPruneRetiredChunks_EvidentialIsExemptAtAnyAge(t *testing.T) {
	d, ctx, ids := pruneFixture(t)
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}

	// A cutoff far in the future: everything retired is old enough.
	n, err := d.PruneRetiredChunks(context.Background(), key, mscope, 1<<62, false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("want exactly 1 chunk pruned (the retired, non-evidential one), got %d", n)
	}
	// The retired one is gone.
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+ids["retired"]+`"}`); !r.IsError {
		t.Error("the retired chunk survived the prune")
	}
	// The evidential one, retired just as long ago, is NOT.
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+ids["evidence"]+`"}`); r.IsError {
		t.Errorf("evidential content was pruned: %s", r.Text)
	}
	// And the live one is untouched.
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+ids["live"]+`"}`); r.IsError {
		t.Errorf("a live chunk was pruned: %s", r.Text)
	}
}

// TestPruneRetiredChunks_LiveContentIsNeverEligible: a chunk with no end-timestamp
// has no retirement to age from, and one with no sidecar row at all is an ordinary
// document chunk. A prune that swept either would delete live documents.
func TestPruneRetiredChunks_LiveContentIsNeverEligible(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	// A plain chunk: no natural key, so no sidecar row.
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"plain"}`); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	n, err := d.PruneRetiredChunks(context.Background(), key, mscope, 1<<62, false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Errorf("nothing is retired, so nothing should be pruned; got %d", n)
	}
}

// TestPruneRetiredChunks_RespectsTheCutoff: content retired AFTER the cutoff stays.
// Without this the max-age setting would be decorative and every retired row would
// go on the first sweep.
func TestPruneRetiredChunks_RespectsTheCutoff(t *testing.T) {
	d, ctx, ids := pruneFixture(t)
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	// Cutoff BEFORE the retirement instant (2000) — nothing is old enough yet.
	n, err := d.PruneRetiredChunks(context.Background(), key, mscope, 1500, false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Errorf("content retired at 2000 must not be pruned with a cutoff of 1500; got %d", n)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+ids["retired"]+`"}`); r.IsError {
		t.Error("a chunk newer than the cutoff was pruned")
	}
}

// TestPruneRetiredChunks_DryRunCountsWithoutDeleting: an operator has to be able to
// size a destructive policy before switching it on.
func TestPruneRetiredChunks_DryRunCountsWithoutDeleting(t *testing.T) {
	d, ctx, ids := pruneFixture(t)
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	n, err := d.PruneRetiredChunks(context.Background(), key, mscope, 1<<62, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n != 1 {
		t.Errorf("dry run should report 1 eligible chunk, got %d", n)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+ids["retired"]+`"}`); r.IsError {
		t.Error("a dry run deleted something")
	}
}

// TestPruneRetiredChunks_LeavesNoOrphans is the cascade parity check. The prune must
// clean everything delete_chunk cleans — the sidecar row, the edges, and the BODY in
// the Memory plane. An orphaned body is invisible: no read returns it and no sweeper
// reaps it, which is the failure this schema's explicit-cascade discipline exists to
// prevent.
func TestPruneRetiredChunks_LeavesNoOrphans(t *testing.T) {
	d, ctx, ids := pruneFixture(t)
	retired := ids["retired"]
	// Give it a body and an edge so there is something to orphan.
	// The body was written at creation. It is NOT set here by a second upsert,
	// because an upsert re-asserts the fact as CURRENT: writeChunkMeta is
	// delete-then-insert, so an upsert carrying no invalid_at clears a previous
	// retirement. That revival is intended — writing a fact makes it true again,
	// matching the store's revive-on-write for a superseded key — but it means an
	// upsert cannot be used to prepare a retired fixture.
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.PruneRetiredChunks(context.Background(), key, mscope, 1<<62, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if n := sidecarRowsFor(t, d, ctx, retired); n != 0 {
		t.Errorf("the sidecar row survived the prune (%d rows)", n)
	}
	// The body must be gone from the Memory plane.
	if _, gerr := d.Store.MemoryGet(context.Background(), direntTenant(ctx), mscope, key.ScopeID, chunkBodyKey(retired)); gerr == nil {
		t.Error("the chunk BODY survived in the Memory plane — an orphan no read returns and no sweeper reaps")
	}
	// And no edge references it.
	res, qerr := d.SqlMem.Query(context.Background(), key,
		d.SqlMem.Rebind(`SELECT count(*) FROM chunk_edges WHERE from_id = ? OR to_id = ?`), []any{retired, retired})
	if qerr != nil {
		t.Fatalf("edge probe: %v", qerr)
	}
	if n := scanCount(res.Rows); n != 0 {
		t.Errorf("%d edge(s) still reference the pruned chunk", n)
	}
}

// TestPruneChunks_CascadeMatchesDeleteChunk — RFC CX-3.
//
// This file's header states the rule: "A prune that reimplemented the delete would
// drift from delete_chunk, and the drift would show up as orphaned edges or a body
// left behind — invisible, because nothing reads an orphan." It had drifted. Three
// tables were added to delete_chunk and never here, and one of them is
// `chunk_revisions`, whose `body` column holds the chunk's text — so a prune removed
// the fact and kept its words in a table no read path consults.
//
// The assertion is per-TABLE rather than "the chunk is gone", because the chunk row
// disappearing is exactly what the drift already did.
func TestPruneChunks_CascadeMatchesDeleteChunk(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	key := sidecarScope(t, d, ctx)

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/doomed","title":"Doomed","body":"The original wording.",
		"type":"fact","subject":"Dave","tags":["area/one"]}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])
	// A second write makes a revision-log row: the log is append-only per body change.
	if _, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/doomed","body":"A corrected wording."}`); r.IsError {
		t.Fatalf("re-upsert: %s", r.Text)
	}
	// Retire it so the prune is eligible to take it.
	if err := d.exec(ctx, key,
		`UPDATE chunk_memory_meta SET expired_at = ? WHERE chunk_id = ?`, 1000, id); err != nil {
		t.Fatalf("retire: %v", err)
	}

	for _, tbl := range []string{"chunk_tags", "chunk_revisions"} {
		if n := chunkRowsIn(t, d, ctx, tbl, id); n == 0 {
			t.Fatalf("%s has no row for the chunk before the prune — the test cannot prove "+
				"the cascade reaches a table that was already empty", tbl)
		}
	}

	if _, err := d.PruneRetiredChunks(ctx, key, store.MemoryScopeUser, 1<<62, false); err != nil {
		t.Fatalf("PruneRetiredChunks: %v", err)
	}

	for _, tbl := range []string{"chunks", "chunk_edges", "chunk_memory_meta",
		"chunk_tags", "chunk_revisions", "chunk_layout"} {
		if n := chunkRowsIn(t, d, ctx, tbl, id); n != 0 {
			t.Errorf("%s still holds %d row(s) for the pruned chunk — an orphan nothing reads "+
				"and nothing reaps", tbl, n)
		}
	}
}

// TestPruneScopeChunks_EmptiesTheScopeIncludingBodies — the whole-scope reclaim's
// half. It takes every chunk regardless of age or class: the agent is gone, so
// there is nothing left for an evidential exemption to protect.
func TestPruneScopeChunks_EmptiesTheScopeIncludingBodies(t *testing.T) {
	d, ctx, st := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	key := sidecarScope(t, d, ctx)

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/live","title":"Live","body":"Never retired.",
		"type":"fact","subject":"Dave","class":"evidential"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])

	n, err := d.PruneScopeChunks(ctx, key, store.MemoryScopeUser, false)
	if err != nil {
		t.Fatalf("PruneScopeChunks: %v", err)
	}
	if n == 0 {
		t.Fatal("PruneScopeChunks pruned nothing — a live, evidential chunk must still go " +
			"when the scope itself is being reclaimed")
	}
	if c := chunkRowsIn(t, d, ctx, "chunks", id); c != 0 {
		t.Errorf("the chunk row survived a whole-scope prune")
	}
	if _, err := st.MemoryGet(ctx, direntTenant(ctx), store.MemoryScopeUser, key.ScopeID,
		chunkBodyKey(id)); err == nil {
		t.Errorf("the chunk BODY survived — it is the half a scope drop cannot reach, " +
			"which is the whole reason this method exists")
	}
}

// TestPruneScopeChunks_DryRunCountsWithoutDeleting.
func TestPruneScopeChunks_DryRunCountsWithoutDeleting(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	key := sidecarScope(t, d, ctx)
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/x","title":"X","body":"Text.","type":"fact","subject":"Dave"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])
	n, err := d.PruneScopeChunks(ctx, key, store.MemoryScopeUser, true)
	if err != nil {
		t.Fatalf("PruneScopeChunks dry run: %v", err)
	}
	if n == 0 {
		t.Error("dry run reported nothing to prune")
	}
	if c := chunkRowsIn(t, d, ctx, "chunks", id); c == 0 {
		t.Error("the dry run deleted the chunk")
	}
}

// chunkRowsIn counts a chunk's rows in one cascade table. The edge table is keyed
// on both endpoints, and `chunks` on `id`, so the column varies by table — which is
// precisely the per-table detail a drift test has to get right.
func chunkRowsIn(t *testing.T, d *Document, ctx context.Context, table, chunkID string) int {
	t.Helper()
	switch table {
	case "chunks":
		return countRows(t, d, ctx, `SELECT COUNT(*) FROM chunks WHERE id = ?`, chunkID)
	case "chunk_edges":
		return countRows(t, d, ctx,
			`SELECT COUNT(*) FROM chunk_edges WHERE from_id = ? OR to_id = ?`, chunkID, chunkID)
	default:
		return countRows(t, d, ctx, `SELECT COUNT(*) FROM `+table+` WHERE chunk_id = ?`, chunkID)
	}
}
