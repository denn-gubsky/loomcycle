package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestRecordEmbedCall_ChargesTheRunWithCharsAndTokens — an embedder call made
// inside a run is an embedding row of that run, with the characters sent, the
// tokens the provider reported, and a cost from the token price.
func TestRecordEmbedCall_ChargesTheRunWithCharsAndTokens(t *testing.T) {
	srv, st := newUsageTestServer(t)
	srv.cfg().Pricing = config.PricingConfig{Currency: "USD", Models: map[string]config.ModelPrice{"openai/te3s": {Input: 0.02}}}
	ctx := tools.WithRunID(context.Background(), "run-emb")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1", UserID: "u1", SessionID: "s1", UserTier: "pro"})
	srv.RecordEmbedCall(ctx, providers.EmbedCall{Provider: "openai", Model: "te3s", Texts: 2, Chars: 812, InputTokens: 500_000, TokensReported: true})

	rows, err := st.TokenUsageForRun(context.Background(), "run-emb")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v; want 1", rows, err)
	}
	r := rows[0]
	if r.Purpose != store.UsagePurposeEmbedding || r.TenantID != "t1" || r.UserID != "u1" || r.UserTier != "pro" ||
		r.InputChars != 812 || r.InputTokens != 500_000 || r.CostCurrency != "USD" || r.Cost < 0.0099 || r.Cost > 0.0101 {
		t.Errorf("row = %+v, want an embedding row of 812 chars / 500000 tokens costing 0.01 USD", r)
	}
	if r.Tier != "" {
		t.Errorf("embedding row tier = %q, want none: the tier did not choose the embedder", r.Tier)
	}
}

// TestRecordEmbedCall_NoCountIsRecordedUnpriced — a provider that reported no
// count leaves a row of characters, unpriced when the model is priced only by
// tokens, so the report flags it rather than showing it as free.
func TestRecordEmbedCall_NoCountIsRecordedUnpriced(t *testing.T) {
	srv, st := newUsageTestServer(t)
	srv.cfg().Pricing = config.PricingConfig{Currency: "USD", Models: map[string]config.ModelPrice{"gemini/ge1": {Input: 0.15}}}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t-g", UserID: "u-g"})
	srv.RecordEmbedCall(ctx, providers.EmbedCall{Provider: "gemini", Model: "ge1", Texts: 1, Chars: 40})

	rep, err := st.UsageReport(context.Background(), store.UsageQuery{TenantID: "t-g",
		GroupBy: []store.UsageDimension{store.UsageByPurpose}})
	if err != nil || len(rep) != 1 {
		t.Fatalf("report = %+v, %v; want one row", rep, err)
	}
	if a := rep[0]; a.Purpose != store.UsagePurposeEmbedding || a.UnpricedCalls != 1 || a.InputTokens != 0 || a.CallCount != 1 {
		t.Errorf("row = %+v, want one unpriced embedding call with no tokens", a)
	}
}

// tokenReportingEmbedder reports a token count per text, as a real driver does.
type tokenReportingEmbedder struct{ fakeEmbedder }

func (e *tokenReportingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	providers.ReportEmbedTokens(ctx, 5*len(texts))
	return e.fakeEmbedder.Embed(ctx, texts)
}

// TestEmbeddings_GatewayCallIsAnEmbeddingRowOfTheCaller — through the shared
// observed embedder, a /v1/embeddings call is written to the ledger as the
// caller's embedding row; its reported tokens are on the budget through that
// row, so no estimate is added on top.
func TestEmbeddings_GatewayCallIsAnEmbeddingRowOfTheCaller(t *testing.T) {
	ts, srv := gatewayBudgetServer(t, 1_000_000, 0)
	oe := providers.ObserveEmbedder(&tokenReportingEmbedder{fakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4}})
	oe.SetOnCall(srv.RecordEmbedCall)
	srv.SetEmbedder(oe)

	req := `{"model":"text-embedding-3-small","input":["` + strings.Repeat("embed this text ", 50) + `","two"]}`
	if code, body := bearerCall(t, ts, "tok-alice", "POST", "/v1/embeddings", req); code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	rep, err := srv.store.UsageReport(context.Background(), store.UsageQuery{TenantID: "acme",
		GroupBy: []store.UsageDimension{store.UsageByUser, store.UsageByPurpose}})
	if err != nil {
		t.Fatal(err)
	}
	// gatewayBudgetServer seeds the budget with a chat row of its own.
	var emb []store.UsageAggregate
	for _, a := range rep {
		if a.Purpose == store.UsagePurposeEmbedding {
			emb = append(emb, a)
		}
	}
	if len(emb) != 1 || emb[0].InputTokens != 10 || emb[0].CallCount != 1 || emb[0].UserID != "alice" {
		t.Errorf("embedding rows = %+v, want alice's one call of 10 tokens", emb)
	}
	// 10 reported tokens and nothing more: an estimate of the ~800 bytes sent
	// would add about 200.
	if used := srv.limits.UsedFor("tenant", "acme", "acme"); used != 10 {
		t.Errorf("tenant month-to-date = %d, want 10 (no estimate on top of a reported count)", used)
	}
}

// TestRecordEmbedCall_IsRecordedAfterItsCallerHasGone — a gateway client that
// hangs up after the provider answered still spent the tokens.
func TestRecordEmbedCall_IsRecordedAfterItsCallerHasGone(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx, cancel := context.WithCancel(tools.WithRunID(context.Background(), "run-emb-gone"))
	cancel()
	srv.RecordEmbedCall(ctx, providers.EmbedCall{Provider: "p", Model: "m", Texts: 1, Chars: 3, InputTokens: 2, TokensReported: true})
	if rows, err := st.TokenUsageForRun(context.Background(), "run-emb-gone"); err != nil || len(rows) != 1 {
		t.Errorf("rows = %+v, %v; want the call's row", rows, err)
	}
}
