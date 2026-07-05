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
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestResolveAgent_TenantScopedDynamicAgentRunnable is the regression for BUG-1
// (RFC N runtime QA, 2026-06-04): the provider/model resolution gate
// (resolveAgent) re-looked-up the agent at tenant "" (context.Background)
// instead of the run's tenant, so a tenant-scoped DYNAMIC agent was unrunnable
// via POST /v1/runs ("unknown agent") — even though the entry def lookup was
// tenant-correct. Here "probe" exists ONLY as a dynamic agent registered under
// tenant "acme" (no static def). Open mode (no auth token) makes the wire
// tenant_id the effective tenant, so we drive the two tenants directly.
//
// Fail-before: revert resolveAgent's body to look up at "" → the acme run
// returns "unknown agent" and this test fails.
func TestResolveAgent_TenantScopedDynamicAgentRunnable(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{}, // NO static "probe"
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode → wire tenant_id is authoritative

	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "probe ran"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "resolveagent_tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// "probe" registered ONLY under tenant "acme".
	probeDef, _ := json.Marshal(config.AgentDef{Model: "stub-model", Tools: []string{}, SystemPrompt: "probe"})
	if err := st.DynamicAgentUpsert(context.Background(), store.DynamicAgent{
		TenantID: "acme", Name: "probe", Definition: probeDef, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	run := func(tenant string) (int, string) {
		body := `{"agent":"probe","tenant_id":"` + tenant + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post(%s): %v", tenant, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// acme: the tenant that OWNS "probe" → the run executes (no "unknown agent").
	code, out := run("acme")
	if strings.Contains(out, "unknown agent") {
		t.Errorf("BUG-1: tenant-owned dynamic agent reported unknown agent (status=%d):\n%s", code, out)
	}
	if !strings.Contains(out, "probe ran") {
		t.Errorf("acme: expected the probe run to execute (status=%d):\n%s", code, out)
	}

	// globex: must NOT resolve acme's tenant-scoped agent.
	if _, out := run("globex"); !strings.Contains(out, "unknown agent") {
		t.Errorf("globex: expected 'unknown agent' (cross-tenant isolation), got:\n%s", out)
	}

	// shared "" tenant: must NOT resolve acme's tenant-scoped agent.
	if _, out := run(""); !strings.Contains(out, "unknown agent") {
		t.Errorf("shared tenant: expected 'unknown agent', got:\n%s", out)
	}
}
