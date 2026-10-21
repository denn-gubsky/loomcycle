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
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{"*"} })

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

// retention travels like the override lock: it survives create and fork, a
// fork can add it and cannot undo it, the config's holds over a tenant's
// version, and a create that changes only it still writes a version.
func TestAgentDefTool_RetentionSurvivesCreateAndForkAndCannotBeLoosened(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	retentionOf := func(tenant, name string) string {
		t.Helper()
		def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, tenant, name)
		if !ok {
			t.Fatalf("%s did not resolve", name)
		}
		return def.Retention
	}
	exec := func(input string) tools.Result {
		t.Helper()
		res, _ := tool.Execute(ctx, json.RawMessage(input))
		return res
	}

	if res := exec(`{"op":"create","name":"quiet","overlay":{"system_prompt":"q","retention":"none"}}`); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	if got := retentionOf("", "quiet"); got != "none" {
		t.Fatalf("after create: retention = %q — the mode did not survive the overlay", got)
	}
	if res := exec(`{"op":"fork","name":"quiet","promote":true,"overlay":{"system_prompt":"q2"}}`); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := retentionOf("", "quiet"); got != "none" {
		t.Errorf("after a fork that did not send it: retention = %q, want the parent's", got)
	}
	if res := exec(`{"op":"fork","name":"quiet","promote":true,"overlay":{"retention":"default"}}`); !res.IsError || !strings.Contains(res.Text, "retention") {
		t.Errorf("a fork that asked for the default on a no-trace parent: %s, want it refused", res.Text)
	}
	if res := exec(`{"op":"create","name":"bad","overlay":{"system_prompt":"b","retention":"nonee"}}`); !res.IsError {
		t.Errorf("a misspelled retention was accepted: %s", res.Text)
	}
	// "default" on an agent that is not no-trace is simply the default.
	if res := exec(`{"op":"create","name":"plain","overlay":{"system_prompt":"p","retention":"default"}}`); res.IsError {
		t.Fatalf("create with the default: %s", res.Text)
	}
	if got := retentionOf("", "plain"); got != "" {
		t.Errorf("retention %q stored for the default", got)
	}
	// A create that changes only the mode is not answered with the old row.
	res := exec(`{"op":"create","name":"plain","promote":true,"overlay":{"system_prompt":"p","retention":"none"}}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out["deduplicated"] == true {
		t.Errorf("a create that adds the mode was deduplicated: %s", res.Text)
	}
	if got := retentionOf("", "plain"); got != "none" {
		t.Errorf("active def retention = %q, want none", got)
	}
}

func TestRetention_TheConfigModeHoldsOverATenantsVersionAndRoundTrips(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithRunIdentity(operatorPlane(ctx), tools.RunIdentityValue{TenantID: "acme", AgentID: "a_author"})
	static, name := firstStaticAgent(t, tool.Cfg)
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","promote":true,"overlay":{"system_prompt":"tenant version"}}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	static.Retention = "none"
	tool.Cfg.Agents[name] = static
	if d, _ := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "acme", name); d.SystemPrompt != "tenant version" || d.Retention != "none" {
		t.Errorf("the tenant's version resolves without the config's mode: %q", d.Retention)
	}
	if d, _, _ := lookup.AgentChecked(context.Background(), tool.Store, tool.Cfg, "acme", name); d.Retention != "none" {
		t.Errorf("AgentChecked: retention %q", d.Retention)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","promote":true,"overlay":{"retention":"default"}}`))
	if !res.IsError {
		t.Errorf("a fork loosened a config-level mode: %s", res.Text)
	}

	if md := staticToMergedDef(config.AgentDef{Retention: "none"}); md.Retention != "none" {
		t.Errorf("staticToMergedDef dropped the mode: %q", md.Retention)
	}
	if back := lookup.SubstrateAgentDefFromConfig(config.AgentDef{Retention: "none"}).ToConfigDef(); back.Retention != "none" {
		t.Errorf("the read adapter dropped the mode: %q", back.Retention)
	}
}
