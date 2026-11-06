package retention

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
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
	keys := auditchain.ChainKeys{Current: []byte("retention-test-audit-key-32bytes")}
	sink, err := auditchain.NewStoreSink(st, keys)
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

	off := New(st, Config{AuditMaxAge: 0, AuditKeys: &keys, Now: func() time.Time { return now }})
	if res, _ := off.sweepOnce(ctx); res.Audit != 0 {
		t.Fatalf("no audit age: pruned %d rows", res.Audit)
	}
	noKey := New(st, Config{AuditMaxAge: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if res, _ := noKey.sweepOnce(ctx); res.Audit != 0 {
		t.Fatalf("no anchor key: pruned %d rows", res.Audit)
	}

	sw := New(st, Config{AuditMaxAge: 90 * 24 * time.Hour, AuditKeys: &keys, Now: func() time.Time { return now }})
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
	reps, err := auditchain.VerifyAll(ctx, st, keys, tenants)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		if !rep.OK || rep.AnchorSeq == 0 {
			t.Errorf("after the prune: %+v, want a chain that verifies from its anchor", rep)
		}
	}
}

// A chain that does not verify is not pruned: pruning would re-sign an
// anchor over the tampered rows. The sweeper counts it, leaves the rows, and
// verification still reports the break; the other chains are pruned.
func TestSweeper_AuditPruneRefusesATamperedChain(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	keys := auditchain.ChainKeys{Current: []byte("retention-test-audit-key-32bytes")}
	sink, err := auditchain.NewStoreSink(st, keys)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, tenant := range []string{"acme", "acme", "acme", "globex", "globex"} {
		if err := sink.Record(audit.Event{TS: now.Add(-400 * 24 * time.Hour), Action: "auth_login", ActorTenant: tenant}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE audit_events SET actor = 'mallory' WHERE tenant_id = 'acme' AND seq = 2`); err != nil {
		t.Fatal(err)
	}
	sw := New(st, Config{AuditMaxAge: 90 * 24 * time.Hour, AuditKeys: &keys, Now: func() time.Time { return now }})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.AuditRefused != 1 {
		t.Errorf("refused %d chains, want 1 (acme)", res.AuditRefused)
	}
	if rows, _ := st.AuditEventsList(ctx, "acme", 0, 0); len(rows) != 3 {
		t.Errorf("acme keeps %d rows after the refused prune, want 3", len(rows))
	}
	if rows, _ := st.AuditEventsList(ctx, "globex", 0, 0); len(rows) != 0 {
		t.Errorf("globex keeps %d rows, want its 2 pruned", len(rows))
	}
	if rep, err := auditchain.Verify(ctx, st, keys, "acme"); err != nil || rep.OK || rep.BreakSeq != 2 {
		t.Errorf("acme after the sweep: %+v, %v; want the break at 2 still reported", rep, err)
	}
}
