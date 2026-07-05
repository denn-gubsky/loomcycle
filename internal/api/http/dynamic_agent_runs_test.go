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
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestDynamicAgent_RegisteredThenRun is the regression for the v0.8.15
// bug surfaced by the bench harness (2026-05-14): RegisterAgent
// persisted to dynamic_agents successfully, but the subsequent
// /v1/runs against the dynamic agent name returned "unknown agent".
//
// Root cause: the dynamic-agent fallback in (*Server).RunOnce filled
// the local agentDef variable, but the next line called
// resolveAgent(name, …) which ONLY consults s.cfg.Agents and emits
// "unknown agent" again. The fix is to call resolveAgentDef(agentDef,
// name, …) instead — the same path the v0.8.5 sub-agent overlay uses.
//
// Without the fix this test fails with "unknown agent" in the SSE
// error frame.
func TestDynamicAgent_RegisteredThenRun(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "hello-dynamic"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "dyn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Step 1 — register the dynamic agent. This is what the bench
	// (and any orchestrator using mcp__loomcycle__register_agent)
	// does at sweep start.
	if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
		Name:         "bench-dynamic-fixture",
		SystemPrompt: "be brief",
		Tools:        []string{"Read"},
		Provider:     "stub",
		Model:        "stub-model",
		Description:  "regression fixture",
		TTLSeconds:   600,
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	// Step 2 — spawn a run against the dynamic agent name. Pre-fix,
	// this surfaces "unknown agent: bench-dynamic-fixture" in the
	// SSE error stream because resolveAgent doesn't consult
	// dynamic_agents.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"bench-dynamic-fixture","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if strings.Contains(bodyStr, "unknown agent") {
		t.Errorf("v0.8.15 regression: dynamic agent flagged as unknown by run path\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "hello-dynamic") {
		t.Errorf("expected stub provider's text in stream; got:\n%s", bodyStr)
	}
}
