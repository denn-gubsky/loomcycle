package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
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

// TestMcpPrincipalCtx_NoPrincipal_MatchesOperatorCtx pins RFC AG's
// no-principal contract: when no authenticated principal is on ctx (the
// stdio / open-mode path), mcpPrincipalCtx must be byte-identical to the
// historical operatorCtx — same synthetic identity, same full-operator
// policies. A drift here would change behavior for every stdio MCP launch.
func TestMcpPrincipalCtx_NoPrincipal_MatchesOperatorCtx(t *testing.T) {
	got := mcpPrincipalCtx(context.Background())
	want := operatorCtx(context.Background())

	g, w := tools.RunIdentity(got), tools.RunIdentity(want)
	if g.UserID != w.UserID || g.AgentID != w.AgentID || g.TenantID != w.TenantID {
		t.Errorf("RunIdentity drift: got {U:%q A:%q T:%q} want {U:%q A:%q T:%q}",
			g.UserID, g.AgentID, g.TenantID, w.UserID, w.AgentID, w.TenantID)
	}
	if g.UserID != operatorUserID {
		t.Errorf("no-principal UserID = %q, want synthetic %q", g.UserID, operatorUserID)
	}
	if tools.AgentName(got) != tools.AgentName(want) {
		t.Errorf("AgentName drift: got %q want %q", tools.AgentName(got), tools.AgentName(want))
	}
	// Operator plane is granted on the no-principal path (operator-trust).
	if !tools.OperatorTokenDefPolicy(got).Admin {
		t.Errorf("no-principal path must grant OperatorTokenDef admin")
	}
}

// TestMcpPrincipalCtx_AuthenticatedPrincipal_KeysOnSubject is the RFC AG
// §3.1a fail-before guard: an authenticated MCP call must stamp identity
// from the PRINCIPAL (UserID = principal.Subject, TenantID =
// principal.TenantID), NOT the synthetic operatorUserID. This is the fix
// for the Document-Assistant mismatch — an MCP-created document landing
// under "mcp-operator" was invisible to the Web UI (which reads
// principal.Subject). Reverting the `UserID: p.Subject` stamp in
// mcpPrincipalCtx back to operatorUserID must break this test.
func TestMcpPrincipalCtx_AuthenticatedPrincipal_KeysOnSubject(t *testing.T) {
	p := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeAdmin}}
	ctx := mcpPrincipalCtx(auth.WithPrincipal(context.Background(), p))

	rid := tools.RunIdentity(ctx)
	if rid.UserID != "alice" {
		t.Errorf("RunIdentity.UserID = %q, want principal subject %q (user-scoped tools would key on the wrong id)", rid.UserID, "alice")
	}
	if rid.TenantID != "acme" {
		t.Errorf("RunIdentity.TenantID = %q, want principal tenant %q", rid.TenantID, "acme")
	}
	// Agent-scope scope_id stays the synthetic operator name (determinism).
	if name := tools.AgentName(ctx); name != operatorAgentName {
		t.Errorf("AgentName = %q, want synthetic %q (agent-scope determinism)", name, operatorAgentName)
	}
}

// TestMcpPrincipalCtx_NonAdminPrincipal_DeniesOperatorPlane pins the
// floor for RFC AG Phase 2 (when the route opens to tenant tokens): a
// non-admin principal keeps its own identity + the "any"-within-tenant def
// scopes, but the OPERATOR PLANE (token mint, cross-tenant history,
// cross-agent evaluation) stays default-deny. An admin principal gets the
// full plane. This guards against a tenant principal silently inheriting
// operator authority through the shared policy grant.
func TestMcpPrincipalCtx_NonAdminPrincipal_DeniesOperatorPlane(t *testing.T) {
	admin := mcpPrincipalCtx(auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}))
	tenant := mcpPrincipalCtx(auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"substrate:tenant"}}))

	// Operator plane: admin yes, tenant no.
	if !tools.OperatorTokenDefPolicy(admin).Admin {
		t.Errorf("admin principal must grant OperatorTokenDef admin")
	}
	if tools.OperatorTokenDefPolicy(tenant).Admin {
		t.Errorf("non-admin principal must NOT grant OperatorTokenDef admin (mint plane leak)")
	}
	if len(tools.HistoryPolicy(admin).Scopes) == 0 {
		t.Errorf("admin principal must grant History scopes")
	}
	if len(tools.HistoryPolicy(tenant).Scopes) != 0 {
		t.Errorf("non-admin principal must NOT grant cross-tenant History; got %v", tools.HistoryPolicy(tenant).Scopes)
	}
	// Evaluation: admin gets the cross-agent verbs; tenant gets submit_self only.
	for _, s := range tools.EvaluationPolicy(tenant).Scopes {
		if s == "submit_any" || s == "read_any" || s == "submit_descendants" {
			t.Errorf("non-admin Evaluation must be submit_self only; got %v", tools.EvaluationPolicy(tenant).Scopes)
			break
		}
	}

	// Def families: both admin and tenant get "any" within their stamped
	// tenant (confinement is the TenantID stamp + lookup filter, not a scope
	// narrowing — RFC AG §3.1).
	if len(tools.AgentDefPolicy(tenant).Scopes) == 0 {
		t.Errorf("non-admin principal must still get def-family scopes (tenant-confined)")
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
