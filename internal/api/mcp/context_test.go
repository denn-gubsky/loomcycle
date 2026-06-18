package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestOperatorCtx_AttachesAllRequiredPolicies pins the load-bearing
// invariant: operatorCtx must enrich ctx with every policy value the
// builtin tools (Memory, Channel, AgentDef, Evaluation, Context) check
// at Execute time. Without all 5 attaches, the corresponding MCP
// wrapper handler returns "no scope configured" tool errors on every
// call — the v0.8.15 launch bug found in code review.
//
// If a future tool grows a new policy value, add the attach to
// operatorCtx and the assertion here in the same change.
func TestOperatorCtx_AttachesAllRequiredPolicies(t *testing.T) {
	ctx := operatorCtx(context.Background())

	// RunIdentity: scope=user memory ops derive scope_id from
	// UserID; scope=agent ops use AgentID via tools.AgentName.
	rid := tools.RunIdentity(ctx)
	if rid.UserID == "" {
		t.Errorf("RunIdentity.UserID is empty; scope=user memory ops would fail")
	}
	if rid.AgentID == "" {
		t.Errorf("RunIdentity.AgentID is empty")
	}

	// AgentName: scope=agent memory ops use this as scope_id.
	if name := tools.AgentName(ctx); name == "" {
		t.Errorf("tools.AgentName(ctx) is empty; scope=agent ops would fail")
	}

	// MemoryPolicy: empty AllowedScopes ⇒ every memory op fails.
	mp := tools.MemoryPolicy(ctx)
	if len(mp.AllowedScopes) == 0 {
		t.Errorf("MemoryPolicy.AllowedScopes empty; all Memory ops would fail")
	}
	wantMemScopes := map[string]bool{"agent": false, "user": false, "global": false}
	for _, s := range mp.AllowedScopes {
		if _, ok := wantMemScopes[s]; ok {
			wantMemScopes[s] = true
		}
	}
	for scope, present := range wantMemScopes {
		if !present {
			t.Errorf("MemoryPolicy.AllowedScopes missing %q", scope)
		}
	}

	// ChannelPolicy: empty Publish/Subscribe ⇒ all channel ops fail.
	cp := tools.ChannelPolicy(ctx)
	if len(cp.Publish) == 0 {
		t.Errorf("ChannelPolicy.Publish empty; publish ops would fail")
	}
	if len(cp.Subscribe) == 0 {
		t.Errorf("ChannelPolicy.Subscribe empty; subscribe/peek/ack would fail")
	}

	// AgentDefPolicy: empty Scopes ⇒ every mutation op fails.
	ap := tools.AgentDefPolicy(ctx)
	if len(ap.Scopes) == 0 {
		t.Errorf("AgentDefPolicy.Scopes empty; all AgentDef ops would fail")
	}
	if ap.SelfName == "" {
		t.Errorf("AgentDefPolicy.SelfName empty; `self` scope check would always fail")
	}

	// EvaluationPolicy: needs submit_any + read_any for full operator access.
	ep := tools.EvaluationPolicy(ctx)
	wantEvalScopes := map[string]bool{"submit_any": false, "read_any": false}
	for _, s := range ep.Scopes {
		if _, ok := wantEvalScopes[s]; ok {
			wantEvalScopes[s] = true
		}
	}
	for scope, present := range wantEvalScopes {
		if !present {
			t.Errorf("EvaluationPolicy.Scopes missing %q (operator-direct evaluation ops would fail)", scope)
		}
	}

	// HistoryPolicy: empty Scopes ⇒ Context.history refuses.
	hp := tools.HistoryPolicy(ctx)
	if len(hp.Scopes) == 0 {
		t.Errorf("HistoryPolicy.Scopes empty; Context.history would refuse")
	}

	// SkillDefPolicy: empty Scopes ⇒ MCP `skilldef` meta-tool refuses
	// every op.
	skp := tools.SkillDefPolicy(ctx)
	if len(skp.Scopes) == 0 {
		t.Errorf("SkillDefPolicy.Scopes empty; all SkillDef ops via MCP would fail")
	}

	// ScheduleDefPolicy: empty Scopes ⇒ MCP `scheduledef` meta-tool
	// refuses every op with default-deny. v1.x RFC E regression: this
	// was missing from operatorCtx for one release and made every MCP
	// scheduledef call return tool_refused silently.
	sdp := tools.ScheduleDefPolicy(ctx)
	if len(sdp.Scopes) == 0 {
		t.Errorf("ScheduleDefPolicy.Scopes empty; all ScheduleDef ops via MCP would fail")
	}
	if sdp.SelfName == "" {
		t.Errorf("ScheduleDefPolicy.SelfName empty; `self` scope check would always fail")
	}

	// VolumeDefPolicy: empty Scopes ⇒ MCP `volumedef` meta-tool refuses
	// create/delete/purge with default-deny. RFC AH Phase 5 regression guard.
	vdp := tools.VolumeDefPolicy(ctx)
	if len(vdp.Scopes) == 0 {
		t.Errorf("VolumeDefPolicy.Scopes empty; VolumeDef create/delete/purge via MCP would fail")
	}
}

// TestOperatorCtx_PreservesParentValues ensures operatorCtx layers on
// top of an existing ctx rather than replacing it — values attached
// upstream (e.g., cancellation, request ID) survive.
func TestOperatorCtx_PreservesParentValues(t *testing.T) {
	type customKey struct{}
	parent := context.WithValue(context.Background(), customKey{}, "parent-marker")
	enriched := operatorCtx(parent)
	if got, _ := enriched.Value(customKey{}).(string); got != "parent-marker" {
		t.Errorf("operatorCtx dropped parent ctx value; got %q want %q", got, "parent-marker")
	}
}
