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
// backend, against the table: the report does not group by purpose.)
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
	if a := rep[0]; a.InputTokens != 300 || a.Cost != 3.75 || a.CallCount != 4 || a.UnpricedCalls != 0 {
		t.Errorf("archived total = (in %d, cost %v, calls %d, unpriced %d), want (300, 3.75, 4, 0)",
			a.InputTokens, a.Cost, a.CallCount, a.UnpricedCalls)
	}
}
