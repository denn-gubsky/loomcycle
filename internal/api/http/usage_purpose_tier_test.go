package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/mock"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestUsage_RowCarriesTierAndUserTier — a run's own turn records the model
// tier its agent routes by and the run's user tier; a side call made for the
// same run keeps the user tier, since it was spent for that user, but not the
// model tier, since the tier did not choose the model that served it.
func TestUsage_RowCarriesTierAndUserTier(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := context.Background()
	rid := tools.RunIdentityValue{TenantID: "t1", UserID: "u1", ModelTier: "middle", UserTier: "pro"}
	u := &providers.Usage{InputTokens: 10, Provider: "p", Model: "m"}
	_ = srv.recordCallUsage(ctx, "run-tier", rid, "s1", 0, u, store.UsagePurposeChat)
	_ = srv.recordCallUsage(ctx, "run-tier", rid, "s1", 1, u, store.UsagePurposeRerank)

	rows, err := st.TokenUsageForRun(ctx, "run-tier")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v; want 2", rows, err)
	}
	if r := rows[0]; r.Purpose != store.UsagePurposeChat || r.Tier != "middle" || r.UserTier != "pro" {
		t.Errorf("turn row = (purpose %q, tier %q, user tier %q), want (chat, middle, pro)", r.Purpose, r.Tier, r.UserTier)
	}
	if r := rows[1]; r.Purpose != store.UsagePurposeRerank || r.Tier != "" || r.UserTier != "pro" {
		t.Errorf("rerank row = (purpose %q, tier %q, user tier %q), want (rerank, \"\", pro)", r.Purpose, r.Tier, r.UserTier)
	}
}

// TestUsage_InternalAgentRunIsConsolidation — the turns of an `internal:`
// agent are the runtime's own maintenance and are charged as consolidation.
func TestUsage_InternalAgentRunIsConsolidation(t *testing.T) {
	if got := runCallPurpose(tools.RunIdentityValue{InternalAgent: true}); got != store.UsagePurposeConsolidation {
		t.Errorf("internal agent's turns = %q, want consolidation", got)
	}
	if got := runCallPurpose(tools.RunIdentityValue{}); got != store.UsagePurposeChat {
		t.Errorf("an agent's turns = %q, want chat", got)
	}
}

// TestUsage_SummariesAreBilled — a compaction summary and a recap emit no
// usage event, so they used to be measured and never charged. Through the
// run's call observer each is now a ledger row of its own purpose, charged to
// the run, with the run's user tier and no model tier.
func TestUsage_SummariesAreBilled(t *testing.T) {
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_MS", "0")
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS", "0")
	srv, st := newUsageTestServer(t)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		TenantID: "t1", UserID: "u1", SessionID: "s1", UserTier: "pro", ModelTier: "middle"})
	ctx = providers.WithCallObserver(ctx, srv.callObserver("run-sum"))
	msgs := []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "a long conversation"}}}}
	if _, err := loop.Summarize(ctx, mock.New(), "mock-generic", msgs, 30); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if _, err := loop.Recap(ctx, mock.New(), "mock-generic", msgs); err != nil {
		t.Fatalf("Recap: %v", err)
	}

	rows, err := st.TokenUsageForRun(context.Background(), "run-sum")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v; want a compaction row and a recap row", rows, err)
	}
	for i, want := range []string{store.UsagePurposeCompaction, store.UsagePurposeRecap} {
		r := rows[i]
		if r.Purpose != want || r.TenantID != "t1" || r.UserID != "u1" || r.UserTier != "pro" || r.Tier != "" {
			t.Errorf("row %d = (purpose %q, tenant %q, user %q, user tier %q, tier %q), want (%s, t1, u1, pro, \"\")",
				i, r.Purpose, r.TenantID, r.UserID, r.UserTier, r.Tier, want)
		}
		if r.InputTokens == 0 || r.Provider != "mock" {
			t.Errorf("row %d = %+v, want the mock provider's tokens", i, r)
		}
	}
}

// TestUsage_ObservedCallWithoutPurposeIsNotBilled — a row must say what it
// was for, so an observed call whose ctx names no purpose is measured only.
func TestUsage_ObservedCallWithoutPurposeIsNotBilled(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t1", UserID: "u1"})
	srv.callObserver("run-np")(ctx, &providers.Usage{InputTokens: 10, Provider: "p", Model: "m"})
	if rows, err := st.TokenUsageForRun(context.Background(), "run-np"); err != nil || len(rows) != 0 {
		t.Errorf("rows = %+v, %v; want none", rows, err)
	}
}

// TestUsage_BackgroundCallIsBilledToTheScopeItWorkedOn — unit generation runs
// from an operator route with no run behind it; it is charged to the tenant and
// user whose documents it worked on (the identity the route puts on ctx).
func TestUsage_BackgroundCallIsBilledToTheScopeItWorkedOn(t *testing.T) {
	srv, st := newUsageTestServer(t)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t-doc", UserID: "u-owner"})
	srv.BackgroundCallObserver(store.UsagePurposeUnitGeneration)(ctx, &providers.Usage{InputTokens: 40, OutputTokens: 8, Provider: "p", Model: "m"})

	rep, err := st.UsageReport(context.Background(), store.UsageQuery{TenantID: "t-doc",
		GroupBy: []store.UsageDimension{store.UsageByUser, store.UsageByPurpose}})
	if err != nil || len(rep) != 1 {
		t.Fatalf("report = %+v, %v; want one row", rep, err)
	}
	if r := rep[0]; r.UserID != "u-owner" || r.Purpose != store.UsagePurposeUnitGeneration || r.InputTokens != 40 || r.CallCount != 1 {
		t.Errorf("row = %+v, want u-owner / unit_generation / 40 tokens / 1 call", r)
	}
}

// TestUsage_LoopSummaryPurposesAreLedgerPurposes — the loop names its summary
// purposes without importing the store; the ledger must accept them, as
// background calls.
func TestUsage_LoopSummaryPurposesAreLedgerPurposes(t *testing.T) {
	for _, p := range []string{loop.PurposeCompaction, loop.PurposeRecap} {
		if c, ok := store.UsagePurposeClass(p); !ok || c != store.UsageClassBackground {
			t.Errorf("purpose %q: class %q, known %v; want a background purpose", p, c, ok)
		}
	}
}

func TestUsageModelTier(t *testing.T) {
	for _, tc := range []struct {
		def  config.AgentDef
		want string
	}{
		{config.AgentDef{Tier: "middle"}, "middle"},
		{config.AgentDef{Model: "m"}, store.UsageTierPinned},
		{config.AgentDef{Provider: "p", Model: "m"}, store.UsageTierPinned},
		// A row stored with both is resolved to the pin, so it is recorded as one.
		{config.AgentDef{Tier: "high", Model: "m"}, store.UsageTierPinned},
		{config.AgentDef{}, ""},
	} {
		if got := usageModelTier(tc.def); got != tc.want {
			t.Errorf("usageModelTier(%+v) = %q, want %q", tc.def, got, tc.want)
		}
	}
}

func TestGatewayUsageTier(t *testing.T) {
	for _, tc := range []struct {
		req  llmChatRequest
		want string
	}{
		{llmChatRequest{Provider: "p", Model: "m"}, store.UsageTierPinned},
		{llmChatRequest{Model: "m"}, store.UsageTierPinned},
		{llmChatRequest{Provider: "p", Tier: "high"}, "high"},
		{llmChatRequest{Tier: "low"}, "low"},
		{llmChatRequest{}, "default"},
	} {
		if got := gatewayUsageTier(&tc.req); got != tc.want {
			t.Errorf("gatewayUsageTier(%+v) = %q, want %q", tc.req, got, tc.want)
		}
	}
}

// TestGateway_CallsAreRecordedAsGateway — a gateway call is foreground spend
// with no run, recorded under its own purpose and, as it names its model, as
// pinned.
func TestGateway_CallsAreRecordedAsGateway(t *testing.T) {
	ts, srv := gatewayBudgetServer(t, 1_000_000, 0)
	for path, req := range gatewayBodies {
		if code, body := bearerCall(t, ts, "tok-alice", "POST", path, req); code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, code, body)
		}
	}
	rows, err := srv.store.TokenUsageForRun(context.Background(), "")
	if err != nil || len(rows) != len(gatewayBodies) {
		t.Fatalf("gateway rows = %+v, %v; want %d", rows, err, len(gatewayBodies))
	}
	for _, r := range rows {
		if r.Purpose != store.UsagePurposeGateway || r.Tier != store.UsageTierPinned {
			t.Errorf("gateway row = (purpose %q, tier %q), want (gateway, pinned)", r.Purpose, r.Tier)
		}
	}
}

// TestUsage_RunsRecordTheirAgentsTierAndPurpose — end to end through POST
// /v1/runs: a pinned agent's turns are recorded as pinned chat, an internal
// agent's as consolidation.
func TestUsage_RunsRecordTheirAgentsTierAndPurpose(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"solo":   {Model: "stub-model", SystemPrompt: "you are solo"},
			"keeper": {Model: "stub-model", SystemPrompt: "you keep memory", Internal: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	done := []providers.Event{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 5, Model: "stub-model"}},
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{done, done}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	ctx := context.Background()
	for _, tc := range []struct{ agent, agentID, purpose string }{
		{"solo", "a-solo", store.UsagePurposeChat},
		{"keeper", "a-keeper", store.UsagePurposeConsolidation},
	} {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"`+tc.agent+`","agent_id":"`+tc.agentID+`","user_id":"u1","tenant_id":"acme","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
		if err != nil {
			t.Fatalf("post %s: %v", tc.agent, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", tc.agent, resp.StatusCode)
		}
		run, err := st.GetRunByAgentID(ctx, tc.agentID)
		if err != nil {
			t.Fatalf("GetRunByAgentID(%s): %v", tc.agentID, err)
		}
		rows, err := st.TokenUsageForRun(ctx, run.ID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s rows = %+v, %v; want 1", tc.agent, rows, err)
		}
		if r := rows[0]; r.Purpose != tc.purpose || r.Tier != store.UsageTierPinned {
			t.Errorf("%s row = (purpose %q, tier %q), want (%s, pinned)", tc.agent, r.Purpose, r.Tier, tc.purpose)
		}
	}
}
