package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// An operator taking an agent's override lock off is written to the audit
// log, as taking a no-trace mode off is: who, which agent, which tenant.
func TestAgentDefTool_AnOperatorLiftingTheOverrideLockIsAudited(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	sink := &noTraceAuditSink{}
	tool.Audit = sink
	op := operatorPlane(auth.WithPrincipal(ctx, auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
	execOK(t, tool, op, `{"op":"promote","def_id":"`+v1+`"}`)
	if len(sink.evs) != 1 {
		t.Fatalf("audit events = %d (%+v), want 1", len(sink.evs), sink.evs)
	}
	ev := sink.evs[0]
	if ev.Action != "allow_overrides_lock_removed" || ev.TargetName != "judge" || ev.TargetTenant != "" || ev.ActorSubject != "ops" {
		t.Errorf("audit event = %+v, want allow_overrides_lock_removed on judge in the shared tenant by ops", ev)
	}
}

// When the record cannot be written, the lift is refused and the lock stays.
func TestAgentDefTool_TheOverrideLockStaysWhenItsLiftCannotBeAudited(t *testing.T) {
	tool, ctx, v1 := overrideLockFixture(t)
	tool.Audit = &noTraceAuditSink{fail: true}
	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"promote","def_id":"`+v1+`"}`))
	if !res.IsError {
		t.Fatal("promote of the open version succeeded although its audit record could not be written")
	}
	assertJudgeLocked(t, tool, "", "after the refused promote")
}

// Changes that keep the lock, and lineages that never had it, write nothing.
func TestAgentDefTool_KeepingTheOverrideLockWritesNoAudit(t *testing.T) {
	tool, ctx, _ := overrideLockFixture(t)
	sink := &noTraceAuditSink{}
	tool.Audit = sink
	op := operatorPlane(ctx)
	execOK(t, tool, op, `{"op":"fork","name":"judge","promote":true,"overlay":{"system_prompt":"v3"}}`)
	execOK(t, tool, op, `{"op":"create","name":"open","overlay":{"system_prompt":"o1"}}`)
	execOK(t, tool, op, `{"op":"fork","name":"open","promote":true,"overlay":{"system_prompt":"o2"}}`)
	if len(sink.evs) != 0 {
		t.Errorf("audit events = %+v, want none", sink.evs)
	}
	if _, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "open"); !ok {
		t.Fatal("open did not resolve")
	}
}
