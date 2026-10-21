package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ephemeral_volumes takes write access away, so the failure that matters is
// the quiet one: a definition written read-only that reads back without it.

func ephemeralVolumesOf(t *testing.T, tool *AgentDef, name string) string {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("%s did not resolve", name)
	}
	return def.EphemeralVolumes
}

func TestAgentDefTool_EphemeralVolumesSurvivesCreateAndForkAndCannotBeCleared(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	run := func(what, input string) {
		t.Helper()
		if res, _ := tool.Execute(ctx, json.RawMessage(input)); res.IsError {
			t.Fatalf("%s: %s", what, res.Text)
		}
	}

	run("create", `{"op":"create","name":"reviewer","overlay":{"system_prompt":"review","ephemeral_volumes":"ro"}}`)
	if got := ephemeralVolumesOf(t, tool, "reviewer"); got != "ro" {
		t.Fatalf("after create: ephemeral_volumes = %q, want ro", got)
	}
	// A fork that does not mention it keeps its parent's.
	run("fork", `{"op":"fork","name":"reviewer","promote":true,"overlay":{"system_prompt":"review, revised"}}`)
	if got := ephemeralVolumesOf(t, tool, "reviewer"); got != "ro" {
		t.Errorf("after a fork that did not send it: %q, want the parent's ro", got)
	}
	// No value clears it: "rw" does not exist, and an empty one says nothing.
	for _, v := range []string{`"rw"`, `"none"`, `"RO"`} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"reviewer","promote":true,"overlay":{"ephemeral_volumes":`+v+`}}`))
		if !res.IsError || !strings.Contains(res.Text, "ephemeral_volumes") {
			t.Errorf("a fork with ephemeral_volumes %s: %s, want it refused", v, res.Text)
		}
	}
	run("fork empty", `{"op":"fork","name":"reviewer","promote":true,"overlay":{"ephemeral_volumes":""}}`)
	if got := ephemeralVolumesOf(t, tool, "reviewer"); got != "ro" {
		t.Errorf("after forks that tried to clear it: %q, want ro", got)
	}
	// A fork of an agent without it may add it.
	run("create open", `{"op":"create","name":"helper","overlay":{"system_prompt":"help"}}`)
	run("fork add", `{"op":"fork","name":"helper","promote":true,"overlay":{"ephemeral_volumes":"ro"}}`)
	if got := ephemeralVolumesOf(t, tool, "helper"); got != "ro" {
		t.Errorf("a fork that added it: %q, want ro", got)
	}
}

// A create that changes only this setting is a new definition: the setting is
// out of the content hash, so the dedup has to compare it on its own.
func TestAgentDefTool_CreateThatOnlyAddsEphemeralVolumesIsNotDeduped(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	for _, in := range []string{
		`{"op":"create","name":"reviewer","overlay":{"system_prompt":"review"}}`,
		`{"op":"create","name":"reviewer","overlay":{"system_prompt":"review","ephemeral_volumes":"ro"}}`,
	} {
		if res, _ := tool.Execute(ctx, json.RawMessage(in)); res.IsError {
			t.Fatal(res.Text)
		}
	}
	if got := ephemeralVolumesOf(t, tool, "reviewer"); got != "ro" {
		t.Errorf("the second create was answered with the first definition: ephemeral_volumes = %q", got)
	}
}

// The operator's setting in the config holds over a tenant's own version of
// the agent. A tenant version shadows the static definition whole, so one
// forked before the operator added the setting would never pick it up.
func TestLookupAgent_StaticEphemeralVolumesHoldsOverAnEarlierTenantVersion(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithRunIdentity(operatorPlane(ctx), tools.RunIdentityValue{TenantID: "acme", AgentID: "a_author"})
	static, name := firstStaticAgent(t, tool.Cfg)

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","promote":true,"overlay":{"system_prompt":"tenant version"}}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "acme", name)
	if !ok || def.SystemPrompt != "tenant version" || def.EphemeralVolumes != "" {
		t.Fatalf("the tenant version is not what resolves, or has the setting before the config does: ok=%v %q", ok, def.EphemeralVolumes)
	}

	// The operator now sets it in the config.
	static.EphemeralVolumes = "ro"
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
		if d := resolve(); d.SystemPrompt != "tenant version" || d.EphemeralVolumes != "ro" {
			t.Errorf("%s: the tenant's version resolves with ephemeral_volumes %q, want the config's ro", what, d.EphemeralVolumes)
		}
	}
}

// The setting is enforced where a volume name is resolved, so every file tool
// gets it, and only for ephemeral volumes: an agent's own bindings keep their
// own mode.
func TestResolveVolume_EphemeralReadOnlyRunCannotWriteAnEphemeralVolume(t *testing.T) {
	bound := realDir(t)
	eph := realDir(t)
	set := tools.NewEphemeralVolumeSet()
	set.Add("scratch", tools.EphemeralVolumeRef{Root: eph}) // created read-write
	ctx := tools.WithEphemeralVolumes(ephemeralWritable(ctxWith(tools.VolumeBinding{Name: "default", Root: bound, Default: true})), set)

	write := func(ctx context.Context, volume string) tools.Result {
		return fileCall(t, ctx, &Write{}, map[string]any{"volume": volume, "path": "a.txt", "content": "x"})
	}
	if res := write(ctx, "scratch"); res.IsError {
		t.Fatalf("without the setting the write must work (otherwise this test proves nothing): %s", res.Text)
	}
	ro := tools.WithEphemeralReadOnly(ctx)
	if res := write(ro, "scratch"); !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Errorf("a read-only run wrote to the ephemeral volume: error=%v %q", res.IsError, res.Text)
	}
	if res := fileCall(t, ro, &Edit{}, map[string]any{"volume": "scratch", "path": "a.txt", "old_string": "x", "new_string": "y"}); !res.IsError {
		t.Error("a read-only run edited a file on the ephemeral volume")
	}
	if res := fileCall(t, ro, &Read{}, map[string]any{"volume": "scratch", "path": "a.txt"}); res.IsError || res.Text != "x" {
		t.Errorf("a read-only run cannot read the ephemeral volume: %q", res.Text)
	}
	if res := write(ro, "default"); res.IsError {
		t.Errorf("the setting reached the agent's own read-write binding: %s", res.Text)
	}
	// The volume itself is unchanged: another run of the tree still writes.
	if res := write(ctx, "scratch"); res.IsError {
		t.Errorf("the setting changed the volume for a run without it: %s", res.Text)
	}
}
