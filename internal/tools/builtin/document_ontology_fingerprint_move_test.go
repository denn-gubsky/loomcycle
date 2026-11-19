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
