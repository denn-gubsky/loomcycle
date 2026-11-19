package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func lookupWorkspace(t *testing.T, tool *AgentDef, name string) *config.AgentWorkspace {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("%s not found", name)
	}
	return def.Workspace
}

// RFC GW: workspace authored over the substrate survives persist and
// resolve, a fork that does not send it keeps its parent's, and a create or
// fork that changes only it mints a new version (it is not part of the
// content hash, so the create dedup has to compare it).
func TestAgentDefTool_WorkspaceSurvivesCreateForkAndDedup(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)
	read := &config.AgentWorkspace{Ops: []string{"read"}}
	all := &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Engine: "container"}

	for _, tc := range []struct {
		name, field string
		want        *config.AgentWorkspace
	}{
		{"ws-absent", ``, nil},
		{"ws-read", `,"workspace":{"ops":["read"]}`, read},
		{"ws-all", `,"workspace":{"ops":["read","write","exec"],"open":true,"engine":"container"}`, all},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+tc.name+`","promote":true,"overlay":{
			"system_prompt":"code","tools":["Read"]`+tc.field+`}}`))
		if res.IsError {
			t.Fatalf("create %s: %s", tc.name, res.Text)
		}
		if got := lookupWorkspace(t, tool, tc.name); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: workspace = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"ws-read","promote":true,"overlay":{
		"system_prompt":"code","tools":["Read"],"workspace":{"ops":["read","write","exec"],"open":true,"engine":"container"}}}`))
	if res.IsError {
		t.Fatalf("re-create changing only workspace: %s", res.Text)
	}
	if got := lookupWorkspace(t, tool, "ws-read"); !reflect.DeepEqual(got, all) {
		t.Errorf("a create changing only workspace deduplicated: workspace = %+v", got)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"ws-all","promote":true,"overlay":{"system_prompt":"code more"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := lookupWorkspace(t, tool, "ws-all"); !reflect.DeepEqual(got, all) {
		t.Errorf("after a fork that did not send it: workspace = %+v, want %+v", got, all)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"ws-all","promote":true,"overlay":{"workspace":{"ops":["read"]}}}`))
	if res.IsError {
		t.Fatalf("fork changing only workspace: %s", res.Text)
	}
	if got := lookupWorkspace(t, tool, "ws-all"); !reflect.DeepEqual(got, read) {
		t.Errorf("after a fork that changed only it: workspace = %+v, want %+v", got, read)
	}
}

// A fork of a YAML agent keeps the YAML agent's workspace. Read from the
// fork's own stored row: lookup answers from the static agent itself.
func TestAgentDefTool_ForkOfAStaticAgentKeepsItsWorkspace(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	want := &config.AgentWorkspace{Ops: []string{"exec"}, Open: true}
	tool.Cfg.Agents["builder"] = config.AgentDef{Model: "m", SystemPrompt: "build", Tools: []string{"Read"}, Workspace: want}

	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"fork","name":"builder","promote":true,"overlay":{"system_prompt":"build carefully"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	row, err := tool.Store.AgentDefGetActive(context.Background(), tools.RunIdentity(ctx).TenantID, "builder")
	if err != nil {
		t.Fatalf("AgentDefGetActive: %v", err)
	}
	var stored lookup.SubstrateAgentDef
	if err := json.Unmarshal(row.Definition, &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Workspace, want) {
		t.Errorf("the stored fork of a static agent has workspace %+v, want %+v", stored.Workspace, want)
	}
}

// The write path refuses an invalid block, even for the operator, and a key
// a later phase adds.
func TestAgentDefTool_RefusesAnInvalidWorkspace(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	op := operatorPlane(ctx)
	res, _ := tool.Execute(op, json.RawMessage(`{"op":"create","name":"coder","overlay":{"workspace":{"ops":["run"]}}}`))
	wantRefused(t, res, "an unknown operation", "workspace.ops[0]", `"run"`)
	res, _ = tool.Execute(op, json.RawMessage(`{"op":"create","name":"coder","overlay":{"workspace":{"ops":["read"],"engine":"vm"}}}`))
	wantRefused(t, res, "an unknown engine", "workspace.engine", `"vm"`)
	res, _ = tool.Execute(op, json.RawMessage(`{"op":"create","name":"coder","overlay":{"workspace":{"ops":["read"],"share":true}}}`))
	wantRefused(t, res, "a later phase's key", "workspace", "share")
}

// The ceiling (RFC GW): an in-run author may store only the operations and
// open its own run holds, under an engine requirement no weaker than its
// own, plus what a forked version already held. A grant of nothing is within
// every ceiling.
func TestAgentDefCreate_WorkspaceCeiling(t *testing.T) {
	tool, ctx := ceilingFixture(t) // in-run author with no workspace grant

	for name, field := range map[string]string{"sdlc/unset": ``, "sdlc/nothing": `,"workspace":{"engine":"host"}`} {
		if res := agentDefOp(t, tool, ctx, "create", name, `{"history_scope":["user"]`+field+`}`); res.IsError {
			t.Fatalf("%s: a grant of nothing was refused: %s", name, res.Text)
		}
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/r0", `{"history_scope":["user"],"workspace":{"ops":["read"]}}`),
		"read by an author with no workspace", "workspace.ops", `"read"`)

	reader := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Workspace = tools.WorkspacePolicyValue{Read: true, Exec: true, Engine: "container"}
	})
	if res := agentDefOp(t, tool, reader, "create", "sdlc/r1", `{"history_scope":["user"],"workspace":{"ops":["exec"],"engine":"container"}}`); res.IsError {
		t.Fatalf("exec within the author's own grant refused: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, reader, "create", "sdlc/w1", `{"history_scope":["user"],"workspace":{"ops":["read","write"],"engine":"container"}}`),
		"write by an author that cannot write", "workspace.ops", `"write"`)
	wantRefused(t, agentDefOp(t, tool, reader, "create", "sdlc/o1", `{"history_scope":["user"],"workspace":{"ops":["read"],"open":true,"engine":"container"}}`),
		"open by an author that cannot open", "workspace.open")
	wantRefused(t, agentDefOp(t, tool, reader, "create", "sdlc/e1", `{"history_scope":["user"],"workspace":{"ops":["read"],"engine":"host"}}`),
		"a weaker engine than the author's", "workspace.engine", `"host"`)
	wantRefused(t, agentDefOp(t, tool, reader, "create", "sdlc/e2", `{"history_scope":["user"],"workspace":{"ops":["read"]}}`),
		"any engine under an author held to containers", "workspace.engine")

	// An operator mints an agent that may open and write on the host engine;
	// a read-only author may fork it keeping that (the forked version's), but
	// cannot fork its own read-only version into a writer.
	op := operatorPlane(context.Background())
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) { rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}} })
	if res, _ := tool.Execute(op, json.RawMessage(`{"op":"create","name":"sdlc/builder","promote":true,
		"overlay":{"history_scope":["user"],"workspace":{"ops":["read","write"],"open":true,"engine":"host"}}}`)); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	if res := agentDefOp(t, tool, reader, "fork", "sdlc/builder", `{"system_prompt":"again"}`); res.IsError {
		t.Fatalf("fork keeping the base's grant: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, reader, "fork", "sdlc/r1", `{"workspace":{"ops":["exec","write"],"engine":"container"}}`),
		"fork widening to write", "workspace.ops", `"write"`)
}
