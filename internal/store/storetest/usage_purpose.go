package storetest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// testTokenUsagePurposeRoundTrip holds both backends to the transcription
// columns on the ledger: a row's purpose and audio seconds read back as
// written, a row written without a purpose reads back as chat with no seconds,
// and a purpose or a second count neither backend could read back is refused.
func testTokenUsagePurposeRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	const run = "r_usage_purpose"
	rows := []store.TokenUsageRow{
		// A loop call, written the way every caller wrote one before purpose.
		{RunID: run, TenantID: "tp", Iteration: 0, Provider: "anthropic", Model: "m-chat",
			CredentialSource: "operator", InputTokens: 100, OutputTokens: 10, Cost: 0.01, CostCurrency: "USD"},
		// A vision OCR call: an ordinary token row with its purpose.
		{RunID: run, TenantID: "tp", Iteration: 1, Provider: "anthropic", Model: "m-vision",
			CredentialSource: "operator", InputTokens: 3000, OutputTokens: 900, Cost: 0.05, CostCurrency: "USD",
			Purpose: store.UsagePurposeOCR},
		// An STT call: no tokens, seconds and a per-minute cost.
		{RunID: run, TenantID: "tp", Iteration: 2, Provider: "whisper-local", Model: "whisper-large-v3",
			CredentialSource: "operator", Cost: 0.17, CostCurrency: "USD",
			Purpose: store.UsagePurposeSTT, AudioSeconds: 2550.5},
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage(%s): %v", r.Purpose, err)
		}
	}
	got, err := s.TokenUsageForRun(ctx, run)
	if err != nil || len(got) != 3 {
		t.Fatalf("TokenUsageForRun = %d rows, %v; want 3", len(got), err)
	}
	want := []struct {
		purpose string
		seconds float64
	}{{store.UsagePurposeChat, 0}, {store.UsagePurposeOCR, 0}, {store.UsagePurposeSTT, 2550.5}}
	for i, w := range want {
		if got[i].Purpose != w.purpose || got[i].AudioSeconds != w.seconds {
			t.Errorf("row %d = (purpose %q, seconds %v), want (%q, %v)",
				i, got[i].Purpose, got[i].AudioSeconds, w.purpose, w.seconds)
		}
	}
	if got[2].InputTokens != 0 || got[2].OutputTokens != 0 || got[2].Cost != 0.17 || got[2].CostCurrency != "USD" {
		t.Errorf("stt row = %+v, want no tokens and a 0.17 USD cost", got[2])
	}

	for _, bad := range []store.TokenUsageRow{
		{RunID: run, TenantID: "tp", Provider: "p", Model: "m", CredentialSource: "operator", Purpose: "translate"},
		{RunID: run, TenantID: "tp", Provider: "p", Model: "m", CredentialSource: "operator", Purpose: store.UsagePurposeSTT, AudioSeconds: -1},
		{RunID: run, TenantID: "tp", Provider: "p", Model: "m", CredentialSource: "operator", Purpose: store.UsagePurposeSTT, AudioSeconds: math.NaN()},
	} {
		if err := s.RecordCallUsage(ctx, bad); err == nil {
			t.Errorf("RecordCallUsage(purpose %q, seconds %v) succeeded, want refused", bad.Purpose, bad.AudioSeconds)
		}
	}
	if after, _ := s.TokenUsageForRun(ctx, run); len(after) != 3 {
		t.Errorf("a refused row was written: %d rows, want 3", len(after))
	}
}

// testUsageRollupAcrossPurposes rolls rows of every purpose with otherwise the
// same report dimensions into the archive. The report must keep every token,
// cost and call through the fold; a second pass must not count them twice.
// (That the archive keeps the purpose and the seconds apart is checked per
// backend, against the table, and through the report in
// testUsageReportByPurpose.)
func testUsageRollupAcrossPurposes(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	mk := func(purpose string, in int, seconds, cost float64) store.TokenUsageRow {
		return store.TokenUsageRow{RunID: "r_purpose_rollup", TenantID: "tpr", Provider: "p", Model: "m",
			CredentialSource: "operator", InputTokens: in, Cost: cost, CostCurrency: "USD", TS: old,
			Purpose: purpose, AudioSeconds: seconds}
	}
	for _, r := range []store.TokenUsageRow{
		mk("", 100, 0, 1), mk(store.UsagePurposeOCR, 200, 0, 2), mk(store.UsagePurposeSTT, 0, 60, 0.5), mk(store.UsagePurposeSTT, 0, 30, 0.25),
	} {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-5 * 24 * time.Hour)
	if pruned, err := s.RollupAndPruneUsage(ctx, cutoff); err != nil || pruned != 4 {
		t.Fatalf("RollupAndPruneUsage = %d, %v; want 4 pruned", pruned, err)
	}
	if again, err := s.RollupAndPruneUsage(ctx, cutoff); err != nil || again != 0 {
		t.Fatalf("second RollupAndPruneUsage = %d, %v; want 0", again, err)
	}
	rep, err := s.UsageReport(ctx, store.UsageQuery{TenantID: "tpr"})
	if err != nil || len(rep) != 1 {
		t.Fatalf("UsageReport = %+v, %v; want one total row", rep, err)
	}
	if a := rep[0]; a.InputTokens != 300 || a.Cost != 3.75 || a.CallCount != 4 || a.UnpricedCalls != 0 || a.AudioSeconds != 90 {
		t.Errorf("archived total = (in %d, cost %v, calls %d, unpriced %d, seconds %v), want (300, 3.75, 4, 0, 90)",
			a.InputTokens, a.Cost, a.CallCount, a.UnpricedCalls, a.AudioSeconds)
	}
}

// testUsageReportByPurpose groups the report by purpose over recent rows AND
// archived ones: each purpose is its own group on both sides of the union, the
// audio seconds an STT call transcribed are summed through the fold, and
// purpose composes with the other dimensions. An operator reads what OCR and
// speech-to-text cost from this report.
func testUsageReportByPurpose(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	mk := func(tenant, purpose string, in int, seconds, cost float64, ts time.Time) store.TokenUsageRow {
		return store.TokenUsageRow{RunID: "r_purpose_report", TenantID: tenant, Provider: "p", Model: "m",
			CredentialSource: "operator", InputTokens: in, Cost: cost, CostCurrency: "USD", TS: ts,
			Purpose: purpose, AudioSeconds: seconds}
	}
	for _, r := range []store.TokenUsageRow{
		// Archived once the rollup runs.
		mk("tpa", "", 100, 0, 1, old),
		mk("tpa", store.UsagePurposeOCR, 200, 0, 2, old),
		mk("tpa", store.UsagePurposeSTT, 0, 60, 0.5, old),
		// Recent.
		mk("tpa", store.UsagePurposeChat, 10, 0, 0.1, time.Time{}),
		mk("tpa", store.UsagePurposeOCR, 20, 0, 0.2, time.Time{}),
		mk("tpa", store.UsagePurposeSTT, 0, 30, 0.25, time.Time{}),
		// Another tenant's OCR, to show purpose composes with tenant.
		mk("tpb", store.UsagePurposeOCR, 7, 0, 0.07, time.Time{}),
	} {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if pruned, err := s.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil || pruned != 3 {
		t.Fatalf("RollupAndPruneUsage = %d, %v; want 3 pruned", pruned, err)
	}

	rep, err := s.UsageReport(ctx, store.UsageQuery{TenantID: "tpa", GroupBy: []store.UsageDimension{store.UsageByPurpose}})
	if err != nil {
		t.Fatalf("UsageReport by purpose: %v", err)
	}
	byPurpose := map[string]store.UsageAggregate{}
	for _, a := range rep {
		if a.TenantID != "" || a.Provider != "" || a.CredentialSource != "" {
			t.Errorf("an ungrouped dimension is set on %+v", a)
		}
		if _, dup := byPurpose[a.Purpose]; dup {
			t.Errorf("purpose %q appears twice: recent and archived rows were not merged", a.Purpose)
		}
		byPurpose[a.Purpose] = a
	}
	if len(byPurpose) != 3 {
		t.Fatalf("report by purpose = %+v, want chat, ocr and stt", rep)
	}
	for _, w := range []struct {
		purpose      string
		in, calls    int64
		cost, second float64
	}{
		{store.UsagePurposeChat, 110, 2, 1.1, 0},
		{store.UsagePurposeOCR, 220, 2, 2.2, 0},
		{store.UsagePurposeSTT, 0, 2, 0.75, 90},
	} {
		a := byPurpose[w.purpose]
		if a.InputTokens != w.in || a.CallCount != w.calls || math.Abs(a.Cost-w.cost) > 1e-9 || a.AudioSeconds != w.second {
			t.Errorf("%s = (in %d, calls %d, cost %v, seconds %v), want (%d, %d, %v, %v)",
				w.purpose, a.InputTokens, a.CallCount, a.Cost, a.AudioSeconds, w.in, w.calls, w.cost, w.second)
		}
	}

	// Composed with tenant, across both tenants.
	both, err := s.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageByPurpose}})
	if err != nil {
		t.Fatalf("UsageReport by tenant, purpose: %v", err)
	}
	var tpbOCR, tpaOCR int64
	for _, a := range both {
		if a.Purpose == store.UsagePurposeOCR {
			switch a.TenantID {
			case "tpa":
				tpaOCR += a.InputTokens
			case "tpb":
				tpbOCR += a.InputTokens
			}
		}
	}
	if tpaOCR != 220 || tpbOCR != 7 {
		t.Errorf("ocr input tokens by tenant = (tpa %d, tpb %d), want (220, 7)", tpaOCR, tpbOCR)
	}

	// Not grouped by purpose: one row, purpose empty, the seconds still summed.
	total, err := s.UsageReport(ctx, store.UsageQuery{TenantID: "tpa"})
	if err != nil || len(total) != 1 {
		t.Fatalf("UsageReport total = %+v, %v; want one row", total, err)
	}
	if a := total[0]; a.Purpose != "" || a.AudioSeconds != 90 || a.CallCount != 6 {
		t.Errorf("total = (purpose %q, seconds %v, calls %d), want (\"\", 90, 6)", a.Purpose, a.AudioSeconds, a.CallCount)
	}
}
