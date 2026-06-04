package http

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type namedTool struct{ name string }

func (n namedTool) Name() string                 { return n.name }
func (n namedTool) Description() string          { return "fake" }
func (n namedTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n namedTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{}, nil
}

// TestCandidateTools_AdvertisesDynamicTools pins post-boot tool advertising:
// a substrate-registered tool returned by the dynamic enumerator is folded
// into the per-run candidate set, so the allowed_tools filter advertises it
// to the model without a restart — but only when the agent allows it.
func TestCandidateTools_AdvertisesDynamicTools(t *testing.T) {
	// New auto-appends the built-in Agent tool, so the boot set is N≥1.
	srv := New(&config.Config{}, &stubResolver{}, []tools.Tool{namedTool{"Read"}}, concurrency.New(4, 4, time.Second), nil)
	base := len(srv.candidateTools(context.Background(), ""))

	// Enumerator returns a post-boot dynamic MCP tool → exactly one more.
	srv.SetDynamicToolEnumerator(func(context.Context, string) []tools.Tool {
		return []tools.Tool{namedTool{"mcp__jobs__getAgentContext"}}
	})
	cand := srv.candidateTools(context.Background(), "")
	if len(cand) != base+1 {
		t.Fatalf("with enumerator: %d candidate tools, want base+1 (%d)", len(cand), base+1)
	}

	// Advertised when the agent's allowed_tools permits it.
	if got := toolNames(filterTools(cand, []string{"mcp__jobs__getAgentContext"}, nil)); len(got) != 1 || got[0] != "mcp__jobs__getAgentContext" {
		t.Errorf("dynamic tool not advertised when allowed; got %v", got)
	}
	// NOT advertised when the agent doesn't allow it — the allowlist still gates.
	for _, n := range toolNames(filterTools(cand, []string{"Read"}, nil)) {
		if n == "mcp__jobs__getAgentContext" {
			t.Error("dynamic tool advertised despite not being in allowed_tools")
		}
	}
}
