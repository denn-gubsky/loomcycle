package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// testTokenUsageTierRoundTrip holds both backends to the tier columns and the
// wider purpose list: a row's model tier and user tier read back as written, a
// row written without them reads back empty, and every purpose the class table
// lists is accepted.
func testTokenUsageTierRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	const run = "r_usage_tier"
	purposes := []string{
		store.UsagePurposeGateway, store.UsagePurposeConsolidation, store.UsagePurposeRerank,
		store.UsagePurposeDecision, store.UsagePurposeCompaction, store.UsagePurposeRecap,
		store.UsagePurposeUnitGeneration, store.UsagePurposeImageDescription, store.UsagePurposeEmbedding,
	}
	rows := []store.TokenUsageRow{
		{RunID: run, TenantID: "tt", Iteration: 0, Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: 10, Tier: "middle", UserTier: "pro"},
		{RunID: run, TenantID: "tt", Iteration: 1, Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: 10, Tier: store.UsageTierPinned},
		{RunID: run, TenantID: "tt", Iteration: 2, Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: 10},
	}
	for i, p := range purposes {
		rows = append(rows, store.TokenUsageRow{RunID: run, TenantID: "tt", Iteration: 3 + i, Provider: "anthropic",
			Model: "m", CredentialSource: "operator", InputTokens: 1, Purpose: p})
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage(purpose %q, tier %q): %v", r.Purpose, r.Tier, err)
		}
	}
	got, err := s.TokenUsageForRun(ctx, run)
	if err != nil || len(got) != len(rows) {
		t.Fatalf("TokenUsageForRun = %d rows, %v; want %d", len(got), err, len(rows))
	}
	tiers := [][2]string{{"middle", "pro"}, {store.UsageTierPinned, ""}, {"", ""}}
	for i, w := range tiers {
		if got[i].Tier != w[0] || got[i].UserTier != w[1] {
			t.Errorf("row %d tiers = (%q, %q), want (%q, %q)", i, got[i].Tier, got[i].UserTier, w[0], w[1])
		}
	}
	for i, p := range purposes {
		if got[3+i].Purpose != p {
			t.Errorf("row %d purpose = %q, want %q", 3+i, got[3+i].Purpose, p)
		}
	}
}

// testUsageRollupAcrossTiers rolls rows that differ only in their tiers into
// the archive. The fold must not collide on the archive's key (a key without
// the tiers makes one INSERT update the same row twice, which Postgres
// refuses), and every token and call must survive it.
func testUsageRollupAcrossTiers(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	mk := func(tier, userTier string, in int) store.TokenUsageRow {
		return store.TokenUsageRow{RunID: "r_tier_rollup", TenantID: "ttr", Provider: "p", Model: "m",
			CredentialSource: "operator", InputTokens: in, Cost: 1, CostCurrency: "USD", TS: old,
			Tier: tier, UserTier: userTier}
	}
	for _, r := range []store.TokenUsageRow{
		mk("", "", 100), mk("middle", "", 200), mk("middle", "pro", 300), mk("middle", "pro", 400), mk(store.UsageTierPinned, "free", 500),
	} {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-5 * 24 * time.Hour)
	if pruned, err := s.RollupAndPruneUsage(ctx, cutoff); err != nil || pruned != 5 {
		t.Fatalf("RollupAndPruneUsage = %d, %v; want 5 pruned", pruned, err)
	}
	if again, err := s.RollupAndPruneUsage(ctx, cutoff); err != nil || again != 0 {
		t.Fatalf("second RollupAndPruneUsage = %d, %v; want 0", again, err)
	}
	rep, err := s.UsageReport(ctx, store.UsageQuery{TenantID: "ttr"})
	if err != nil || len(rep) != 1 {
		t.Fatalf("UsageReport = %+v, %v; want one total row", rep, err)
	}
	if a := rep[0]; a.InputTokens != 1500 || a.Cost != 5 || a.CallCount != 5 {
		t.Errorf("archived total = (in %d, cost %v, calls %d), want (1500, 5, 5)", a.InputTokens, a.Cost, a.CallCount)
	}
}
