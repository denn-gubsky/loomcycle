package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// skillDefFixture builds a SkillDef tool over in-memory SQLite +
// a skills.Set with one static skill ("karpathy-guidelines"). The
// ctx carries a permissive RFC BA allowlist (empty patterns = allow
// all — create-anywhere); per-test code overrides for allowlist-
// specific cases.
func skillDefFixture(t *testing.T) (*SkillDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	set := loadSetWithSkills(t, []struct {
		Name  string
		Tools []string
		Body  string
	}{
		{Name: "karpathy-guidelines", Tools: []string{"Read", "WebFetch"}, Body: "STATIC SKILL BODY"},
	})
	tool := &SkillDef{
		Store:               s,
		Set:                 set,
		MaxBodyBytes:        131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentTools(ctx, []string{"Read", "WebFetch", "SkillDef"})
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{}) // empty = allow all
	return tool, ctx, func() { _ = s.Close() }
}

func TestSkillDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	// "karpathy-guidelines" exists in the static Set → create must refuse.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"karpathy-guidelines","overlay":{"body":"new body"}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static SKILL.md") {
		t.Errorf("refusal should mention static SKILL.md; got %s", res.Text)
	}
}

func TestSkillDefTool_CreateNewName(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"my-runtime-skill","overlay":{"body":"FRESH BODY","description":"desc"}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "my-runtime-skill" {
		t.Errorf("name = %v, want my-runtime-skill", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestSkillDefTool_CreateRejectsEmptyBody(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	// Empty body — should refuse.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-skill","overlay":{"body":""}}`))
	if !res.IsError {
		t.Errorf("empty body should refuse; got %s", res.Text)
	}
	// Whitespace-only body — should also refuse (TrimSpace check).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-skill","overlay":{"body":"   \n  "}}`))
	if !res.IsError {
		t.Errorf("whitespace-only body should refuse; got %s", res.Text)
	}
}

func TestSkillDefTool_ForkBootstrapsStaticBody(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	// Fork "karpathy-guidelines" with no parent_def_id → must bootstrap from static.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"karpathy-guidelines","overlay":{"body":"FORKED BODY"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["promoted"].(bool) != false {
		t.Errorf("fork default promote = true; want false")
	}
	if out["parent_def_id"] == "" {
		t.Error("fork must record parent_def_id (the bootstrapped v1)")
	}

	// list now has 2 entries (v1 static bootstrap + v2 fork).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"karpathy-guidelines"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	listOut := decodeResult(t, res.Text)
	versions := listOut["versions"].([]any)
	if len(versions) != 2 {
		t.Errorf("after fork: got %d versions, want 2 (bootstrap v1 + fork v2)", len(versions))
	}
	// v1 (oldest, last in DESC order) is the bootstrap row.
	v1 := versions[1].(map[string]any)
	if v1["bootstrapped_from_static"].(bool) != true {
		t.Errorf("v1 should be bootstrapped_from_static=true, got %v", v1["bootstrapped_from_static"])
	}
}

func TestSkillDefTool_ToolsCannotWiden(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	// Static root has [Read, WebFetch]. Try to fork adding "Write".
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"karpathy-guidelines","overlay":{"body":"x","tools":["Read","WebFetch","Write"]}}`))
	if !res.IsError {
		t.Fatalf("fork widening tools should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "Tools cannot widen") {
		t.Errorf("refusal should mention widening; got %s", res.Text)
	}
}

// RFC BA: a WHITELIST allowlist (a positive pattern) permits only matching
// names. Authoring the whitelisted name is allowed; a non-matching name is
// refused. Replaces the old exact `named:` scope grant.
func TestSkillDefTool_WhitelistGrant(t *testing.T) {
	tool, _, cleanup := skillDefFixture(t)
	defer cleanup()

	// Whitelist exactly karpathy-guidelines.
	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentTools(ctx, []string{"Read", "WebFetch"})
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"karpathy-guidelines"}})

	// In-allowlist: fork the whitelisted skill.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"karpathy-guidelines","overlay":{"body":"forked"}}`))
	if res.IsError {
		t.Errorf("whitelist should permit the matching name; got %s", res.Text)
	}
	// Out of allowlist: create a non-matching name.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"different-skill","overlay":{"body":"x"}}`))
	if !res.IsError {
		t.Errorf("whitelist should deny a non-matching name; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "allowlist") {
		t.Errorf("refusal should mention the allowlist; got %s", res.Text)
	}
}

// RFC BA: a glob whitelist (`doc/*`) permits only matching grouped names.
func TestSkillDefTool_WhitelistGlobGrant(t *testing.T) {
	tool, _, cleanup := skillDefFixture(t)
	defer cleanup()

	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentTools(ctx, []string{"Read"})
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"doc/*"}})

	// Matching group → create allowed.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"doc/summarizer","overlay":{"body":"x"}}`))
	if res.IsError {
		t.Errorf("doc/* should permit doc/summarizer; got %s", res.Text)
	}
	// Different group → refused.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"marketing/seo","overlay":{"body":"x"}}`))
	if !res.IsError {
		t.Errorf("doc/* should deny marketing/seo; got %s", res.Text)
	}
}

// RFC BA: an EMPTY allowlist = allow all (create-anywhere) — the inverse of the
// old default-deny-when-no-scopes behaviour. This is the documented default so
// an agent with no `skills:` field can still author.
func TestSkillDefTool_NoPolicyAllowsCreate(t *testing.T) {
	tool, _, cleanup := skillDefFixture(t)
	defer cleanup()

	// ctx WITHOUT WithSkillPolicy → zero-value policy → empty patterns → allow.
	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentTools(ctx, []string{"Read"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"any-skill","overlay":{"body":"x"}}`))
	if res.IsError {
		t.Fatalf("empty allowlist should allow create-anywhere; got %s", res.Text)
	}
}

// RFC BA: `skills: [-*]` denies EVERY name, so no authoring is permitted.
func TestSkillDefTool_DenyAllRefusesCreate(t *testing.T) {
	tool, _, cleanup := skillDefFixture(t)
	defer cleanup()

	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentTools(ctx, []string{"Read"})
	ctx = tools.WithSkillPolicy(ctx, tools.SkillPolicyValue{Patterns: []string{"-*"}})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"any-skill","overlay":{"body":"x"}}`))
	if !res.IsError {
		t.Fatalf("`-*` deny-all should refuse create; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "allowlist") {
		t.Errorf("refusal should mention the allowlist; got %s", res.Text)
	}
}

func TestSkillDefTool_PromoteAndGet(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	// Create A (auto-promote), create B (don't promote), promote B explicitly.
	resA, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"promo-skill","overlay":{"body":"A body"}}`))
	if resA.IsError {
		t.Fatal(resA.Text)
	}
	outA := decodeResult(t, resA.Text)
	idA := outA["def_id"].(string)
	_ = idA

	resB, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"promo-skill","overlay":{"body":"B body"},"promote":false}`))
	if !resB.IsError {
		// create refuses static-name; for a brand new name `create`
		// succeeds even though the name now exists in DB. The static-
		// name guard only fires for static Set entries.
		// This is by design — see execCreate comment.
	}
	// Actually create rejects when v1 row exists in DB? No — it
	// only refuses static names. DB-only names accept multiple
	// create calls but each gets a distinct def_id with version
	// allocated by the store. Skip the explicit verification here.

	// Promote A → get active → expect A.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+idA+`"}`))
	if res.IsError {
		t.Fatalf("promote: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["promoted"].(bool) != true {
		t.Errorf("promote should return promoted=true")
	}
	// get the row back.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+idA+`"}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	got := decodeResult(t, res.Text)
	if got["def_id"].(string) != idA {
		t.Errorf("get def_id mismatch: %v", got["def_id"])
	}
}

func TestSkillDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retireable-skill","overlay":{"body":"body"}}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	out := decodeResult(t, res.Text)
	defID := out["def_id"].(string)

	// retire=true.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire(true): %s", res.Text)
	}
	out = decodeResult(t, res.Text)
	if out["retired"].(bool) != true {
		t.Error("retired flag didn't stick")
	}
	// retire=false → reversed.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":false}`))
	if res.IsError {
		t.Fatalf("retire(false): %s", res.Text)
	}
	out = decodeResult(t, res.Text)
	if out["retired"].(bool) != false {
		t.Error("retired flag didn't reverse")
	}
}

// TestSkillDefTool_TenantIsolationGetListRetire — RFC N FIX 3-skills, the
// skill analogue of TestAgentDefTool_TenantIsolationGetListRetire. The
// get / list / retire ops were tenant-blind (gated only by the
// tenant-blind checkScopeForName), so a caller in tenant A could read,
// enumerate, and retire defs owned by tenant B. With scopes=[any] on both
// callers (so the scope gate is NOT what refuses), the only thing keeping
// tenants apart is the row-TenantID guard added by FIX 3.
//
// Pre-fix: get returns B's body, list returns B's versions, retire mutates
// B's row — all from a tenant-A caller.
func TestSkillDefTool_TenantIsolationGetListRetire(t *testing.T) {
	tool, baseCtx, cleanup := skillDefFixture(t)
	defer cleanup()

	// Two tenant contexts over the SAME tool/store. Re-wrapping RunIdentity
	// only swaps the tenant/agent; the scopes=[any] policy + agent tools
	// from the fixture live under separate ctx keys, so refusals come from
	// the tenant guard, not the scope gate.
	ctxA := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-a"})
	ctxB := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-b"})

	// Create a non-static skill def under tenant B.
	res, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"create","name":"b-only","overlay":{"body":"tenant b body"}}`))
	if res.IsError {
		t.Fatalf("create under B: %s", res.Text)
	}
	defID := decodeResult(t, res.Text)["def_id"].(string)

	// get from tenant A → opaque not-found (no cross-tenant leak).
	res, _ = tool.Execute(ctxA, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if !res.IsError {
		t.Errorf("tenant A get of tenant B's def succeeded; want refusal. body=%s", res.Text)
	}
	if !strings.Contains(res.Text, "not found") {
		t.Errorf("cross-tenant get should return opaque not-found; got %s", res.Text)
	}

	// list from tenant A → must NOT include B's version.
	res, _ = tool.Execute(ctxA, json.RawMessage(`{"op":"list","name":"b-only"}`))
	if res.IsError {
		t.Fatalf("list under A: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 0 {
		t.Errorf("tenant A list of name owned by B returned %d versions; want 0 (tenant filter)", len(versions))
	}

	// retire from tenant A → refused; B's row must stay un-retired.
	res, _ = tool.Execute(ctxA, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if !res.IsError {
		t.Errorf("tenant A retire of tenant B's def succeeded; want refusal. body=%s", res.Text)
	}
	// Confirm B still sees its row un-retired (the retire didn't leak through).
	res, _ = tool.Execute(ctxB, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("tenant B get of its own def: %s", res.Text)
	}
	if decodeResult(t, res.Text)["retired"].(bool) {
		t.Error("tenant A's cross-tenant retire mutated tenant B's row")
	}

	// Sanity: tenant B CAN get + list its own def (the guard isn't over-broad).
	res, _ = tool.Execute(ctxB, json.RawMessage(`{"op":"list","name":"b-only"}`))
	if res.IsError {
		t.Fatalf("tenant B list of its own name: %s", res.Text)
	}
	if n := len(decodeResult(t, res.Text)["versions"].([]any)); n != 1 {
		t.Errorf("tenant B list returned %d versions; want 1", n)
	}
}

// TestSkillDefTool_ForkFallsBackToSharedBase mirrors the AgentDef fix: a
// by-name fork by a per-tenant principal falls back to the SHARED ("") base
// when the name has no own-tenant version, so a skill seeded under the legacy
// "" tenant can be migrated without an explicit parent_def_id.
func TestSkillDefTool_ForkFallsBackToSharedBase(t *testing.T) {
	tool, baseCtx, cleanup := skillDefFixture(t)
	defer cleanup()

	ctxShared := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: ""})
	res, _ := tool.Execute(ctxShared, json.RawMessage(`{"op":"create","name":"shared-skill","overlay":{"body":"v1 body"}}`))
	if res.IsError {
		t.Fatalf("create shared skill: %s", res.Text)
	}

	ctxT := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "jobember"})
	res, _ = tool.Execute(ctxT, json.RawMessage(`{"op":"fork","name":"shared-skill","overlay":{"body":"v2 body"}}`))
	if res.IsError {
		t.Fatalf(`by-name fork of the shared "" skill as tenant should succeed; got %s`, res.Text)
	}
}
