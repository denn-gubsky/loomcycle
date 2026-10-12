package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

	// A fork cannot remove it: forking a locked agent would otherwise be the
	// way to run it with other settings.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"judge","promote":true,"overlay":{"allow_overrides":true}}`))
	if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
		t.Errorf("a fork that sent true on a locked parent: %s, want it refused", res.Text)
	}
	if got := allowOverridesOf(t, tool, "judge"); got == nil || *got {
		t.Errorf("after the refused fork: allow_overrides = %v, want false", got)
	}
	// A fork of an open agent may add it.
	run("create open", `{"op":"create","name":"scorer","overlay":{"system_prompt":"score"}}`)
	run("fork lock", `{"op":"fork","name":"scorer","promote":true,"overlay":{"allow_overrides":false}}`)
	if got := allowOverridesOf(t, tool, "scorer"); got == nil || *got {
		t.Errorf("after a fork that added the lock: allow_overrides = %v, want false", got)
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

// The operator's lock, written in the config, holds over a tenant's own
// version of the agent: one forked before the lock was added, and one that
// tries to open it.
func TestAllowOverrides_TheConfigLockHoldsOverATenantsVersion(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithRunIdentity(operatorPlane(ctx), tools.RunIdentityValue{TenantID: "acme", AgentID: "a_author"})
	static, name := firstStaticAgent(t, tool.Cfg)

	// Forked while the config left the agent open.
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","promote":true,"overlay":{"system_prompt":"tenant version"}}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "acme", name)
	if !ok || def.SystemPrompt != "tenant version" || def.AllowOverrides != nil {
		t.Fatalf("the tenant version is not what resolves, or is locked before the config says so: ok=%v %+v", ok, def.AllowOverrides)
	}

	// The operator locks the agent in the config.
	locked := false
	static.AllowOverrides = &locked
	tool.Cfg.Agents[name] = static
	for what, resolve := range map[string]func() config.AgentDef{
		"Agent": func() config.AgentDef {
			d, _ := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "acme", name)
			return d
		},
		"AgentChecked": func() config.AgentDef {
			d, _, _ := lookup.AgentChecked(context.Background(), tool.Store, tool.Cfg, "acme", name)
			return d
		},
	} {
		if d := resolve(); d.SystemPrompt != "tenant version" || d.AllowOverrides == nil || *d.AllowOverrides {
			t.Errorf("%s: the tenant's version resolves without the config's lock: %v", what, d.AllowOverrides)
		}
	}

	// And its open parent version does not let a fork switch the lock off.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","promote":true,"overlay":{"allow_overrides":true}}`))
	if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
		t.Errorf("a fork that sent true under a config lock: %s, want it refused", res.Text)
	}
}

func firstStaticAgent(t *testing.T, cfg *config.Config) (config.AgentDef, string) {
	t.Helper()
	for name, def := range cfg.Agents {
		return def, name
	}
	t.Fatal("the fixture declares no static agent")
	return config.AgentDef{}, ""
}
