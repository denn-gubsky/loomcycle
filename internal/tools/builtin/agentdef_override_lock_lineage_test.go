package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// overrideLockFixture is an agent "judge" defined at runtime (no static
// definition, so lookup adds no lock of its own) with an open first version
// and a locked, active second one, authored by an agent. It returns the tool,
// the author's ctx and v1's def_id.
func overrideLockFixture(t *testing.T) (*AgentDef, context.Context, string) {
	t.Helper()
	tool, ctx, cleanup := agentDefFixture(t)
	t.Cleanup(cleanup)
	if _, static := tool.Cfg.Agents["judge"]; static {
		t.Fatal("the fixture declares a static judge; this test is about one defined at runtime")
	}
	v1 := execOK(t, tool, ctx, `{"op":"create","name":"judge","overlay":{"system_prompt":"v1"}}`)
	execOK(t, tool, ctx, `{"op":"fork","name":"judge","promote":true,"overlay":{"system_prompt":"v2","allow_overrides":false}}`)
	assertJudgeLocked(t, tool, "", "setup")
	return tool, ctx, defIDOf(t, v1)
}

func execOK(t *testing.T, tool *AgentDef, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(input))
	if res.IsError {
		t.Fatalf("%s: %s", input, res.Text)
	}
	return res
}

func assertJudgeLocked(t *testing.T, tool *AgentDef, tenant, when string) {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, tenant, "judge")
	if !ok {
		t.Fatalf("%s: judge did not resolve", when)
	}
	if def.AllowOverrides == nil || *def.AllowOverrides {
		t.Fatalf("%s: judge resolves with allow_overrides = %v, want the lock (false)", when, def.AllowOverrides)
	}
}

// Forking the version from before the lock, pinned by parent_def_id, would
// mint a version without it — refused whether or not the fork is promoted,
// since an unpromoted version is the newest one, which a later promote or a
// cleared active pointer brings forward.
func TestAgentDefTool_ForkOfAVersionBeforeTheOverrideLockIsRefused(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	for who, c := range map[string]context.Context{"an agent": ctx, "an agent in a run": tools.WithRunID(ctx, "r_parent")} {
		for _, promote := range []string{"true", "false"} {
			res, _ := tool.Execute(c, json.RawMessage(`{"op":"fork","name":"judge","parent_def_id":"`+v1+`","promote":`+promote+`,"overlay":{"system_prompt":"v3"}}`))
			if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
				t.Errorf("%s, promote=%s: fork of the open v1 = %s, want it refused", who, promote, res.Text)
			}
			assertJudgeLocked(t, tool, "", who)
		}
	}
	// The same fork that puts the lock back on is fine.
	execOK(t, tool, ctx, `{"op":"fork","name":"judge","parent_def_id":"`+v1+`","promote":true,"overlay":{"system_prompt":"v3","allow_overrides":false}}`)
	assertJudgeLocked(t, tool, "", "after a fork of v1 that keeps the lock")
}

// Promoting the version from before the lock would make the open one active.
func TestAgentDefTool_PromoteOfAVersionBeforeTheOverrideLockIsRefused(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+v1+`"}`))
	if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
		t.Errorf("promote of the open v1 = %s, want it refused", res.Text)
	}
	assertJudgeLocked(t, tool, "", "after the refused promote")
}

// A create of the same name is a new version of the same lineage: without
// the lock it is refused, as a fork is.
func TestAgentDefTool_CreateOverALockedAgentMustKeepTheOverrideLock(t *testing.T) {
	tool, ctx, _ := overrideLockFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"judge","overlay":{"system_prompt":"anew"}}`))
	if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
		t.Errorf("create without the lock = %s, want it refused", res.Text)
	}
	assertJudgeLocked(t, tool, "", "after the refused create")
	execOK(t, tool, ctx, `{"op":"create","name":"judge","overlay":{"system_prompt":"anew","allow_overrides":false}}`)
	assertJudgeLocked(t, tool, "", "after a create that keeps the lock")
}

// The lock stays on once the active version drops out: a retired active
// version clears the pointer, and the newest version still holds the lock.
func TestAgentDefTool_PromoteAfterRetiringTheLockedVersionIsRefused(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	active, err := tool.Store.AgentDefGetActive(context.Background(), "", "judge")
	if err != nil {
		t.Fatal(err)
	}
	execOK(t, tool, ctx, `{"op":"retire","def_id":"`+active.DefID+`","retired":true}`)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+v1+`"}`))
	if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
		t.Errorf("promote of the open v1 after retiring the locked active = %s, want it refused", res.Text)
	}
}

// The operator plane may take the lock off — by a create, a fork of an older
// version or a promote of one — as it may lift a read mode.
func TestAgentDefTool_TheOperatorMayLiftTheOverrideLock(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	op := operatorPlane(ctx)
	execOK(t, tool, op, `{"op":"promote","def_id":"`+v1+`"}`)
	def, _ := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "judge")
	if def.AllowOverrides != nil {
		t.Errorf("after the operator promoted v1: allow_overrides = %v, want it open", *def.AllowOverrides)
	}
}

// Lineages without the lock, and forks that keep it, are untouched.
func TestAgentDefTool_OverrideLockLeavesOtherVersionsAlone(t *testing.T) {
	tool, ctx, _ := overrideLockFixture(t)
	// Forking the locked active version keeps the lock and is allowed.
	execOK(t, tool, ctx, `{"op":"fork","name":"judge","promote":true,"overlay":{"system_prompt":"v3"}}`)
	assertJudgeLocked(t, tool, "", "after a fork of the active version")

	// An agent never locked: fork an older version, promote it, create anew.
	s1 := execOK(t, tool, ctx, `{"op":"create","name":"scorer","overlay":{"system_prompt":"s1"}}`)
	execOK(t, tool, ctx, `{"op":"fork","name":"scorer","promote":true,"overlay":{"system_prompt":"s2"}}`)
	execOK(t, tool, ctx, `{"op":"fork","name":"scorer","parent_def_id":"`+defIDOf(t, s1)+`","overlay":{"system_prompt":"s3"}}`)
	execOK(t, tool, ctx, `{"op":"promote","def_id":"`+defIDOf(t, s1)+`"}`)
	execOK(t, tool, ctx, `{"op":"create","name":"scorer","overlay":{"system_prompt":"s4"}}`)
}

// A tenant that holds no version of its own runs the shared ("") tier's. A
// fork of the shared tier's open, older version would be the tenant's first
// version and resolve ahead of the locked shared one.
func TestAgentDefTool_ATenantForkOfASharedVersionBeforeTheLockIsRefused(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	acme := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	assertJudgeLocked(t, tool, "acme", "before: acme resolves the shared version")
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"fork","name":"judge","parent_def_id":"`+v1+`","promote":true,"overlay":{"system_prompt":"acme v1"}}`))
	if !res.IsError || !strings.Contains(res.Text, "allow_overrides") {
		t.Errorf("acme's fork of the shared open v1 = %s, want it refused", res.Text)
	}
	assertJudgeLocked(t, tool, "acme", "after acme's refused fork")
	// Forking the shared active version keeps the lock and is allowed.
	execOK(t, tool, acme, `{"op":"fork","name":"judge","promote":true,"overlay":{"system_prompt":"acme v1"}}`)
	assertJudgeLocked(t, tool, "acme", "after acme forked the shared active version")
}
