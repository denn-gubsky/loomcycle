package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// testUsageCallTimings reads the latest timed calls per model in a tenant and
// window on both backends: untimed rows and other tenants are left out, the
// per-model cap keeps the newest, and the dimension filters apply.
func testUsageCallTimings(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now()
	add := func(tenant, model string, ageMin int, dur int64, out int) {
		t.Helper()
		if err := s.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r_timing", TenantID: tenant, Provider: "p", Model: model,
			CredentialSource: "operator", OutputTokens: out, DurationMs: dur, TTFTMs: dur / 4, DecodeMs: dur / 2,
			TS: now.Add(-time.Duration(ageMin) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 4; i++ {
		add("t_timing", "fast", i, int64(100*i), 50) // newest has duration 100
	}
	add("t_timing", "slow", 1, 5000, 100)
	add("t_timing", "slow", 2, 0, 100) // untimed: no duration recorded
	add("t_other", "fast", 1, 99999, 1)

	rows, err := s.UsageCallTimings(ctx, store.UsageQuery{TenantID: "t_timing", From: now.Add(-time.Hour)}, 3)
	if err != nil {
		t.Fatal(err)
	}
	durs := map[string][]int64{}
	for _, r := range rows {
		durs[r.Model] = append(durs[r.Model], r.DurationMs)
		if r.DurationMs == 99999 {
			t.Errorf("another tenant's call was read: %+v", r)
		}
	}
	if len(durs["fast"]) != 3 || len(durs["slow"]) != 1 {
		t.Fatalf("timings = %v, want the 3 newest fast calls and the 1 timed slow call", durs)
	}
	for _, d := range durs["fast"] {
		if d == 400 {
			t.Errorf("the oldest fast call was read past the per-model cap: %v", durs["fast"])
		}
	}

	rows, err = s.UsageCallTimings(ctx, store.UsageQuery{TenantID: "t_timing",
		Filters: map[store.UsageDimension][]string{store.UsageByModel: {"slow"}}}, 10)
	if err != nil || len(rows) != 1 || rows[0].Model != "slow" || rows[0].OutputTokens != 100 || rows[0].DecodeMs != 2500 {
		t.Errorf("model=slow timings = %+v, %v; want the one timed slow call", rows, err)
	}
}
