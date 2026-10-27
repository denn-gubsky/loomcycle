package storetest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// testTokenUsageTTSRoundTrip holds both backends to the text-to-speech ledger
// row: its purpose, the characters sent and the seconds produced read back as
// written; a chat row written the way every caller wrote one reads back with
// no characters; and a row whose units contradict its purpose is refused.
func testTokenUsageTTSRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	const run = "r_usage_tts"
	rows := []store.TokenUsageRow{
		{RunID: run, TenantID: "tt", Iteration: 0, Provider: "anthropic", Model: "m-chat",
			CredentialSource: "operator", InputTokens: 100, OutputTokens: 10, Cost: 0.01, CostCurrency: "USD"},
		// A TTS call: no tokens, the characters it sent, the audio it produced
		// and a cost priced per million characters.
		{RunID: run, TenantID: "tt", Iteration: 1, Provider: "openai", Model: "tts-1",
			CredentialSource: "tenant", Cost: 0.0657, CostCurrency: "USD",
			Purpose: store.UsagePurposeTTS, InputChars: 4380, AudioSeconds: 312.4},
	}
	for _, r := range rows {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage(%q): %v", r.Purpose, err)
		}
	}
	got, err := s.TokenUsageForRun(ctx, run)
	if err != nil || len(got) != 2 {
		t.Fatalf("TokenUsageForRun = %d rows, %v; want 2", len(got), err)
	}
	if c := got[0]; c.Purpose != store.UsagePurposeChat || c.InputChars != 0 || c.AudioSeconds != 0 || c.InputTokens != 100 {
		t.Errorf("chat row = %+v, want purpose chat, 100 tokens, no characters or seconds", c)
	}
	if v := got[1]; v.Purpose != store.UsagePurposeTTS || v.InputChars != 4380 || v.AudioSeconds != 312.4 ||
		v.InputTokens != 0 || v.OutputTokens != 0 || v.Cost != 0.0657 || v.CostCurrency != "USD" {
		t.Errorf("tts row = %+v, want purpose tts, 4380 chars, 312.4 s, no tokens, 0.0657 USD", v)
	}

	base := store.TokenUsageRow{RunID: run, TenantID: "tt", Provider: "p", Model: "m", CredentialSource: "operator"}
	for name, mut := range map[string]func(*store.TokenUsageRow){
		"negative chars":    func(r *store.TokenUsageRow) { r.Purpose, r.InputChars = store.UsagePurposeTTS, -1 },
		"chars on chat":     func(r *store.TokenUsageRow) { r.InputChars = 10 },
		"chars on ocr":      func(r *store.TokenUsageRow) { r.Purpose, r.InputChars = store.UsagePurposeOCR, 10 },
		"chars on stt":      func(r *store.TokenUsageRow) { r.Purpose, r.InputChars = store.UsagePurposeSTT, 10 },
		"seconds on chat":   func(r *store.TokenUsageRow) { r.AudioSeconds = 5 },
		"seconds on ocr":    func(r *store.TokenUsageRow) { r.Purpose, r.AudioSeconds = store.UsagePurposeOCR, 5 },
		"nan seconds (tts)": func(r *store.TokenUsageRow) { r.Purpose, r.AudioSeconds = store.UsagePurposeTTS, math.NaN() },
	} {
		r := base
		mut(&r)
		if err := s.RecordCallUsage(ctx, r); err == nil {
			t.Errorf("%s: RecordCallUsage succeeded, want refused", name)
		}
	}
	if after, _ := s.TokenUsageForRun(ctx, run); len(after) != 2 {
		t.Errorf("a refused row was written: %d rows, want 2", len(after))
	}
}

// testUsageReportTTSPurpose rolls TTS rows into the archive beside recent
// ones: grouped by purpose, tts is its own group on both sides of the union,
// its seconds and cost survive the fold, and a second pass counts nothing
// twice. The characters are not on the report; that the archive sums them is
// checked per backend, against the table.
func testUsageReportTTSPurpose(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	mk := func(purpose string, in int, chars int64, seconds, cost float64, ts time.Time) store.TokenUsageRow {
		return store.TokenUsageRow{RunID: "r_tts_report", TenantID: "ttr", Provider: "p", Model: "m",
			CredentialSource: "operator", InputTokens: in, Cost: cost, CostCurrency: "USD", TS: ts,
			Purpose: purpose, InputChars: chars, AudioSeconds: seconds}
	}
	for _, r := range []store.TokenUsageRow{
		mk("", 100, 0, 0, 1, old),
		mk(store.UsagePurposeTTS, 0, 4000, 240, 0.5, old),
		mk(store.UsagePurposeTTS, 0, 1000, 60, 0.25, old),
		mk(store.UsagePurposeTTS, 0, 500, 30, 0.125, time.Time{}),
	} {
		if err := s.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-5 * 24 * time.Hour)
	if pruned, err := s.RollupAndPruneUsage(ctx, cutoff); err != nil || pruned != 3 {
		t.Fatalf("RollupAndPruneUsage = %d, %v; want 3 pruned", pruned, err)
	}
	if again, err := s.RollupAndPruneUsage(ctx, cutoff); err != nil || again != 0 {
		t.Fatalf("second RollupAndPruneUsage = %d, %v; want 0", again, err)
	}
	rep, err := s.UsageReport(ctx, store.UsageQuery{TenantID: "ttr", GroupBy: []store.UsageDimension{store.UsageByPurpose}})
	if err != nil {
		t.Fatalf("UsageReport by purpose: %v", err)
	}
	by := map[string]store.UsageAggregate{}
	for _, a := range rep {
		if _, dup := by[a.Purpose]; dup {
			t.Errorf("purpose %q appears twice: recent and archived rows were not merged", a.Purpose)
		}
		by[a.Purpose] = a
	}
	if len(by) != 2 {
		t.Fatalf("report by purpose = %+v, want chat and tts", rep)
	}
	if a := by[store.UsagePurposeTTS]; a.InputTokens != 0 || a.CallCount != 3 || a.AudioSeconds != 330 || math.Abs(a.Cost-0.875) > 1e-9 {
		t.Errorf("tts = (in %d, calls %d, seconds %v, cost %v), want (0, 3, 330, 0.875)", a.InputTokens, a.CallCount, a.AudioSeconds, a.Cost)
	}
	if a := by[store.UsagePurposeChat]; a.InputTokens != 100 || a.CallCount != 1 || a.AudioSeconds != 0 {
		t.Errorf("chat = (in %d, calls %d, seconds %v), want (100, 1, 0)", a.InputTokens, a.CallCount, a.AudioSeconds)
	}
}
