package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

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
