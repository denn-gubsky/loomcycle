package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// periodTotals reads a bucketed report into period → input tokens.
func periodTotals(t *testing.T, s store.Store, q store.UsageQuery) map[string]int64 {
	t.Helper()
	rows, err := s.UsageReport(context.Background(), q)
	if err != nil {
		t.Fatalf("UsageReport(%+v): %v", q, err)
	}
	out := map[string]int64{}
	for _, r := range rows {
		if r.Period.IsZero() {
			t.Fatalf("bucketed row without a period: %+v", r)
		}
		out[r.Period.UTC().Format(time.RFC3339)] += r.InputTokens
	}
	return out
}

func sameTotals(t *testing.T, name string, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}

// testUsageReportBuckets splits a window into UTC hours, days, ISO weeks
// (Monday-first) and months on both backends, and gives the same answer after
// the older rows are folded into the day-bucketed archive.
func testUsageReportBuckets(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant = "t_buckets"
	for i, ts := range []string{
		"2026-03-02T10:00:00Z", // Monday
		"2026-03-02T15:30:00Z",
		"2026-03-04T09:00:00Z", // Wednesday, same ISO week
		"2026-03-09T01:00:00Z", // the next Monday
		"2026-04-01T00:30:00Z", // Wednesday; its week starts Monday 30 March
	} {
		if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r_buckets", TenantID: tenant, Iteration: i,
			Provider: "p", Model: "m", CredentialSource: "operator", InputTokens: 1 << i, TS: at(ts)}); err != nil {
			t.Fatal(err)
		}
	}
	q := func(b store.UsageBucket) store.UsageQuery {
		return store.UsageQuery{TenantID: tenant, Bucket: b}
	}
	days := map[string]int64{"2026-03-02T00:00:00Z": 3, "2026-03-04T00:00:00Z": 4, "2026-03-09T00:00:00Z": 8, "2026-04-01T00:00:00Z": 16}
	weeks := map[string]int64{"2026-03-02T00:00:00Z": 7, "2026-03-09T00:00:00Z": 8, "2026-03-30T00:00:00Z": 16}
	months := map[string]int64{"2026-03-01T00:00:00Z": 15, "2026-04-01T00:00:00Z": 16}
	sameTotals(t, "hours", periodTotals(t, s, q(store.UsageBucketHour)), map[string]int64{
		"2026-03-02T10:00:00Z": 1, "2026-03-02T15:00:00Z": 2, "2026-03-04T09:00:00Z": 4,
		"2026-03-09T01:00:00Z": 8, "2026-04-01T00:00:00Z": 16})
	sameTotals(t, "days", periodTotals(t, s, q(store.UsageBucketDay)), days)
	sameTotals(t, "weeks", periodTotals(t, s, q(store.UsageBucketWeek)), weeks)
	sameTotals(t, "months", periodTotals(t, s, q(store.UsageBucketMonth)), months)

	// Fold the March rows into the archive: days, weeks and months read the same.
	if _, err := s.RollupAndPruneUsage(ctx, at("2026-03-20T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	sameTotals(t, "days after rollup", periodTotals(t, s, q(store.UsageBucketDay)), days)
	sameTotals(t, "weeks after rollup", periodTotals(t, s, q(store.UsageBucketWeek)), weeks)
	sameTotals(t, "months after rollup", periodTotals(t, s, q(store.UsageBucketMonth)), months)

	// An unbucketed report carries no period.
	rows, err := s.UsageReport(ctx, store.UsageQuery{TenantID: tenant})
	if err != nil || len(rows) != 1 || !rows[0].Period.IsZero() || rows[0].InputTokens != 31 {
		t.Errorf("unbucketed = %+v, %v; want one row of 31 tokens with no period", rows, err)
	}
}

// testUsageReportFiltersAndNewDims filters on any dimension and groups by the
// tier, user tier, agent and class on both backends; the agent reads the
// detail window only, since the archive keeps no agent.
func testUsageReportFiltersAndNewDims(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant = "t_filters"
	old := time.Now().Add(-40 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	for _, r := range []store.TokenUsageRow{
		{Model: "a", Purpose: store.UsagePurposeChat, Tier: "middle", UserTier: "pro", AgentID: "writer", InputTokens: 1, TS: recent},
		{Model: "a", Purpose: store.UsagePurposeChat, Tier: "high", UserTier: "pro", AgentID: "writer", InputTokens: 2, TS: recent},
		{Model: "b", Purpose: store.UsagePurposeRerank, UserTier: "free", AgentID: "reader", InputTokens: 4, TS: recent},
		{Model: "c", Purpose: store.UsagePurposeTTS, UserTier: "free", AgentID: "reader", InputChars: 900, TS: recent},
		{Model: "a", Purpose: store.UsagePurposeChat, Tier: "middle", UserTier: "pro", AgentID: "writer", InputTokens: 8, TS: old},
	} {
		r.RunID, r.TenantID, r.Provider, r.CredentialSource = "r_filters", tenant, "p", "operator"
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RollupAndPruneUsage(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	report := func(q store.UsageQuery) []store.UsageAggregate {
		t.Helper()
		q.TenantID = tenant
		rows, err := s.UsageReport(ctx, q)
		if err != nil {
			t.Fatalf("UsageReport(%+v): %v", q, err)
		}
		return rows
	}
	byKey := func(rows []store.UsageAggregate, key func(store.UsageAggregate) string) map[string]int64 {
		out := map[string]int64{}
		for _, r := range rows {
			out[key(r)] += r.InputTokens
		}
		return out
	}

	sameTotals(t, "by tier", byKey(report(store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTier}}),
		func(a store.UsageAggregate) string { return a.Tier }), map[string]int64{"middle": 9, "high": 2, "": 4})
	sameTotals(t, "by user tier", byKey(report(store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByUserTier}}),
		func(a store.UsageAggregate) string { return a.UserTier }), map[string]int64{"pro": 11, "free": 4})
	sameTotals(t, "by class", byKey(report(store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByClass}}),
		func(a store.UsageAggregate) string { return a.Class }), map[string]int64{"foreground": 11, "background": 4})
	// The archived writer row (8 tokens) has no agent and is not counted here.
	sameTotals(t, "by agent", byKey(report(store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByAgent}}),
		func(a store.UsageAggregate) string { return a.AgentID }), map[string]int64{"writer": 3, "reader": 4})

	if rows := report(store.UsageQuery{Filters: map[store.UsageDimension][]string{store.UsageByModel: {"a"}}}); len(rows) != 1 || rows[0].InputTokens != 11 {
		t.Errorf("model=a = %+v, want 11 tokens across detail and archive", rows)
	}
	if rows := report(store.UsageQuery{Filters: map[store.UsageDimension][]string{store.UsageByClass: {store.UsageClassBackground}}}); len(rows) != 1 || rows[0].InputTokens != 4 || rows[0].InputChars != 900 {
		t.Errorf("class=background = %+v, want 4 tokens and 900 characters", rows)
	}
	if rows := report(store.UsageQuery{Filters: map[store.UsageDimension][]string{
		store.UsageByModel: {"a", "b"}, store.UsageByUserTier: {"pro"}}}); len(rows) != 1 || rows[0].InputTokens != 11 {
		t.Errorf("model in (a,b) and user tier pro = %+v, want 11", rows)
	}
	for name, q := range map[string]store.UsageQuery{
		"unknown dimension": {Filters: map[store.UsageDimension][]string{"tenant_id; DROP TABLE token_usage": {"x"}}},
		"unknown class":     {Filters: map[store.UsageDimension][]string{store.UsageByClass: {"sideways"}}},
	} {
		q.TenantID = tenant
		if _, err := s.UsageReport(ctx, q); err == nil {
			t.Errorf("%s: report succeeded, want refused", name)
		}
	}
}

// testUsageReportTopFoldsTheRest keeps the largest values of the first
// dimension and sums the rest into one "other" row per remaining key.
func testUsageReportTopFoldsTheRest(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant = "t_top"
	for _, r := range []struct {
		model string
		cost  float64
	}{{"m5", 5}, {"m4", 4}, {"m3", 3}, {"m1", 1}} {
		if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r_top", TenantID: tenant, Provider: "p", Model: r.model,
			CredentialSource: "operator", InputTokens: 1, Cost: r.cost, CostCurrency: "USD"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.UsageReport(ctx, store.UsageQuery{TenantID: tenant, GroupBy: []store.UsageDimension{store.UsageByModel}, Top: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Model != "m5" || rows[1].Model != "m4" || !rows[2].Other || rows[2].Model != "" ||
		rows[2].Cost != 4 || rows[2].CallCount != 2 {
		t.Errorf("top 2 = %+v, want m5, m4, then other (cost 4, 2 calls)", rows)
	}
}
