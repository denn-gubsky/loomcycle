package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// F14: an agent authored via `agentdef create` with the full capability set
// (channels / evaluation_scopes / max_iterations / interruption) must
// round-trip the whole config through the substrate persist → resolve path —
// not just tools. Before the mergedDef + SubstrateAgentDef additions,
// these fields were silently dropped at persist/read, so an MCP-authored agent
// could never be a complete interactive/multi-agent agent.
func TestAgentDefTool_CreateWithCapabilityFields_RoundTrips(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	// Operator ceiling (the F11 fix provides this on the MCP/HTTP/gRPC paths).
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	overlay := `{"op":"create","name":"complete-agent","promote":true,"overlay":{
		"system_prompt":"coordinate the loop",
		"tools":["Memory","Channel","Evaluation","Interruption"],
		"memory_scopes":["user"],
		"memory_consolidation":true,
		"evaluation_scopes":["submit_self","read_any"],
		"max_iterations":42,
		"channels":{"publish":["findings"],"subscribe":["tasks"]},
		"interruption":{"enabled":true,"kinds":["question"],"max_pending":3}
	}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(overlay)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "complete-agent")
	if !ok {
		t.Fatal("resolve: complete-agent not found after create+promote")
	}
	if def.MaxIterations != 42 {
		t.Errorf("MaxIterations = %d, want 42", def.MaxIterations)
	}
	if got := def.EvaluationScopes; len(got) != 2 || got[0] != "submit_self" || got[1] != "read_any" {
		t.Errorf("EvaluationScopes = %v, want [submit_self read_any]", got)
	}
	if pub, sub := def.Channels.Publish, def.Channels.Subscribe; len(pub) != 1 || pub[0] != "findings" || len(sub) != 1 || sub[0] != "tasks" {
		t.Errorf("Channels = %+v, want publish=[findings] subscribe=[tasks]", def.Channels)
	}
	if i := def.Interruption; !i.Enabled || i.MaxPending != 3 || len(i.Kinds) != 1 || i.Kinds[0] != "question" {
		t.Errorf("Interruption = %+v, want {enabled:true kinds:[question] max_pending:3}", def.Interruption)
	}
	// RFC BL P2: the consolidation grant must survive overlay → persist → resolve.
	if !def.MemoryConsolidation {
		t.Error("MemoryConsolidation grant did not round-trip through create+promote+resolve")
	}
}
