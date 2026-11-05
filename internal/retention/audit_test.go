package retention

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// The audit family prunes each chain's old rows behind a signed anchor, and
// the chain still verifies from it — the retention-anchor guarantee. Off (no
// age, or no key) it removes nothing.
func TestSweeper_AuditPruneKeepsEveryChainVerifiable(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	keys := audit.ChainKeys{Current: []byte("retention-test-audit-key-32bytes")}
	sink, err := audit.NewStoreSink(st, keys)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		tenant string
		age    time.Duration
	}{
		{"acme", 400 * 24 * time.Hour}, {"acme", 300 * 24 * time.Hour}, {"acme", time.Hour},
		{"globex", 500 * 24 * time.Hour}, {"globex", 2 * time.Hour},
	} {
		if err := sink.Record(audit.Event{TS: now.Add(-tc.age), Action: "auth_login", ActorTenant: tc.tenant}); err != nil {
			t.Fatal(err)
		}
	}

	off := New(st, Config{AuditMaxAge: 0, AuditAnchorSeal: keys.SealAnchor(), Now: func() time.Time { return now }})
	if res, _ := off.sweepOnce(ctx); res.Audit != 0 {
		t.Fatalf("no audit age: pruned %d rows", res.Audit)
	}
	noKey := New(st, Config{AuditMaxAge: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if res, _ := noKey.sweepOnce(ctx); res.Audit != 0 {
		t.Fatalf("no anchor key: pruned %d rows", res.Audit)
	}

	sw := New(st, Config{AuditMaxAge: 90 * 24 * time.Hour, AuditAnchorSeal: keys.SealAnchor(), Now: func() time.Time { return now }})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// acme's two old rows, globex's one, and the operator chain's two
	// openings (recorded when each chain began, as old as its first row).
	if res.Audit != 5 {
		t.Errorf("pruned %d rows, want 5", res.Audit)
	}
	tenants, err := st.AuditTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reps, err := audit.VerifyAll(ctx, st, keys, tenants)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		if !rep.OK || rep.AnchorSeq == 0 {
			t.Errorf("after the prune: %+v, want a chain that verifies from its anchor", rep)
		}
	}
}
