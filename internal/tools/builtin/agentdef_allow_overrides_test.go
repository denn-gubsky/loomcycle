package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// allow_overrides is a lock, so the failure that matters is the quiet one: a
// definition that was written locked and reads back open. Every way the field
// travels is checked for that here.

func allowOverridesOf(t *testing.T, tool *AgentDef, name string) *bool {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("%s did not resolve", name)
	}
	return def.AllowOverrides
}

func TestAgentDefTool_AllowOverridesSurvivesCreateAndFork(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	run := func(what, input string) {
		t.Helper()
		if res, _ := tool.Execute(ctx, json.RawMessage(input)); res.IsError {
			t.Fatalf("%s: %s", what, res.Text)
		}
	}

	run("create", `{"op":"create","name":"judge","overlay":{"system_prompt":"judge","allow_overrides":false}}`)
	if got := allowOverridesOf(t, tool, "judge"); got == nil || *got {
		t.Fatalf("after create: allow_overrides = %v, want false — the lock did not survive the overlay", got)
	}

	// A fork that does not mention it keeps its parent's.
	run("fork", `{"op":"fork","name":"judge","promote":true,"overlay":{"system_prompt":"judge, revised"}}`)
	if got := allowOverridesOf(t, tool, "judge"); got == nil || *got {
		t.Errorf("after a fork that did not send it: allow_overrides = %v, want the parent's false", got)
	}

	// A fork that sends true opens it; that is the author's to decide.
	run("fork open", `{"op":"fork","name":"judge","promote":true,"overlay":{"allow_overrides":true}}`)
	if got := allowOverridesOf(t, tool, "judge"); got == nil || !*got {
		t.Errorf("after a fork that sent true: allow_overrides = %v, want true", got)
	}
}

// The field is not part of the content hash, and create returns the active
// row when the hash matches. A create that changes only the lock must still
// write it, or the caller is told the agent is locked while it runs open.
func TestAgentDefTool_CreateDoesNotDedupAcrossAChangedOverrideLock(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	base := `{"op":"create","name":"judge","promote":true,"overlay":{"system_prompt":"judge","tools":["Read"]%s}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(base, ""))); res.IsError {
		t.Fatalf("first create: %s", res.Text)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(base, `,"allow_overrides":false`)))
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, res.Text)
	}
	if out["deduplicated"] == true {
		t.Error("create deduplicated although it adds the override lock")
	}
	if got := allowOverridesOf(t, tool, "judge"); got == nil || *got {
		t.Errorf("active def allow_overrides = %v, want false", got)
	}
}

// A static agent's lock is carried into the substrate when it is forked, and
// through the read adapter both ways.
func TestAllowOverrides_StaticAndAdapterRoundTrip(t *testing.T) {
	locked := false
	if md := staticToMergedDef(config.AgentDef{AllowOverrides: &locked}); md.AllowOverrides == nil || *md.AllowOverrides {
		t.Errorf("staticToMergedDef dropped the lock: %v", md.AllowOverrides)
	}
	back := lookup.SubstrateAgentDefFromConfig(config.AgentDef{AllowOverrides: &locked}).ToConfigDef()
	if back.AllowOverrides == nil || *back.AllowOverrides {
		t.Errorf("the read adapter dropped the lock: %v", back.AllowOverrides)
	}
	if lookup.SubstrateAgentDefFromConfig(config.AgentDef{}).ToConfigDef().AllowOverrides != nil {
		t.Error("an agent that never set it reads back as having set it")
	}
}
