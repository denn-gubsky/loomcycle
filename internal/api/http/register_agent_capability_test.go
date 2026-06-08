package http

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// F11/F14: a dynamic agent registered over MCP (RegisterAgent) must carry the
// full capability set — channels / evaluation_scopes / max_iterations /
// interruption — not just allowed_tools, so it can be a complete interactive /
// multi-agent agent. Before the connector + connector_impl additions these
// fields never reached the persisted config.AgentDef.
func TestRegisterAgent_CapabilityFields_RoundTrip(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "dyn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: &stubProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
		Name:             "complete-dyn",
		SystemPrompt:     "coordinate",
		AllowedTools:     []string{"Memory", "Channel", "Evaluation", "Interruption"},
		Provider:         "stub",
		Model:            "stub-model",
		MemoryScopes:     []string{"user"},
		MaxIterations:    42,
		EvaluationScopes: []string{"submit_self", "read_any"},
		Channels:         config.AgentChannelACL{Publish: []string{"findings"}, Subscribe: []string{"tasks"}},
		Interruption:     config.AgentInterruptionACL{Enabled: true, Kinds: []string{"question"}, MaxPending: 3},
		TTLSeconds:       600,
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	def, ok := lookup.Agent(ctx, st, cfg, "", "complete-dyn")
	if !ok {
		t.Fatal("resolve: complete-dyn not found after RegisterAgent")
	}
	if def.MaxIterations != 42 {
		t.Errorf("MaxIterations = %d, want 42", def.MaxIterations)
	}
	if got := def.EvaluationScopes; len(got) != 2 || got[0] != "submit_self" {
		t.Errorf("EvaluationScopes = %v, want [submit_self read_any]", got)
	}
	if pub, sub := def.Channels.Publish, def.Channels.Subscribe; len(pub) != 1 || pub[0] != "findings" || len(sub) != 1 || sub[0] != "tasks" {
		t.Errorf("Channels = %+v, want publish=[findings] subscribe=[tasks]", def.Channels)
	}
	if i := def.Interruption; !i.Enabled || i.MaxPending != 3 || len(i.Kinds) != 1 {
		t.Errorf("Interruption = %+v, want {enabled:true kinds:[question] max_pending:3}", def.Interruption)
	}
}
