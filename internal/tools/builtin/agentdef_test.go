package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentDefFixture builds an AgentDef tool over in-memory SQLite +
// a stub Config with one "static" agent. Returns a ctx with a
// permissive policy (scopes=[any]); per-test code overrides for
// scope-specific cases.
func agentDefFixture(t *testing.T) (*AgentDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"researcher": {
				Provider:     "anthropic",
				Model:        "claude-haiku-4-5",
				SystemPrompt: "operator-blessed root prompt",
				AllowedTools: []string{"Read", "WebFetch", "AgentDef"},
			},
		},
	}
	tool := &AgentDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "researcher",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestAgentDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// "researcher" exists in cfg.Agents → create must refuse.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"researcher","overlay":{"system_prompt":"new"}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.Agents") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

// F40 (RFC W): an agent created via AgentDef with `*_def_scopes` in the
// overlay must round-trip those capability gates — overlay → persisted def →
// lookup.Agent → config.AgentDef — so a runtime-authored meta-agent (breeder /
// scheduler-of-agents) isn't silently dropped to default-deny. Fail-before:
// the overlay struct had no def-scope fields, so they resolved empty.
func TestAgentDefTool_CreateRoundTripsDefScopes(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// No allowed_tools in the overlay: the def-scopes round-trip is what F40 is about,
	// and the narrow-only allowed_tools ceiling check requires WithAgentTools on ctx,
	// which agentDefFixture intentionally does not set (matches TestAgentDefTool_CreateNewName).
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"breeder","overlay":{`+
		`"system_prompt":"breed agents",`+
		`"agent_def_scopes":["named:foo"],"schedule_def_scopes":["any"],`+
		`"skill_def_scopes":["named:s"],"a2a_server_card_def_scopes":["any"],`+
		`"a2a_agent_def_scopes":["any"]}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "breeder")
	if !ok {
		t.Fatal("breeder did not resolve via lookup.Agent")
	}
	if got := def.AgentDefScopes; len(got) != 1 || got[0] != "named:foo" {
		t.Errorf("AgentDefScopes = %v, want [named:foo] (F40: dropped on the overlay round-trip)", got)
	}
	if got := def.ScheduleDefScopes; len(got) != 1 || got[0] != "any" {
		t.Errorf("ScheduleDefScopes = %v, want [any]", got)
	}
	if got := def.SkillDefScopes; len(got) != 1 || got[0] != "named:s" {
		t.Errorf("SkillDefScopes = %v, want [named:s]", got)
	}
	if len(def.A2AServerCardDefScopes) != 1 || len(def.A2AAgentDefScopes) != 1 {
		t.Errorf("a2a def scopes dropped: server=%v agent=%v", def.A2AServerCardDefScopes, def.A2AAgentDefScopes)
	}
}

// TestAgentDefTool_SamplingRoundTrips: a create overlay's sampling block
// survives create → persist → lookup → config.AgentDef (the breeder mints
// variants this way).
func TestAgentDefTool_SamplingRoundTrips(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"explorer","overlay":{`+
		`"system_prompt":"explore","sampling":{"temperature":0.2,"top_p":0.9,"seed":7}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "explorer")
	if !ok {
		t.Fatal("explorer did not resolve via lookup.Agent")
	}
	if def.Sampling == nil {
		t.Fatal("Sampling dropped on the round-trip")
	}
	if def.Sampling.Temperature == nil || *def.Sampling.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", def.Sampling.Temperature)
	}
	if def.Sampling.TopP == nil || *def.Sampling.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9", def.Sampling.TopP)
	}
	if def.Sampling.Seed == nil || *def.Sampling.Seed != 7 {
		t.Errorf("seed = %v, want 7", def.Sampling.Seed)
	}
}

// TestAgentDefTool_ForkMergesSamplingPerField: a fork that overrides ONLY
// temperature keeps the parent's top_p (per-field merge through the substrate).
func TestAgentDefTool_ForkMergesSamplingPerField(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"explorer","overlay":{`+
		`"system_prompt":"explore","sampling":{"temperature":0.2,"top_p":0.9}}}`)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	// Fork overriding only temperature; promote so lookup resolves the fork.
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"explorer","promote":true,`+
		`"overlay":{"sampling":{"temperature":0.9}}}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "explorer")
	if !ok {
		t.Fatal("explorer did not resolve after fork")
	}
	if def.Sampling == nil || def.Sampling.Temperature == nil || *def.Sampling.Temperature != 0.9 {
		t.Errorf("temperature = %v, want 0.9 (fork override)", def.Sampling.Temperature)
	}
	if def.Sampling.TopP == nil || *def.Sampling.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9 (inherited from parent — fork left it unset)", def.Sampling.TopP)
	}
}

func TestAgentDefTool_CreateNewName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reviewer","overlay":{"provider":"openai","system_prompt":"new agent"},"description":"reviewer for code"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "reviewer" {
		t.Errorf("name = %v, want reviewer", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestAgentDefTool_ForkInheritsParent(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Fork "researcher" (static MD bootstrap). Default promote=false.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"forked prompt"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["promoted"].(bool) != false {
		t.Errorf("fork default promote = true; want false")
	}
	if out["parent_def_id"] == "" {
		t.Error("fork must record parent_def_id")
	}
	// list should now have 2 entries (v1 static bootstrap + v2 fork).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"researcher"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	out = decodeResult(t, res.Text)
	versions := out["versions"].([]any)
	if len(versions) != 2 {
		t.Errorf("after fork: got %d versions, want 2 (bootstrap v1 + fork v2)", len(versions))
	}
}

// TestAgentDefTool_ForkFallsBackToSharedBase pins the fix for the
// admin-`list`-sees-it-but-`fork`-can't gap. A per-tenant principal whose
// registry was seeded under the legacy shared "" tenant (e.g. a jobember
// substrate:admin whose pre-RFC-N defs all live under "") must be able to fork
// that "" base WITHOUT an explicit parent_def_id, landing the new version
// under its OWN tenant. This mirrors run-time lookup.Agent precedence
// (own-tenant → static → shared "").
//
// Regression: on the pre-fix code the no-parent branch jumped straight from
// the own-tenant active-pointer miss to static cfg.Agents and refused "fork:
// no parent" — even though the name was visibly deployed under "". Revert the
// agentdef.go fallback and this fails with that refusal.
func TestAgentDefTool_ForkFallsBackToSharedBase(t *testing.T) {
	tool, baseCtx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Seed a NON-static name under the shared "" tenant — the fixture ctx
	// carries TenantID="" by default — promoted so it has an "" active
	// pointer. (Omit allowed_tools so the later fork can't trip the ceiling.)
	res, _ := tool.Execute(baseCtx, json.RawMessage(`{"op":"create","name":"legacy-seed","overlay":{"system_prompt":"shared base body"}}`))
	if res.IsError {
		t.Fatalf("seed create under \"\" tenant: %s", res.Text)
	}
	sharedDefID := decodeResult(t, res.Text)["def_id"].(string)

	// A per-tenant principal with NO own-tenant version of the name.
	ctxTenant := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "jobember"})

	res, _ = tool.Execute(ctxTenant, json.RawMessage(`{"op":"fork","name":"legacy-seed","overlay":{"system_prompt":"tenant override"},"promote":true}`))
	if res.IsError {
		t.Fatalf("fork should fall back to the shared \"\" base; got refusal: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["parent_def_id"].(string) != sharedDefID {
		t.Errorf("forked parent_def_id = %q, want the \"\" base %q", out["parent_def_id"], sharedDefID)
	}
	tenantForkDefID := out["def_id"].(string)

	// The new version is stamped under jobember: a non-admin tenant `list`
	// is tenant-scoped, so jobember sees exactly its own forked version and
	// NOT the "" base.
	res, _ = tool.Execute(ctxTenant, json.RawMessage(`{"op":"list","name":"legacy-seed"}`))
	if res.IsError {
		t.Fatalf("tenant list after fork: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("jobember list = %d versions, want 1 (its own fork, stamped under jobember)", len(versions))
	}
	if versions[0].(map[string]any)["def_id"].(string) != tenantForkDefID {
		t.Errorf("jobember's only version should be its fork %q", tenantForkDefID)
	}

	// Own-tenant precedence: a SECOND jobember fork now resolves jobember's
	// own active pointer as the parent (not the "" base again).
	res, _ = tool.Execute(ctxTenant, json.RawMessage(`{"op":"fork","name":"legacy-seed","overlay":{"system_prompt":"second tenant rev"},"promote":true}`))
	if res.IsError {
		t.Fatalf("second tenant fork: %s", res.Text)
	}
	if pid := decodeResult(t, res.Text)["parent_def_id"].(string); pid != tenantForkDefID {
		t.Errorf("second fork parent = %q, want the prior tenant fork %q (own-tenant precedence)", pid, tenantForkDefID)
	}
}

// TestAgentDefTool_ForkNoParentStillRefuses guards that the "" fallback did not
// soften the genuine no-parent refusal: a tenant principal forking a name with
// no own-tenant version, no shared "" base, and no static cfg.Agents entry must
// still be refused.
func TestAgentDefTool_ForkNoParentStillRefuses(t *testing.T) {
	tool, baseCtx, cleanup := agentDefFixture(t)
	defer cleanup()

	ctxTenant := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "jobember"})
	res, _ := tool.Execute(ctxTenant, json.RawMessage(`{"op":"fork","name":"never-existed","overlay":{"system_prompt":"x"}}`))
	if !res.IsError {
		t.Fatalf("fork of a name with no own/shared/static parent should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "no parent") {
		t.Errorf("refusal should mention \"no parent\"; got %s", res.Text)
	}
}

// ---- RFC J: inline code_body ingestion ----

// Single-quoted JS string so the body embeds cleanly inside the JSON test
// fixtures below (double quotes would need escaping).
const validInlineBody = `function run(input){ return { final_text: 'ok' }; }`

// TestAgentDefTool_InlineCodeRefusedWhenCodeJSDisabled pins the gate: with
// LOOMCYCLE_CODE_AGENTS_ENABLED off (the fixture default), create/fork must
// refuse a non-empty code_body loudly — persisting a body the runtime can't
// execute would be a silent footgun. Fails on the pre-feature code, which had
// no code_body field and would create "codebot" successfully.
func TestAgentDefTool_InlineCodeRefusedWhenCodeJSDisabled(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"codebot","overlay":{"provider":"code-js","code_body":"`+validInlineBody+`"}}`))
	if !res.IsError {
		t.Fatalf("inline code_body with code-js disabled should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "code agents are disabled") {
		t.Errorf("refusal should mention the disabled gate; got %s", res.Text)
	}
}

// TestAgentDefTool_InlineCodeRejectsSyntaxError pins authorship-time compile.
func TestAgentDefTool_InlineCodeRejectsSyntaxError(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Env.CodeAgentsEnabled = true

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"codebot","overlay":{"provider":"code-js","code_body":"function run(input){ return {final_text: }"}}`))
	if !res.IsError {
		t.Fatalf("syntactically broken code_body should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "does not compile") {
		t.Errorf("refusal should mention compile failure; got %s", res.Text)
	}
}

// TestAgentDefTool_InlineCodeRejectsOversize pins the dedicated byte cap.
func TestAgentDefTool_InlineCodeRejectsOversize(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Env.CodeAgentsEnabled = true
	tool.MaxCodeBytes = 10 // smaller than validInlineBody

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"codebot","overlay":{"provider":"code-js","code_body":"`+validInlineBody+`"}}`))
	if !res.IsError {
		t.Fatalf("oversize code_body should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "exceeds max") {
		t.Errorf("refusal should mention the byte cap; got %s", res.Text)
	}
}

// TestAgentDefTool_InlineCodeCreateAndForkInherits pins the happy path +
// fork inheritance: a created inline body persists, and forking an INLINE
// parent carries the body through buildDefinition's parent-JSON unmarshal.
func TestAgentDefTool_InlineCodeCreateAndForkInherits(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Env.CodeAgentsEnabled = true

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"codebot","overlay":{"provider":"code-js","code_body":"`+validInlineBody+`"}}`))
	if res.IsError {
		t.Fatalf("create inline code agent: %s", res.Text)
	}
	created := decodeResult(t, res.Text)
	if def, _ := created["definition"].(map[string]any); def["code_body"] != validInlineBody {
		t.Fatalf("created definition.code_body = %v, want the inline body", def["code_body"])
	}

	// Fork with no code overlay → must inherit the parent's body.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"codebot","overlay":{"system_prompt":"forked"}}`))
	if res.IsError {
		t.Fatalf("fork inline code agent: %s", res.Text)
	}
	forked := decodeResult(t, res.Text)
	def, _ := forked["definition"].(map[string]any)
	if def["code_body"] != validInlineBody {
		t.Errorf("forked definition.code_body = %v, want inherited inline body", def["code_body"])
	}
}

// TestAgentDefTool_CreateIdempotentOnSameContent pins the content-addressed
// create dedup (mirror of MCPServerDef): re-creating identical content returns
// the active def with deduplicated=true and mints NO new version — the
// server-side guarantee the TS ensureCodeAgent `changed` flag depends on.
// Fails on the pre-fix code, which minted a fresh version on every create.
func TestAgentDefTool_CreateIdempotentOnSameContent(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	body := `{"op":"create","name":"reviewer","overlay":{"provider":"openai","system_prompt":"same"}}`
	r1, _ := tool.Execute(ctx, json.RawMessage(body))
	if r1.IsError {
		t.Fatalf("first create: %s", r1.Text)
	}
	if decodeResult(t, r1.Text)["deduplicated"] == true {
		t.Error("first create should not be a dedup")
	}

	r2, _ := tool.Execute(ctx, json.RawMessage(body))
	if r2.IsError {
		t.Fatalf("second create: %s", r2.Text)
	}
	if decodeResult(t, r2.Text)["deduplicated"] != true {
		t.Errorf("identical re-create should dedup; got %s", r2.Text)
	}

	lr, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"reviewer"}`))
	versions := decodeResult(t, lr.Text)["versions"].([]any)
	if len(versions) != 1 {
		t.Errorf("identical re-create must not mint a new version; got %d", len(versions))
	}
}

func TestAgentDefTool_ForkAllowedToolsCannotWiden(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Static root has [Read, WebFetch, AgentDef]. Try to fork adding "Write".
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"allowed_tools":["Read","WebFetch","Write"]}}`))
	if !res.IsError {
		t.Fatalf("fork widening allowed_tools should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "AllowedTools cannot widen") {
		t.Errorf("refusal should mention widening; got %s", res.Text)
	}
}

func TestAgentDefTool_ForkAllowedToolsCanNarrow(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"allowed_tools":["Read"]}}`))
	if res.IsError {
		t.Fatalf("fork narrowing should succeed; got %s", res.Text)
	}
}

func TestAgentDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   nil,
		SelfName: "researcher",
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"newbot"}`))
	if !res.IsError {
		t.Fatalf("no scopes = default-deny; want refusal, got %s", res.Text)
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestAgentDefTool_SelfScopeOnlyOwnName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"self"},
		SelfName: "researcher",
	})
	// Own name → ok.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher"}`))
	if res.IsError {
		t.Errorf("self scope on own name should succeed; got %s", res.Text)
	}
	// Other name → refused.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"otheragent"}`))
	if !res.IsError {
		t.Fatalf("self scope on different name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "agent_def_scopes") {
		t.Errorf("refusal should mention agent_def_scopes; got %s", res.Text)
	}
}

func TestAgentDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"named:reviewer"},
		SelfName: "orchestrator",
	})
	// "reviewer" → ok.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reviewer"}`))
	if res.IsError {
		t.Errorf("named:reviewer on reviewer should succeed; got %s", res.Text)
	}
	// "coder" → refused.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"coder"}`))
	if !res.IsError {
		t.Fatalf("named:reviewer on coder should refuse")
	}
}

func TestAgentDefTool_PromoteAndGet(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Create with promote=false, then explicit promote.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reviewer","promote":false}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	defID := decodeResult(t, res.Text)["def_id"].(string)
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("promote: %s", res.Text)
	}
	// Get back the row.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if decodeResult(t, res.Text)["name"] != "reviewer" {
		t.Error("get returned wrong row")
	}
}

func TestAgentDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retiretest"}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if !decodeResult(t, res.Text)["retired"].(bool) {
		t.Error("retire(true) didn't stick")
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":false}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if decodeResult(t, res.Text)["retired"].(bool) {
		t.Error("retire(false) didn't reverse")
	}
}

// TestAgentDefTool_ReclaimNameAfterRetire pins the soft-reclaim end-to-end
// at the tool layer: after retiring the active def for a name, a fresh
// `create` of the SAME name succeeds (the cleared active pointer means the
// name no longer collides). This is the operator workflow for granting an
// agent MORE tools — a fork can't widen the allowed_tools ceiling, but a
// recreate (fresh root) can, and reclaim unblocks it.
func TestAgentDefTool_ReclaimNameAfterRetire(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reclaimtest"}`))
	if res.IsError {
		t.Fatalf("first create: %s", res.Text)
	}
	firstDefID := decodeResult(t, res.Text)["def_id"].(string)

	// Retire the active def — clears the active pointer.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+firstDefID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}

	// Re-create the same name — must NOT be blocked, mints a new version.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reclaimtest"}`))
	if res.IsError {
		t.Fatalf("reclaim create after retire: %s", res.Text)
	}
	secondDefID := decodeResult(t, res.Text)["def_id"].(string)
	if secondDefID == "" || secondDefID == firstDefID {
		t.Errorf("reclaim create returned def_id %q (first was %q) — expected a fresh row", secondDefID, firstDefID)
	}
}

// TestAgentDefTool_TenantIsolationGetListRetire — RFC N FIX 3. The
// get / list / retire ops were tenant-blind (gated only by the
// tenant-blind checkScopeForName), so a caller in tenant A could read,
// enumerate, and retire defs owned by tenant B. With scopes=[any] on both
// callers (so the scope gate is NOT what refuses), the only thing keeping
// tenants apart is the row-TenantID guard added by FIX 3.
//
// Pre-fix: get returns B's body, list returns B's versions, retire mutates
// B's row — all from a tenant-A caller.
func TestAgentDefTool_TenantIsolationGetListRetire(t *testing.T) {
	tool, baseCtx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Two tenant contexts over the SAME tool/store. Both carry scopes=[any]
	// (inherited from the fixture) so refusals come from the tenant guard,
	// not the scope gate.
	ctxA := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-a"})
	ctxB := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-b"})

	// Create a non-static def under tenant B.
	res, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"create","name":"b-only","overlay":{"system_prompt":"tenant b body"}}`))
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

// TestAgentDefTool_AdminCrossesTenantsGetList pins the RFC L invariant that a
// substrate:admin principal crosses tenant boundaries on the def-tool READ ops
// (get/list) — the regression behind "agent/skill/MCP bodies not visible in the
// Web UI library": the role-aware library is an admin surface, but the get/list
// ops (which back the lineage panel's def bodies) tenant-filtered with no admin
// bypass, so an admin whose principal tenant differed from the def's tenant
// (notably the shared "" tenant where bootstrapped/legacy defs live) saw empty
// bodies. A NON-admin principal must still be tenant-scoped.
func TestAgentDefTool_AdminCrossesTenantsGetList(t *testing.T) {
	tool, baseCtx, cleanup := agentDefFixture(t)
	defer cleanup()

	// A def owned by tenant-b.
	ctxB := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-b"})
	res, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"create","name":"b-only","overlay":{"system_prompt":"tenant b body"}}`))
	if res.IsError {
		t.Fatalf("create under B: %s", res.Text)
	}
	defID := decodeResult(t, res.Text)["def_id"].(string)

	// substrate:admin principal in a DIFFERENT tenant must SEE tenant-b's def.
	ctxAdmin := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "ops"})
	ctxAdmin = auth.WithPrincipal(ctxAdmin, auth.Principal{TenantID: "ops", Subject: "op", Scopes: []string{auth.ScopeAdmin}})

	res, _ = tool.Execute(ctxAdmin, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Errorf("admin get of another tenant's def should succeed (admin crosses tenants); got %s", res.Text)
	}
	res, _ = tool.Execute(ctxAdmin, json.RawMessage(`{"op":"list","name":"b-only"}`))
	if res.IsError {
		t.Fatalf("admin list: %s", res.Text)
	}
	if n := len(decodeResult(t, res.Text)["versions"].([]any)); n != 1 {
		t.Errorf("admin list should see tenant-b's version across tenants; got %d", n)
	}

	// Control: a NON-admin principal in "ops" still cannot see tenant-b's def.
	ctxNon := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "ops"})
	ctxNon = auth.WithPrincipal(ctxNon, auth.Principal{TenantID: "ops", Subject: "op", Scopes: []string{auth.ScopeRunsCreate}})
	res, _ = tool.Execute(ctxNon, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if !res.IsError {
		t.Errorf("non-admin cross-tenant get should be refused; got %s", res.Text)
	}
}

// TestAgentDefTool_ForkFromSharedBaseAllowed pins the regression behind
// "job-search-batch never migrated to code-js": the RFC N cross-tenant fork
// guard refused forking the SHARED ("") base, so an authenticated principal
// (legacy "default" or any tenant — never "" once auth is on) could not fork a
// pre-RFC-N / bootstrapped def (which lives at "") to migrate it (e.g. to
// code-js). Forking the shared base must succeed (it lands under the caller's
// tenant); forking ANOTHER specific tenant's private def stays refused.
func TestAgentDefTool_ForkFromSharedBaseAllowed(t *testing.T) {
	tool, baseCtx, cleanup := agentDefFixture(t)
	defer cleanup()

	// A def owned by the SHARED "" tenant (the pre-RFC-N / bootstrapped shape).
	ctxShared := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: ""})
	res, _ := tool.Execute(ctxShared, json.RawMessage(`{"op":"create","name":"shared-base","overlay":{"system_prompt":"v1"}}`))
	if res.IsError {
		t.Fatalf("create shared base: %s", res.Text)
	}
	sharedID := decodeResult(t, res.Text)["def_id"].(string)

	// A "default"-tenant principal forks the shared base by def_id → must
	// SUCCEED (was refused "belongs to another tenant").
	ctxDefault := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "default"})
	res, _ = tool.Execute(ctxDefault, json.RawMessage(`{"op":"fork","name":"shared-base","parent_def_id":"`+sharedID+`","overlay":{"system_prompt":"v2"}}`))
	if res.IsError {
		t.Fatalf(`fork of the shared ("") base as tenant "default" should succeed; got %s`, res.Text)
	}

	// Control: forking ANOTHER specific tenant's PRIVATE def is still refused
	// for a non-admin caller (cross-tenant isolation preserved).
	ctxB := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-b"})
	res, _ = tool.Execute(ctxB, json.RawMessage(`{"op":"create","name":"b-priv","overlay":{"system_prompt":"b"}}`))
	if res.IsError {
		t.Fatalf("create b-priv: %s", res.Text)
	}
	bID := decodeResult(t, res.Text)["def_id"].(string)
	ctxA := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tenant-a"})
	res, _ = tool.Execute(ctxA, json.RawMessage(`{"op":"fork","name":"b-priv","parent_def_id":"`+bID+`","overlay":{"system_prompt":"x"}}`))
	if !res.IsError {
		t.Errorf("forking another tenant's PRIVATE def should still be refused; got %s", res.Text)
	}
}

// Capability-escalation guard on `create`: an agent with narrow
// allowed_tools cannot mint a new agent with a wider tool surface
// than its own. The caller's effective AgentTools(ctx) is the
// ceiling. Mirror of the subset check in `fork`.
func TestAgentDefTool_CreateRefusedOnAllowedToolsWidening(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Caller's effective tools is [Read, AgentDef] only.
	narrowCtx := tools.WithAgentTools(ctx, []string{"Read", "AgentDef"})

	// Overlay tries to add Write — wider than the caller's surface.
	res, _ := tool.Execute(narrowCtx, json.RawMessage(`{"op":"create","name":"newagent","overlay":{"allowed_tools":["Read","Write"]}}`))
	if !res.IsError {
		t.Fatalf("create with wider allowed_tools should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "AllowedTools cannot widen") {
		t.Errorf("error should mention AllowedTools widening; got %s", res.Text)
	}

	// Same overlay but subset of caller's tools is fine.
	res, _ = tool.Execute(narrowCtx, json.RawMessage(`{"op":"create","name":"newagent2","overlay":{"allowed_tools":["Read"]}}`))
	if res.IsError {
		t.Fatalf("create with narrowed allowed_tools should pass; got %s", res.Text)
	}
}

// Wildcard caller tools — used by the substrate-admin HTTP context
// (substrateAdminCtx in internal/api/http/substrate_admin.go) so the
// operator can register agents whose allowed_tools the operator
// chooses, without first listing every per-tool name as their own
// callerTools list. assertAllowedToolsSubset short-circuits on a
// "*" entry in root.
func TestAgentDefTool_CreateWithWildcardCallerToolsAllowsAnyOverlay(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	wildCtx := tools.WithAgentTools(ctx, []string{"*"})

	// Operator picks a broad allowed_tools list — every entry should
	// pass even though "*" alone is the caller's ceiling.
	res, _ := tool.Execute(wildCtx, json.RawMessage(`{"op":"create","name":"opagent","overlay":{"allowed_tools":["Read","Write","WebFetch","Bash"]}}`))
	if res.IsError {
		t.Fatalf("create with wildcard ctx + arbitrary allowed_tools should pass; got %s", res.Text)
	}
}

// Missing AgentTools(ctx) — runtime misconfiguration. With a
// non-empty overlay AllowedTools, refuse rather than silently
// allow the wider value.
func TestAgentDefTool_CreateRefusedWhenCallerToolsMissing(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// ctx does NOT have AgentTools attached. Overlay sets allowed_tools.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"newagent","overlay":{"allowed_tools":["Read"]}}`))
	if !res.IsError {
		t.Fatalf("create with no AgentTools(ctx) + AllowedTools overlay should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "not on ctx") {
		t.Errorf("error should mention missing ctx tools; got %s", res.Text)
	}

	// Create WITHOUT allowed_tools overlay should still pass (no
	// widening risk when the def doesn't declare its own tools).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"toolless"}`))
	if res.IsError {
		t.Fatalf("create with no allowed_tools overlay should pass even without ctx tools; got %s", res.Text)
	}
}

// Documents the v0.8.5 gap: the `descendants` scope is behaviourally
// equivalent to `any` because the tool does not walk the lineage
// graph on every check. This pins the current (undesired) behaviour
// so future tightening triggers a deliberate test update rather than
// silently changing the runtime contract.
func TestAgentDefTool_DescendantsScopeIsCurrentlyEquivalentToAny(t *testing.T) {
	tool, _, cleanup := agentDefFixture(t)
	defer cleanup()
	// Build a ctx with ONLY `descendants` scope (no `any`, no `named:foo`).
	ctx := tools.WithAgentName(context.Background(), "alpha")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"descendants"},
		SelfName: "alpha",
	})
	ctx = tools.WithAgentTools(ctx, []string{"Read"})

	// Mutate a totally unrelated name (would-be cross-tree).
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"completely-unrelated"}`))
	if res.IsError {
		t.Fatalf("descendants scope currently accepts unrelated names by design (v0.8.5 gap); got %s", res.Text)
	}
	// When this test starts failing, descendants has been tightened —
	// update the test and the inline comment in checkScopeForName.
}

// v0.9.x — per-agent max_iterations override on the dynamic AgentDef
// path. The yaml-frontmatter knob shipped in PR #168; this is the
// runtime mirror so agents forking themselves to handle discovery-
// style workloads (1.09M-input runs hitting the 16-iteration cap)
// can tune the limit without an operator yaml round-trip.
func TestAgentDefTool_ForkPersistsMaxIterations(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"max_iterations":64}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	defID, _ := out["def_id"].(string)
	if defID == "" {
		t.Fatal("fork response missing def_id")
	}

	// The tool's `get` response doesn't include the raw definition
	// JSON, so we reach into the Store directly to assert what got
	// persisted.
	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("AgentDefGet: %v", err)
	}
	var defJSON map[string]any
	if err := json.Unmarshal(row.Definition, &defJSON); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	n, ok := defJSON["max_iterations"].(float64)
	if !ok || int(n) != 64 {
		t.Errorf("definition.max_iterations = %v, want 64 (full def: %s)", defJSON["max_iterations"], row.Definition)
	}
}

// Forking without max_iterations in the overlay must NOT leak a
// zero value into the JSON — `omitempty` keeps the row clean so
// applyAgentDefOverlay falls through to the static yaml value (if
// any) rather than overwriting it with 0.
func TestAgentDefTool_ForkWithoutMaxIterationsOmitsField(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"forked"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	defID, _ := out["def_id"].(string)

	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("AgentDefGet: %v", err)
	}
	var defJSON map[string]any
	if err := json.Unmarshal(row.Definition, &defJSON); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	if _, present := defJSON["max_iterations"]; present {
		t.Errorf("max_iterations should be omitted (omitempty) when not in overlay; got %v in %s",
			defJSON["max_iterations"], row.Definition)
	}
}

// TestMergedDef_DriftDetection_VsLookupSubstrateAgentDef closes the
// drift-test gap the code review of PR #188 surfaced: the test in
// internal/lookup/agent_test.go reflects only SubstrateAgentDef. A
// behavioural field added to mergedDef + config.AgentDef but
// accidentally omitted from SubstrateAgentDef would silently leak
// through. This test lives in the builtin package where mergedDef is
// in-scope and reflects BOTH shapes against each other.
//
// Exemptions list non-behavioural fields that mergedDef carries for
// substrate-storage reasons but that SubstrateAgentDef deliberately
// omits because they don't round-trip into config.AgentDef:
//   - description: stored on AgentDefRow, not in config.AgentDef
func TestMergedDef_DriftDetection_VsLookupSubstrateAgentDef(t *testing.T) {
	// Fields in mergedDef that are intentionally NOT mirrored in
	// SubstrateAgentDef. Adding to this set is the conscious decision
	// the test forces — drop a tag here only when you've checked the
	// field genuinely doesn't need to flow into the runtime config.
	exempt := map[string]bool{
		"description": true,
	}

	mergedTags := jsonTagsOfFields(reflect.TypeOf(mergedDef{}))
	substrateTags := jsonTagsOfFields(reflect.TypeOf(lookup.SubstrateAgentDef{}))

	for tag := range mergedTags {
		if exempt[tag] {
			continue
		}
		if !substrateTags[tag] {
			t.Errorf("mergedDef has json tag %q but lookup.SubstrateAgentDef does not — either mirror it on the lookup side OR add %q to the exempt set with a comment justifying why it stays substrate-internal",
				tag, tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateAgentDef has json tag %q but mergedDef does not — substrate persistence is the source-of-truth shape; remove from SubstrateAgentDef OR add the field to mergedDef in this package",
				tag)
		}
	}
}

// jsonTagsOfFields walks a struct's exported fields and returns the
// set of json tag names (the part before any "," for `,omitempty`).
// Mirrors the helper in internal/lookup/agent_test.go; duplicated
// because that one is _test.go scoped and not importable.
func jsonTagsOfFields(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		out[tag] = true
	}
	return out
}

// TestAgentDefTool_ListIncludesDefinition pins the v0.9.x Library v2
// contract: the list op's per-version rows include the persisted
// `definition` JSON so the UI can render content inline without a
// second round-trip. Before this fix, rowResponseMap omitted the
// definition field — `row.definition` was undefined on the wire,
// every inline-content expansion rendered empty.
func TestAgentDefTool_ListIncludesDefinition(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentTools(ctx, []string{"Read"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"with-body","overlay":{"system_prompt":"hello world","allowed_tools":["Read"]},"promote":true}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"with-body"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	versions, _ := out["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	v0, _ := versions[0].(map[string]any)
	if v0["definition"] == nil {
		t.Fatalf("list response missing definition field: %v", v0)
	}
	// definition rides through as embedded JSON bytes; on the wire it
	// surfaces as a JSON-decoded object. Either shape is acceptable —
	// what matters is that the field is non-null.
}
