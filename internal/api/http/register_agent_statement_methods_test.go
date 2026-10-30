package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A code agent registered over MCP (register_agent) keeps its
// statement_methods through to its runs, and a model agent declaring it, or a
// list holding an unknown method, is refused rather than registered without
// it. The request is decoded from the JSON the MCP handler receives, so a
// field the request type does not carry is dropped here exactly as it is on
// the wire.
func TestRegisterAgent_StatementMethodsReachTheCodeAgentsRuns(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "dyn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: &stubProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	register := func(args string) error {
		var req connector.RegisterAgentRequest
		if err := json.Unmarshal([]byte(args), &req); err != nil {
			t.Fatalf("decode %s: %v", args, err)
		}
		_, err := srv.RegisterAgent(ctx, req)
		return err
	}

	if err := register(`{"name":"linker","system_prompt":"code","tools":["Document"],"provider":"code-js","statement_methods":["rule","extractor"],"ttl_seconds":600}`); err != nil {
		t.Fatalf("register a code-js agent with statement_methods: %v", err)
	}
	def, ok := lookup.Agent(ctx, st, cfg, "", "linker")
	if !ok {
		t.Fatal("linker not found after RegisterAgent")
	}
	if got := strings.Join(def.StatementMethods, ","); got != "rule,extractor" {
		t.Errorf("stored StatementMethods = %q, want rule,extractor", got)
	}
	policy, _ := srv.agentRunPolicy(ctx, def, "linker", runPolicySource{})
	if got := strings.Join(policy.StatementMethods, ","); got != "rule,extractor" {
		t.Errorf("the registered agent's run holds %q, want rule,extractor", got)
	}

	for name, args := range map[string]string{
		"a model agent": `{"name":"writer","system_prompt":"p","tools":["Document"],"provider":"stub","model":"stub-model","statement_methods":["rule"]}`,
		"a tier agent":  `{"name":"tiered","system_prompt":"p","tools":["Document"],"tier":"middle","statement_methods":["extractor"]}`,
		"unknown":       `{"name":"odd","system_prompt":"code","tools":["Document"],"provider":"code-js","statement_methods":["manual"]}`,
	} {
		if err := register(args); err == nil || !strings.Contains(err.Error(), "statement_methods") {
			t.Errorf("%s: err = %v, want a statement_methods refusal", name, err)
		}
	}
	for _, name := range []string{"writer", "tiered", "odd"} {
		if _, ok := lookup.Agent(ctx, st, cfg, "", name); ok {
			t.Errorf("%s was registered despite the refusal", name)
		}
	}
}
