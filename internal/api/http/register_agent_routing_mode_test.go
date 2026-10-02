package http

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

func newRegisterAgentFixture(t *testing.T) (*Server, *config.Config, *storesqlite.Store) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "dyn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(cfg, &stubResolver{p: &stubProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st), cfg, st
}

// A registration naming a pin AND a tier is refused with the operator-yaml
// words, and nothing is stored: a row with both resolves to the pin, so the
// tier the caller asked for would never be in force.
func TestRegisterAgent_RefusesPinAndTierTogether(t *testing.T) {
	want := config.ValidateRoutingMode("p", "m", "t")
	if want == nil {
		t.Fatal("config.ValidateRoutingMode accepted a pin and a tier; the shared rule changed")
	}
	for _, tc := range []struct {
		label, provider, model string
	}{
		{"provider+model+tier", "stub", "stub-model"},
		{"provider+tier", "stub", ""},
		{"model+tier", "", "stub-model"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			srv, cfg, st := newRegisterAgentFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
				Name:         "both-dyn",
				SystemPrompt: "p",
				Tools:        []string{"Memory"},
				Provider:     tc.provider,
				Model:        tc.model,
				Tier:         "middle",
			})
			if err == nil || err.Error() != want.Error() {
				t.Fatalf("RegisterAgent err = %v, want %q", err, want)
			}
			if _, ok := lookup.Agent(ctx, st, cfg, "", "both-dyn"); ok {
				t.Fatal("a refused registration was stored and resolves")
			}
		})
	}
}

// The rule refuses only the pair: a pin alone and a tier alone still register.
func TestRegisterAgent_PinOrTierAloneStillRegisters(t *testing.T) {
	for _, tc := range []struct {
		label                 string
		provider, model, tier string
	}{
		{"pin only", "stub", "stub-model", ""},
		{"tier only", "", "", "middle"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			srv, cfg, st := newRegisterAgentFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
				Name:         "one-mode-dyn",
				SystemPrompt: "p",
				Tools:        []string{"Memory"},
				Provider:     tc.provider,
				Model:        tc.model,
				Tier:         tc.tier,
			}); err != nil {
				t.Fatalf("RegisterAgent: %v", err)
			}
			def, ok := lookup.Agent(ctx, st, cfg, "", "one-mode-dyn")
			if !ok {
				t.Fatal("registered agent does not resolve")
			}
			if def.Provider != tc.provider || def.Model != tc.model || def.Tier != tc.tier {
				t.Errorf("stored routing = %q/%q tier %q, want %q/%q tier %q",
					def.Provider, def.Model, def.Tier, tc.provider, tc.model, tc.tier)
			}
		})
	}
}

// Over MCP the refusal is a tool error carrying the yaml words, not a
// JSON-RPC failure — the caller can read it and resend with one mode.
func TestMCPRegisterAgent_PinAndTierIsToolError(t *testing.T) {
	srv, cfg, st := newRegisterAgentFixture(t)
	mcpSrv := lcmcp.New(lcmcp.Config{Connector: srv, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_agent","arguments":{"name":"both-dyn","system_prompt":"p","tools":["Memory"],"provider":"stub","model":"stub-model","tier":"middle"}}}` + "\n"
	var stdout bytes.Buffer
	if err := mcpSrv.Serve(ctx, strings.NewReader(in), &stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp loommcp.Response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", stdout.String(), err)
	}
	if resp.Error != nil {
		t.Fatalf("got a JSON-RPC error %+v, want a tool error", resp.Error)
	}
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	want := config.ValidateRoutingMode("stub", "stub-model", "middle").Error()
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, want) {
		t.Fatalf("tool result = %+v, want an error containing %q", res, want)
	}
	if _, ok := lookup.Agent(ctx, st, cfg, "", "both-dyn"); ok {
		t.Fatal("a refused registration was stored and resolves")
	}
}
