package builtin

import "testing"

// TestOntologyFingerprint_SeesAReparentWithoutTheClock (#2766 review): move_chunk bumps
// no revision, so a re-parent written in the same clock tick as the last change (or
// under a clock that went back) was invisible to a fingerprint of count, max(updated_at)
// and sum(revision).
func TestOntologyFingerprint_SeesAReparentWithoutTheClock(t *testing.T) {
	f := newFingerprintFixture(t)
	before := ontologyFingerprintNow(t, f.d, f.ctx)
	key := mustTenantKey(t, f.d, f.ctx)
	if err := f.d.exec(f.ctx, key, `UPDATE chunks SET parent_id = ? WHERE id = ?`, f.chunk("Provision"), f.chunk("Memo")); err != nil {
		t.Fatal(err)
	}
	if after := ontologyFingerprintNow(t, f.d, f.ctx); after == before {
		t.Error("a re-parent that left updated_at and revision alone left the fingerprint as it was")
	}
}

// TestMoveChunk_ParentChangedSinceTheChecksIsRefused: move_chunk's write is conditional on
// the parent its checks read, so a move that lands in between is a conflict, not a move
// past the checks. Also for a chunk whose parent was NULL (a root), which the conditional
// write must match too.
func TestMoveChunk_ParentChangedSinceTheChecksIsRefused(t *testing.T) {
	f := newFingerprintFixture(t)
	provision, event := f.chunk("Provision"), f.chunk("event")
	key := mustTenantKey(t, f.d, f.ctx)
	f.d.beforeMoveWrite = func() {
		f.d.beforeMoveWrite = nil
		if err := f.d.exec(f.ctx, key, `UPDATE chunks SET parent_id = ? WHERE id = ?`, f.chunk("Document"), provision); err != nil {
			t.Fatal(err)
		}
	}
	r := f.change(f.ctx, "move_chunk", provision, map[string]any{"new_parent_id": event})
	if !r.IsError || r.Error == nil || r.Error.Category != "business" {
		t.Fatalf("a move whose parent changed after its checks = %q %+v, want a business conflict", r.Text, r.Error)
	}
	if termParent(t, f.read(), "Provision") != "Document" {
		t.Errorf("Provision's parent = %q, want the concurrent move's Document", termParent(t, f.read(), "Provision"))
	}
	// A plain move, and a reorder of a parentless root, still apply.
	if r := f.change(f.ctx, "move_chunk", provision, map[string]any{"new_parent_id": event}); r.IsError {
		t.Errorf("move after re-reading: %s", r.Text)
	}
	doc := f.must(map[string]any{"op": "create_document", "scope": "user", "title": "Loose"})
	root := doc["root_chunk_id"].(string)
	if _, r := f.do(f.ctx, map[string]any{"op": "move_chunk", "scope": "user", "id": root, "position": 0}); r.IsError {
		t.Errorf("a parentless root's move in place: %s", r.Text)
	}
}
