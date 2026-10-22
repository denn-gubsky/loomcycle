package limits

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestMonthToDate_TotalsEachPairsMonthFromARealStore runs MonthToDate over a
// real store rather than the fake, whose UsageReport ignores the query: the
// totals are right only if MonthToDate asks for the ledger since the month
// start, grouped by tenant AND user. The store answering that query the same
// way on SQLite and Postgres is the store contract's to pin (UsageRowsWithNoRun).
func TestMonthToDate_TotalsEachPairsMonthFromARealStore(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	month := MonthStart(now)
	row := func(tenant, user string, ts time.Time, in, out, cacheCreate, cacheRead int) store.TokenUsageRow {
		return store.TokenUsageRow{TenantID: tenant, UserID: user, Provider: "p", Model: "m", CredentialSource: "operator",
			InputTokens: in, OutputTokens: out, CacheCreationTokens: cacheCreate, CacheReadTokens: cacheRead, TS: ts}
	}
	for _, r := range []store.TokenUsageRow{
		row("acme", "alice", now, 100, 20, 3, 1),
		row("acme", "alice", now, 5, 0, 0, 0),
		row("acme", "bob", now, 7, 0, 0, 0),
		row("acme", "", now, 10, 1, 0, 0),
		row("globex", "alice", now, 40, 0, 0, 0),
		// Last month's: outside the window.
		row("acme", "alice", month.Add(-time.Hour), 1000, 0, 0, 0),
	} {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage(%+v): %v", r, err)
		}
	}
	for _, c := range []store.UsageCarryRow{
		{TenantID: "acme", UserID: "bob", Month: month, Tokens: 50},
		{TenantID: "acme", UserID: "bob", Month: month.AddDate(0, -1, 0), Tokens: 9999},
	} {
		if _, err := s.UsageCarryRaise(ctx, c); err != nil {
			t.Fatalf("UsageCarryRaise(%+v): %v", c, err)
		}
	}

	got, err := MonthToDate(ctx, s, month)
	if err != nil {
		t.Fatal(err)
	}
	want := map[UsageKey]int64{
		{"acme", "alice"}:   100 + 20 + 3 + 1 + 5,
		{"acme", "bob"}:     7 + 50,
		{"acme", ""}:        11,
		{"globex", "alice"}: 40,
	}
	if len(got) != len(want) {
		t.Fatalf("MonthToDate = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("MonthToDate[%v] = %d, want %d", k, got[k], v)
		}
	}
}
