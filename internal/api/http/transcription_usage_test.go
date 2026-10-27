package http

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func newUsageTestServer(t *testing.T) (*Server, *storesqlite.Store) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2}}
	return New(cfg, &stubResolver{p: &scriptedProvider{}}, []tools.Tool{}, concurrency.New(2, 2, time.Second), st), st
}

func runCtx(runID string) context.Context {
	ctx := tools.WithRunID(context.Background(), runID)
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1", UserID: "u1", AgentID: "clerk", SessionID: "s1"})
}

// TestRecordTranscriptionUsage_WritesPurposeSecondsAndTheCallersCost — one OCR
// and one STT unit land on the run's ledger with their purpose, the STT row's
// seconds, and the cost exactly as the caller priced it: an unpriced unit
// stores no cost, a priced one its amount and currency.
func TestRecordTranscriptionUsage_WritesPurposeSecondsAndTheCallersCost(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := runCtx("run-tx")

	if err := srv.RecordTranscriptionUsage(ctx, TranscriptionUsage{
		Purpose: store.UsagePurposeOCR, Provider: "anthropic", Model: "claude-vision",
		InputTokens: 3000, OutputTokens: 800,
	}); err != nil {
		t.Fatalf("record OCR: %v", err)
	}
	if err := srv.RecordTranscriptionUsage(ctx, TranscriptionUsage{
		Purpose: store.UsagePurposeSTT, Provider: "whisper-local", Model: "whisper-large-v3",
		CredentialSource: "tenant", CredentialScopeID: "t1",
		AudioSeconds: 90, Cost: 0.3, Currency: "USD", Priced: true,
	}); err != nil {
		t.Fatalf("record STT: %v", err)
	}

	rows, err := st.TokenUsageForRun(context.Background(), "run-tx")
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger = %d rows, %v; want 2", len(rows), err)
	}
	ocr, stt := rows[0], rows[1]
	if ocr.Purpose != store.UsagePurposeOCR || ocr.InputTokens != 3000 || ocr.OutputTokens != 800 ||
		ocr.AudioSeconds != 0 || ocr.CostCurrency != "" || ocr.Cost != 0 || ocr.CredentialSource != "operator" {
		t.Errorf("ocr row = %+v; want purpose ocr, its tokens, no cost, operator-paid", ocr)
	}
	if stt.Purpose != store.UsagePurposeSTT || stt.AudioSeconds != 90 || stt.InputTokens != 0 ||
		stt.Cost != 0.3 || stt.CostCurrency != "USD" || stt.CredentialSource != "tenant" || stt.CredentialScopeID != "t1" {
		t.Errorf("stt row = %+v; want purpose stt, 90 s, 0.3 USD, tenant-paid", stt)
	}
	for _, r := range rows {
		if r.TenantID != "t1" || r.UserID != "u1" || r.AgentID != "clerk" || r.SessionID != "s1" {
			t.Errorf("row not charged to the run's identity: %+v", r)
		}
	}
	// The per-run cost the run's finish writes comes from the same rows.
	if cost, cur, priced, err := st.RunCostSummary(context.Background(), "run-tx"); err != nil || cost != 0.3 || cur != "USD" || !priced {
		t.Errorf("RunCostSummary = (%v, %q, %v, %v), want (0.3, USD, true)", cost, cur, priced, err)
	}
}

// TestRecordTranscriptionUsage_RefusesWhatTheLedgerCannotMean — a chat or
// unknown purpose, seconds on an OCR unit, a bad second count, a priced unit
// with no currency, negative tokens or a missing provider/model are refused,
// and nothing is written; with no run on the context nothing is charged.
func TestRecordTranscriptionUsage_RefusesWhatTheLedgerCannotMean(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := runCtx("run-bad")
	ok := TranscriptionUsage{Purpose: store.UsagePurposeSTT, Provider: "p", Model: "m", AudioSeconds: 1}
	for name, mut := range map[string]func(*TranscriptionUsage){
		"chat purpose":       func(u *TranscriptionUsage) { u.Purpose = store.UsagePurposeChat },
		"empty purpose":      func(u *TranscriptionUsage) { u.Purpose = "" },
		"unknown purpose":    func(u *TranscriptionUsage) { u.Purpose = "translate" },
		"ocr with seconds":   func(u *TranscriptionUsage) { u.Purpose = store.UsagePurposeOCR },
		"negative seconds":   func(u *TranscriptionUsage) { u.AudioSeconds = -1 },
		"infinite seconds":   func(u *TranscriptionUsage) { u.AudioSeconds = math.Inf(1) },
		"priced, no unit":    func(u *TranscriptionUsage) { u.Priced, u.Cost = true, 1 },
		"negative cost":      func(u *TranscriptionUsage) { u.Priced, u.Cost, u.Currency = true, -1, "USD" },
		"negative tokens":    func(u *TranscriptionUsage) { u.InputTokens = -5 },
		"no provider":        func(u *TranscriptionUsage) { u.Provider = "" },
		"no model":           func(u *TranscriptionUsage) { u.Model = "" },
		"unknown cred owner": func(u *TranscriptionUsage) { u.CredentialSource = "someone" },
	} {
		u := ok
		mut(&u)
		if err := srv.RecordTranscriptionUsage(ctx, u); err == nil {
			t.Errorf("%s: recorded, want refused", name)
		}
	}
	if rows, _ := st.TokenUsageForRun(context.Background(), "run-bad"); len(rows) != 0 {
		t.Errorf("a refused unit was written: %+v", rows)
	}
	if err := srv.RecordTranscriptionUsage(context.Background(), ok); err != nil {
		t.Errorf("no run on the context: %v, want nil (nothing to charge)", err)
	}
	if rows, _ := st.TokenUsageForRun(context.Background(), ""); len(rows) != 0 {
		t.Errorf("a unit with no run was charged: %+v", rows)
	}
}

// TestRecordCallUsage_LoopRowIsUnchangedByPurpose is a GUARD, not a
// fail-before: the row a loop call writes is exactly what it was before the
// transcription columns existed, reading back as a chat row with no seconds.
func TestRecordCallUsage_LoopRowIsUnchangedByPurpose(t *testing.T) {
	srv, st := newUsageTestServer(t)
	rid := tools.RunIdentityValue{TenantID: "t1", UserID: "u1", AgentID: "a", RootRunID: "root"}
	_ = srv.recordCallUsage(context.Background(), "run-loop", rid, "s1", 3, &providers.Usage{
		InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 3, CacheReadTokens: 4,
		Provider: "anthropic", Model: "m", CredentialSource: "tenant", CredentialScopeID: "t1",
	})
	rows, err := st.TokenUsageForRun(context.Background(), "run-loop")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger = %d rows, %v", len(rows), err)
	}
	got := rows[0]
	got.TS = time.Time{}
	want := store.TokenUsageRow{
		RunID: "run-loop", SessionID: "s1", TenantID: "t1", UserID: "u1", AgentID: "a", ParentRunID: "root",
		Iteration: 3, Provider: "anthropic", Model: "m", CredentialSource: "tenant", CredentialScopeID: "t1",
		InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 3, CacheReadTokens: 4,
		Purpose: store.UsagePurposeChat,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loop row =\n %+v\nwant\n %+v", got, want)
	}
}

// TestUsageReport_UngroupedPurposeAddsOnlyAudioSeconds is a GUARD: a report not
// grouped by purpose carries the same fields whatever purposes the ledger
// holds, except that a group with speech-to-text calls also carries their
// audio_seconds; it counts an STT row as a priced call with no tokens.
func TestUsageReport_UngroupedPurposeAddsOnlyAudioSeconds(t *testing.T) {
	srv, _ := newUsageTestServer(t)
	ctx := runCtx("run-rep")
	_ = srv.recordCallUsage(ctx, "run-rep", tools.RunIdentity(ctx), "s1", 0, &providers.Usage{InputTokens: 100, Provider: "p", Model: "m"})
	if err := srv.RecordTranscriptionUsage(ctx, TranscriptionUsage{Purpose: store.UsagePurposeOCR, Provider: "p", Model: "m", InputTokens: 50}); err != nil {
		t.Fatal(err)
	}
	if err := srv.RecordTranscriptionUsage(ctx, TranscriptionUsage{Purpose: store.UsagePurposeSTT, Provider: "p", Model: "m",
		AudioSeconds: 60, Cost: 0.5, Currency: "USD", Priced: true}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/_usage?group_by=tenant")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var raw struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"cache_creation_tokens", "cache_read_tokens", "call_count", "cost", "currency",
		"input_tokens", "output_tokens", "tenant_id", "unpriced_calls"}
	var calls, unpriced, tokens float64
	for _, r := range raw.Rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if r["currency"] == nil {
			// An all-unpriced group omits currency (omitempty).
			wantNoCur := []string{"cache_creation_tokens", "cache_read_tokens", "call_count", "cost",
				"input_tokens", "output_tokens", "tenant_id", "unpriced_calls"}
			if !reflect.DeepEqual(keys, wantNoCur) {
				t.Errorf("row keys = %v, want %v", keys, wantNoCur)
			}
		} else {
			// The priced group is the STT call alone: it gains audio_seconds.
			wantSTT := append([]string{"audio_seconds"}, wantKeys...)
			if !reflect.DeepEqual(keys, wantSTT) {
				t.Errorf("row keys = %v, want %v", keys, wantSTT)
			}
			if r["audio_seconds"] != 60.0 {
				t.Errorf("audio_seconds = %v, want 60", r["audio_seconds"])
			}
		}
		calls += r["call_count"].(float64)
		unpriced += r["unpriced_calls"].(float64)
		tokens += r["input_tokens"].(float64)
	}
	if calls != 3 || unpriced != 2 || tokens != 150 {
		t.Errorf("report totals = (calls %v, unpriced %v, input %v), want (3, 2, 150)", calls, unpriced, tokens)
	}
}

// TestRecordTranscriptionUsage_WritesATTSUnitWithCharsAndSecondsOutsideTheBudget
// — a text-to-speech unit lands on the run's ledger with purpose tts, the
// characters it sent, the seconds it produced and the cost the caller priced;
// its characters and seconds are not tokens, so the token budget counters move
// only by the OCR unit's tokens beside it. (The budget half is a GUARD: what
// keeps tokens off a TTS unit is the refusal tested below.)
func TestRecordTranscriptionUsage_WritesATTSUnitWithCharsAndSecondsOutsideTheBudget(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := runCtx("run-tts")

	if err := srv.RecordTranscriptionUsage(ctx, TranscriptionUsage{
		Purpose: store.UsagePurposeOCR, Provider: "anthropic", Model: "claude-vision", InputTokens: 40, OutputTokens: 10,
	}); err != nil {
		t.Fatalf("record OCR: %v", err)
	}
	if err := srv.RecordTranscriptionUsage(ctx, TranscriptionUsage{
		Purpose: store.UsagePurposeTTS, Provider: "openai", Model: "tts-1",
		CredentialSource: "tenant", CredentialScopeID: "t1",
		InputChars: 4380, AudioSeconds: 312.4, Cost: 0.0657, Currency: "USD", Priced: true,
	}); err != nil {
		t.Fatalf("record TTS: %v", err)
	}

	rows, err := st.TokenUsageForRun(context.Background(), "run-tts")
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger = %d rows, %v; want 2", len(rows), err)
	}
	tts := rows[1]
	if tts.Purpose != store.UsagePurposeTTS || tts.InputChars != 4380 || tts.AudioSeconds != 312.4 ||
		tts.InputTokens != 0 || tts.OutputTokens != 0 || tts.Cost != 0.0657 || tts.CostCurrency != "USD" ||
		tts.CredentialSource != "tenant" || tts.CredentialScopeID != "t1" {
		t.Errorf("tts row = %+v; want purpose tts, 4380 chars, 312.4 s, no tokens, 0.0657 USD, tenant-paid", tts)
	}
	if tts.TenantID != "t1" || tts.UserID != "u1" || tts.AgentID != "clerk" || tts.SessionID != "s1" {
		t.Errorf("tts row not charged to the run's identity: %+v", tts)
	}
	if ocr := rows[0]; ocr.InputChars != 0 {
		t.Errorf("ocr row carries %d input chars, want 0", ocr.InputChars)
	}
	for _, sc := range []struct{ scope, id string }{{"operator", ""}, {"tenant", "t1"}, {"user", "u1"}} {
		if used := srv.limits.UsedFor(sc.scope, "t1", sc.id); used != 50 {
			t.Errorf("%s budget counter = %d, want 50 (the OCR tokens only)", sc.scope, used)
		}
	}
}

// TestRecordTranscriptionUsage_RefusesUnitsATTSUnitCannotCarry — tokens on a
// TTS unit, characters on an STT or OCR unit and a negative character count
// are refused and nothing is written.
func TestRecordTranscriptionUsage_RefusesUnitsATTSUnitCannotCarry(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := runCtx("run-tts-bad")
	ok := TranscriptionUsage{Purpose: store.UsagePurposeTTS, Provider: "p", Model: "m", InputChars: 10, AudioSeconds: 1}
	for name, mut := range map[string]func(*TranscriptionUsage){
		"tts with input tokens":  func(u *TranscriptionUsage) { u.InputTokens = 5 },
		"tts with output tokens": func(u *TranscriptionUsage) { u.OutputTokens = 5 },
		"negative chars":         func(u *TranscriptionUsage) { u.InputChars = -1 },
		"stt with chars":         func(u *TranscriptionUsage) { u.Purpose = store.UsagePurposeSTT },
		"ocr with chars":         func(u *TranscriptionUsage) { u.Purpose, u.AudioSeconds = store.UsagePurposeOCR, 0 },
		"tts negative seconds":   func(u *TranscriptionUsage) { u.AudioSeconds = -1 },
	} {
		u := ok
		mut(&u)
		if err := srv.RecordTranscriptionUsage(ctx, u); err == nil {
			t.Errorf("%s: recorded, want refused", name)
		}
	}
	if rows, _ := st.TokenUsageForRun(context.Background(), "run-tts-bad"); len(rows) != 0 {
		t.Errorf("a refused unit was written: %+v", rows)
	}
	if err := srv.RecordTranscriptionUsage(ctx, ok); err != nil {
		t.Errorf("the valid TTS unit: %v", err)
	}
}
