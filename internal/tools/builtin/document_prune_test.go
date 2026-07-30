package builtin

import (
	"context"
	"testing"
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
