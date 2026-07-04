package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestUsageAttribution_RecordsPerCallLedgerAndRunSummary is the RFC AV Phase 1
// end-to-end: a run whose driver-stamped Usage carries a credential source
// produces a token_usage row (priced from the operator table) AND a per-run
// cost + credential_source summary on the runs row. A tenant-key run attributes
// to the tenant; an operator-key run to the operator — the operator-vs-tenant
// distinction the whole feature exists for.
func TestUsageAttribution_RecordsPerCallLedgerAndRunSummary(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"solo": {Model: "stub-model", SystemPrompt: "you are solo"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
		// loomcycle owns pricing (RFC AV O1): $3 / 1M input, $15 / 1M output.
		Pricing: config.PricingConfig{
			Currency: "USD",
			Models:   map[string]config.ModelPrice{"scripted/stub-model": {Input: 3, Output: 15}},
		},
	}
	cfg.Env.AuthToken = ""

	// Call 0 (run A): a tenant's own key paid. Call 1 (run B): operator key
	// (empty source ⇒ recorded as "operator"). Model is set so the pricing key
	// "scripted/stub-model" resolves.
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{
					InputTokens: 1000, OutputTokens: 500, Model: "stub-model",
					CredentialSource: "tenant", CredentialScopeID: "tenant-x"}},
			},
			{
				{Type: providers.EventText, Text: "hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{
					InputTokens: 2000, OutputTokens: 1000, Model: "stub-model"}}, // no source ⇒ operator
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	postRun := func(agentID string) {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"solo","agent_id":"`+agentID+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
		))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, b)
		}
		_, _ = io.ReadAll(resp.Body) // drain to completion (finishRun runs in-path)
	}
	postRun("solo-tenant")
	postRun("solo-operator")

	ctx := context.Background()

	// --- Run A: tenant key paid. ---
	runA, err := st.GetRunByAgentID(ctx, "solo-tenant")
	if err != nil {
		t.Fatalf("GetRunByAgentID(solo-tenant): %v", err)
	}
	rowsA, err := st.TokenUsageForRun(ctx, runA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsA) != 1 {
		t.Fatalf("run A ledger rows = %d, want 1", len(rowsA))
	}
	rA := rowsA[0]
	if rA.CredentialSource != "tenant" || rA.CredentialScopeID != "tenant-x" {
		t.Errorf("run A source = %q/%q, want tenant/tenant-x", rA.CredentialSource, rA.CredentialScopeID)
	}
	if rA.Provider != "scripted" || rA.Model != "stub-model" {
		t.Errorf("run A provider/model = %q/%q, want scripted/stub-model", rA.Provider, rA.Model)
	}
	// Cost = (1000*3 + 500*15)/1e6 = 0.0105.
	if rA.CostCurrency != "USD" || rA.Cost < 0.0104 || rA.Cost > 0.0106 {
		t.Errorf("run A cost = %v %q, want ~0.0105 USD", rA.Cost, rA.CostCurrency)
	}
	// Per-run summary: source + a non-NULL cost, and the rollup matches.
	if runA.CredentialSource != "tenant" {
		t.Errorf("run A summary source = %q, want tenant", runA.CredentialSource)
	}
	if runA.Cost == nil {
		t.Errorf("run A summary cost is NULL, want priced")
	}
	if runA.InputTokens != rA.InputTokens {
		t.Errorf("rollup: runs.InputTokens=%d Σledger=%d", runA.InputTokens, rA.InputTokens)
	}

	// --- Run B: operator key paid (empty source ⇒ "operator"). ---
	runB, err := st.GetRunByAgentID(ctx, "solo-operator")
	if err != nil {
		t.Fatalf("GetRunByAgentID(solo-operator): %v", err)
	}
	rowsB, _ := st.TokenUsageForRun(ctx, runB.ID)
	if len(rowsB) != 1 || rowsB[0].CredentialSource != "operator" {
		t.Fatalf("run B rows=%d source=%v, want 1 operator", len(rowsB), rowsB)
	}
	if runB.CredentialSource != "operator" {
		t.Errorf("run B summary source = %q, want operator", runB.CredentialSource)
	}
}

// TestUsageAttribution_LedgerRowCarriesRunIdentity is the fix-1 regression: a
// top-level run's per-call token_usage row must be attributed to the run's TRUE
// identity — tenant/user/agent + session — not the zero value. makeRecordingEmit
// was constructed BEFORE tools.WithRunIdentity stamped the loop ctx, so
// recordCallUsage read tools.RunIdentity(ctx) == zero and every directly-invoked
// run's ledger rows landed with tenant_id/user_id/agent_id all "" and session_id
// unset. Fails on the pre-fix code (all four fields empty).
func TestUsageAttribution_LedgerRowCarriesRunIdentity(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"solo": {Model: "stub-model", SystemPrompt: "you are solo"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
		Pricing: config.PricingConfig{
			Currency: "USD",
			Models:   map[string]config.ModelPrice{"scripted/stub-model": {Input: 3, Output: 15}},
		},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{
					InputTokens: 1000, OutputTokens: 500, Model: "stub-model"}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Open mode (AuthToken=="") passes wire tenant/user/agent_id through verbatim.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"solo","agent_id":"agent-77","user_id":"u-alice","tenant_id":"acme","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	_, _ = io.ReadAll(resp.Body) // drain to completion (recordCallUsage runs in-path)

	ctx := context.Background()
	run, err := st.GetRunByAgentID(ctx, "agent-77")
	if err != nil {
		t.Fatalf("GetRunByAgentID(agent-77): %v", err)
	}
	rows, err := st.TokenUsageForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.TenantID != "acme" {
		t.Errorf("row TenantID = %q, want acme", r.TenantID)
	}
	if r.UserID != "u-alice" {
		t.Errorf("row UserID = %q, want u-alice", r.UserID)
	}
	if r.AgentID != "agent-77" {
		t.Errorf("row AgentID = %q, want agent-77", r.AgentID)
	}
	if r.SessionID == "" || r.SessionID != run.SessionID {
		t.Errorf("row SessionID = %q, want run's session %q", r.SessionID, run.SessionID)
	}
}

// noopUsageTool lets a scripted run take a SECOND model iteration (tool_use →
// tool_result → model call) so the loop makes two priced calls in ONE run —
// required by the fix-9 regression to exercise a mid-run model change.
type noopUsageTool struct{}

func (noopUsageTool) Name() string                 { return "Noop" }
func (noopUsageTool) Description() string          { return "does nothing" }
func (noopUsageTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (noopUsageTool) Execute(_ context.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "ok"}, nil
}

// TestUsageAttribution_RunCostIsSumOfLedgerNotFinalModel is the fix-9 regression:
// runs.cost must equal Σ(the run's per-call token_usage costs), NOT the final
// model priced over cumulative tokens. finishRun used to price res.Usage (final
// model × summed tokens); on a run that changed model mid-flight (fallback) that
// disagreed with the ledger, which prices each call at its OWN model. Here call 0
// runs on an expensive model and call 1 on a cheap one, so the two pricings differ
// sharply. Fails on the pre-fix code (runs.cost ≈ 0.002, not Σ(ledger) ≈ 0.101).
func TestUsageAttribution_RunCostIsSumOfLedgerNotFinalModel(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "model-a"},
		Agents: map[string]config.AgentDef{
			"solo": {Model: "model-a", Tools: []string{"Noop"}, SystemPrompt: "you are solo"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
		// model-a is 100× the price of model-b, so summing per-call (ledger) vs
		// pricing the final model over cumulative tokens give very different numbers.
		Pricing: config.PricingConfig{
			Currency: "USD",
			Models: map[string]config.ModelPrice{
				"scripted/model-a": {Input: 100, Output: 100}, // expensive (served call 0)
				"scripted/model-b": {Input: 1, Output: 1},     // cheap (served call 1, the final)
			},
		},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// Call 0: model-a, 1000 input tokens, tool_call(Noop) → continue.
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu1", Name: "Noop", Input: json.RawMessage(`{}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1000, OutputTokens: 0, Model: "model-a"}},
			},
			// Call 1: model-b (the FINAL model), 1000 output tokens, end_turn.
			{
				{Type: providers.EventText, Text: "done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 0, OutputTokens: 1000, Model: "model-b"}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{noopUsageTool{}}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"solo","agent_id":"fallback-run","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	_, _ = io.ReadAll(resp.Body) // drain to completion (finishRun runs in-path)

	ctx := context.Background()
	run, err := st.GetRunByAgentID(ctx, "fallback-run")
	if err != nil {
		t.Fatalf("GetRunByAgentID: %v", err)
	}
	rows, err := st.TokenUsageForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2 (one per call)", len(rows))
	}
	var ledgerSum float64
	for _, r := range rows {
		ledgerSum += r.Cost
	}
	// Σ(ledger) = model-a(1000 in × 100/1e6 = 0.1) + model-b(1000 out × 1/1e6 = 0.001) = 0.101.
	if run.Cost == nil {
		t.Fatalf("runs.cost is NULL, want the priced Σ(ledger)")
	}
	if diff := *run.Cost - ledgerSum; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("runs.cost = %v, want Σ(ledger) = %v", *run.Cost, ledgerSum)
	}
	// Guard explicitly against the pre-fix value: pricing the final model (model-b)
	// over cumulative tokens (2000) = 0.002 — nowhere near Σ(ledger) ≈ 0.101.
	if *run.Cost < 0.05 {
		t.Errorf("runs.cost = %v looks like final-model×cumulative pricing (~0.002), not Σ(ledger) (~0.101)", *run.Cost)
	}
}
