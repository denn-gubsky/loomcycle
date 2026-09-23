package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// RFC DI: an AgentDef's tool_choice survives create → persist → lookup, and a
// fork that sets one REPLACES it whole rather than merging a mode from one
// version with a tool name from another.
func TestAgentDefTool_ToolChoiceRoundTripsAndForkReplacesWhole(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"searcher-tc","overlay":{`+
		`"system_prompt":"research","tool_choice":{"mode":"tool","name":"WebSearch","until":"until_called"}}}`)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "searcher-tc")
	if !ok || def.ToolChoice == nil || def.ToolChoice.Name != "WebSearch" || def.ToolChoice.Until != "until_called" {
		t.Fatalf("tool_choice after create = %+v (resolved %v), want the authored choice", def.ToolChoice, ok)
	}

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"searcher-tc","promote":true,`+
		`"overlay":{"tool_choice":{"mode":"required"}}}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	def, _ = lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "searcher-tc")
	if def.ToolChoice == nil || def.ToolChoice.Mode != "required" || def.ToolChoice.Name != "" || def.ToolChoice.Until != "" {
		t.Errorf("tool_choice after fork = %+v, want the fork's choice exactly — no name or until carried over", def.ToolChoice)
	}
}

// Content-identifying: a fork that changes only which tool the model must call
// is a different definition and must mint a different content_sha256.
func TestAgentDefTool_ToolChoiceAffectsContentSHA(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"r","overlay":{"system_prompt":"x"}}`))
	parent, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"r","overlay":{"tool_choice":{"mode":"tool","name":"WebSearch"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	fork, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	if parent == "" || fork == "" || parent == fork {
		t.Errorf("parent %q fork %q — a tool_choice-only fork must change the content hash", parent, fork)
	}
}

// The substrate write path holds an agent-authored def to the rule operator
// yaml is held to: a choice that could never let a run finish is refused.
func TestAgentDefTool_RefusesANeverFinishingToolChoice(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"looper","overlay":{`+
		`"system_prompt":"x","tool_choice":{"mode":"required","until":"always"}}}`))
	if !res.IsError || !strings.Contains(res.Text, "never give a final answer") {
		t.Errorf("create with until=always + required = %q, want a refusal naming why", res.Text)
	}
}

// A static (yaml) agent's tool_choice survives the first fork, which starts
// from staticToMergedDef.
func TestStaticToMergedDef_PreservesToolChoice(t *testing.T) {
	md := staticToMergedDef(config.AgentDef{ToolChoice: &config.ToolChoice{Mode: "none"}})
	if md.ToolChoice == nil || md.ToolChoice.Mode != "none" {
		t.Errorf("staticToMergedDef dropped tool_choice: %+v", md.ToolChoice)
	}
}
