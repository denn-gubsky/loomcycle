package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func loadTestHelpSet(t *testing.T) (*help.Set, error) {
	t.Helper()
	return help.LoadSet("")
}

// fakeTool is a minimal tools.Tool used for the Context tool's
// catalog tests. Name + Description + InputSchema are caller-supplied;
// Execute is a no-op (Context never calls Execute on its catalog).
type fakeTool struct {
	NameVal   string
	DescVal   string
	SchemaVal string
}

func (f *fakeTool) Name() string                 { return f.NameVal }
func (f *fakeTool) Description() string          { return f.DescVal }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(f.SchemaVal) }
func (f *fakeTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{}, nil
}

func contextFixture(t *testing.T) (*Context, context.Context) {
	t.Helper()
	tool := &Context{
		Tools: []tools.Tool{
			&fakeTool{NameVal: "Read", DescVal: "Read a file", SchemaVal: `{"type":"object"}`},
			&fakeTool{NameVal: "Memory", DescVal: "Persistent KV", SchemaVal: `{"type":"object"}`},
			&fakeTool{NameVal: "Context", DescVal: "Introspect", SchemaVal: `{"type":"object"}`},
			&fakeTool{NameVal: "Bash", DescVal: "Run shell", SchemaVal: `{"type":"object"}`},
			&fakeTool{NameVal: "mcp__jobs__patchApp", DescVal: "MCP", SchemaVal: `{"type":"object"}`},
		},
	}
	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		AgentID:    "a_test",
		UserID:     "alice",
		UserTier:   "medium",
		AgentDefID: "def_abc",
	})
	ctx = tools.WithRunID(ctx, "r_test123")
	ctx = tools.WithResolvedProvider(ctx, "anthropic")
	ctx = tools.WithResolvedModel(ctx, "claude-opus-4-test")
	temp := 0.7
	ctx = tools.WithResolvedSampling(ctx, &config.Sampling{Temperature: &temp})
	ctx = tools.WithAgentTools(ctx, []string{"Read", "Memory", "Context", "mcp__jobs__patchApp"})
	return tool, ctx
}

// ---- self ----

func TestContextTool_SelfReturnsIdentity(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if res.IsError {
		t.Fatalf("self: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["agent_name"] != "researcher" {
		t.Errorf("agent_name = %v, want researcher", out["agent_name"])
	}
	if out["agent_id"] != "a_test" {
		t.Errorf("agent_id = %v, want a_test", out["agent_id"])
	}
	if out["user_id"] != "alice" {
		t.Errorf("user_id = %v, want alice", out["user_id"])
	}
	if out["user_tier"] != "medium" {
		t.Errorf("user_tier = %v, want medium", out["user_tier"])
	}
	// v0.8.7 PR 1 review fix: agent_def_id surfaced from RunIdentity.
	if out["agent_def_id"] != "def_abc" {
		t.Errorf("agent_def_id = %v, want def_abc", out["agent_def_id"])
	}
	// v0.12.x — run_id surfaced so an agent can pass its own run_id
	// through Channel messages, enabling cross-agent Evaluation.submit
	// (which requires run_id, not agent_id) without a separate lookup.
	if out["run_id"] != "r_test123" {
		t.Errorf("run_id = %v, want r_test123", out["run_id"])
	}
	// Resolved provider + model — non-secret introspection so the agent
	// knows what it is running on.
	if out["provider"] != "anthropic" {
		t.Errorf("provider = %v, want anthropic", out["provider"])
	}
	if out["model"] != "claude-opus-4-test" {
		t.Errorf("model = %v, want claude-opus-4-test", out["model"])
	}
	// sampling: the resolved LLM params surface so a self-evolving agent can
	// read its own temperature. Decoded JSON → map with "temperature": 0.7.
	samp, ok := out["sampling"].(map[string]any)
	if !ok {
		t.Fatalf("sampling = %v (%T), want an object", out["sampling"], out["sampling"])
	}
	if samp["temperature"] != 0.7 {
		t.Errorf("sampling.temperature = %v, want 0.7", samp["temperature"])
	}
}

// TestContextTool_SelfOmitsSamplingWhenUnset: no sampling stamped → no
// "sampling" key (the agent sees provider defaults; the key isn't a misleading
// empty object).
func TestContextTool_SelfOmitsSamplingWhenUnset(t *testing.T) {
	tool := &Context{}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_x"})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	out := decodeResult(t, res.Text)
	if _, present := out["sampling"]; present {
		t.Errorf("sampling key present (%v) with no sampling configured — want omitted", out["sampling"])
	}
	if _, present := out["compaction"]; present {
		t.Errorf("compaction key present (%v) with none configured — want omitted", out["compaction"])
	}
}

// op=self reports the resolved compaction settings when configured (so an agent
// can decide whether to self-compact).
func TestContextTool_SelfReportsCompaction(t *testing.T) {
	tool := &Context{}
	enabled := true
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_x"})
	ctx = tools.WithCompactionPolicy(ctx, &config.Compaction{Enabled: &enabled})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	out := decodeResult(t, res.Text)
	if _, present := out["compaction"]; !present {
		t.Error("compaction key missing from op=self with a configured policy")
	}
}

// op=compact sets the loop's compact-request flag; without one wired it errors.
func TestContextTool_CompactSetsFlag(t *testing.T) {
	tool := &Context{}
	var flag atomic.Bool
	ctx := tools.WithCompactRequest(context.Background(), &flag)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"compact"}`))
	if res.IsError {
		t.Fatalf("op=compact errored with a flag wired: %s", res.Text)
	}
	if !flag.Load() {
		t.Error("op=compact did not set the compact-request flag")
	}
	// No flag on ctx → not available → error.
	res2, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"compact"}`))
	if !res2.IsError {
		t.Error("op=compact should error when compaction isn't available for the run")
	}
}

// Provider/model are present-as-empty (not missing keys) when the run was
// started outside the loop's stamping path — mirrors the agent_def_id
// surfaced-even-when-empty contract so callers can rely on the shape.
func TestContextTool_SelfProviderModelEmptyWhenUnset(t *testing.T) {
	tool := &Context{}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_x"})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	out := decodeResult(t, res.Text)
	if v, ok := out["provider"]; !ok || v != "" {
		t.Errorf("provider = %v (ok=%v), want present empty string", v, ok)
	}
	if v, ok := out["model"]; !ok || v != "" {
		t.Errorf("model = %v (ok=%v), want present empty string", v, ok)
	}
}

// PR 1 review fix: empty AgentDefID (static-resolved run, no pin)
// surfaces as empty string, not as a missing key.
func TestContextTool_SelfEmptyAgentDefIDStillSurfaced(t *testing.T) {
	tool := &Context{}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID: "a_static",
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	out := decodeResult(t, res.Text)
	if _, ok := out["agent_def_id"]; !ok {
		t.Error("agent_def_id key missing — should be present (empty string) for static runs")
	}
	if out["agent_def_id"] != "" {
		t.Errorf("agent_def_id = %v, want empty string", out["agent_def_id"])
	}
}

func TestContextTool_SelfWithEmptyCtxReturnsZeros(t *testing.T) {
	tool := &Context{}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"self"}`))
	if res.IsError {
		t.Fatalf("self: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["agent_name"] != "" {
		t.Errorf("agent_name = %v, want empty", out["agent_name"])
	}
}

// ---- tools ----

func TestContextTool_ToolsFiltersByAgentToolsList(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"tools"}`))
	if res.IsError {
		t.Fatalf("tools: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	// 4 of the 5 fake tools are in AgentTools (Bash is excluded).
	if out["count"].(float64) != 4 {
		t.Errorf("count = %v, want 4 (Bash excluded by ctx allowlist)", out["count"])
	}
	got := out["tools"].([]any)
	gotNames := map[string]bool{}
	for _, e := range got {
		gotNames[e.(map[string]any)["name"].(string)] = true
	}
	if gotNames["Bash"] {
		t.Error("Bash should be filtered out (not in AgentTools ctx)")
	}
	if !gotNames["Read"] || !gotNames["Memory"] || !gotNames["Context"] || !gotNames["mcp__jobs__patchApp"] {
		t.Errorf("missing expected tool in catalog: %v", gotNames)
	}
}

func TestContextTool_ToolsSortedByName(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"tools"}`))
	out := decodeResult(t, res.Text)
	got := out["tools"].([]any)
	var prev string
	for _, e := range got {
		name := e.(map[string]any)["name"].(string)
		if prev != "" && name < prev {
			t.Errorf("tools not sorted: %q after %q", name, prev)
		}
		prev = name
	}
}

func TestContextTool_ToolsSideEffectClass(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"tools"}`))
	out := decodeResult(t, res.Text)
	classes := map[string]string{}
	for _, e := range out["tools"].([]any) {
		m := e.(map[string]any)
		classes[m["name"].(string)] = m["side_effect_class"].(string)
	}
	if classes["Read"] != "filesystem" {
		t.Errorf("Read class = %q, want filesystem", classes["Read"])
	}
	if classes["Memory"] != "state" {
		t.Errorf("Memory class = %q, want state", classes["Memory"])
	}
	if classes["Context"] != "pure" {
		t.Errorf("Context class = %q, want pure", classes["Context"])
	}
	if classes["mcp__jobs__patchApp"] != "unknown" {
		t.Errorf("mcp__ class = %q, want unknown", classes["mcp__jobs__patchApp"])
	}
}

func TestContextTool_ToolsWithoutCtxShowsAll(t *testing.T) {
	// No WithAgentTools attached → fallback shows the full c.Tools
	// catalog. Useful for unit tests and dev introspection.
	tool := &Context{Tools: []tools.Tool{
		&fakeTool{NameVal: "Read", DescVal: "x", SchemaVal: `{}`},
		&fakeTool{NameVal: "Bash", DescVal: "x", SchemaVal: `{}`},
	}}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"tools"}`))
	out := decodeResult(t, res.Text)
	if out["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2 (no ctx filter)", out["count"])
	}
}

// ---- doc ----

func TestContextTool_DocReturnsFullSchema(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"doc","name":"Read"}`))
	if res.IsError {
		t.Fatalf("doc: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "Read" {
		t.Errorf("name = %v, want Read", out["name"])
	}
	if out["description"] != "Read a file" {
		t.Errorf("description = %v, want \"Read a file\"", out["description"])
	}
	if out["input_schema"] == nil {
		t.Error("input_schema missing")
	}
}

func TestContextTool_DocMissingName(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"doc"}`))
	if !res.IsError {
		t.Fatalf("doc without name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "name") {
		t.Errorf("error should mention name; got %q", res.Text)
	}
}

func TestContextTool_DocUnknownTool(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"doc","name":"Nonexistent"}`))
	if !res.IsError {
		t.Fatalf("doc for unknown tool should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "not found") {
		t.Errorf("error should mention not found; got %q", res.Text)
	}
}

func TestContextTool_DocOutsideAllowlist(t *testing.T) {
	// Tool exists in the catalog but isn't in the ctx allowlist.
	// Should refuse rather than leak the docs.
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"doc","name":"Bash"}`))
	if !res.IsError {
		t.Fatalf("doc for out-of-allowlist tool should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "allowed_tools") {
		t.Errorf("error should mention allowed_tools; got %q", res.Text)
	}
}

// ---- permissions ----

func TestContextTool_PermissionsBundle(t *testing.T) {
	tool, ctx := contextFixture(t)
	// Attach a host policy + memory policy so they appear in the output.
	ctx = tools.WithHostPolicy(ctx, tools.HostPolicyValue{
		AllowedHosts: []string{"api.example.com"},
		HasList:      true,
	})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
		QuotaBytes:    1024,
	})
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"findings"},
		Subscribe: []string{"findings"},
	})
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"self"},
		SelfName: "researcher",
	})
	ctx = tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{
		Scopes: []string{"named:karpathy-guidelines"},
	})
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{
		Scopes: []string{"submit_self", "read_any"},
	})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"permissions"}`))
	if res.IsError {
		t.Fatalf("permissions: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if got := out["allowed_tools"].([]any); len(got) != 4 {
		t.Errorf("allowed_tools len = %d, want 4", len(got))
	}
	if hp := out["host_policy"].(map[string]any); hp["has_list"].(bool) != true {
		t.Error("host_policy.has_list != true")
	}
	if mem := out["memory"].(map[string]any); mem["quota_bytes"].(float64) != 1024 {
		t.Errorf("memory.quota_bytes = %v, want 1024", mem["quota_bytes"])
	}
	if ch := out["channels"].(map[string]any); len(ch["publish"].([]any)) != 1 {
		t.Errorf("channels.publish len = %d, want 1", len(ch["publish"].([]any)))
	}
	if adScopes := out["agent_def_scopes"].([]any); adScopes[0] != "self" {
		t.Errorf("agent_def_scopes[0] = %v, want self", adScopes[0])
	}
	if sdScopes := out["skill_def_scopes"].([]any); sdScopes[0] != "named:karpathy-guidelines" {
		t.Errorf("skill_def_scopes[0] = %v, want named:karpathy-guidelines", sdScopes[0])
	}
}

// ---- dispatch ----

func TestContextTool_UnknownOp(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"frob"}`))
	if !res.IsError {
		t.Fatalf("unknown op should refuse; got %s", res.Text)
	}
}

func TestContextTool_MissingOp(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("missing op should refuse; got %s", res.Text)
	}
}

func TestContextTool_MalformedJSON(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{not json`))
	if !res.IsError {
		t.Fatal("malformed JSON should error")
	}
}

// ---- agents / lineage / evaluations fixtures + tests (PR 2) ----

// substrateFixture builds a Context tool with Cfg + an in-memory
// sqlite store seeded with two agent_defs rows (a v1 + a v2-child)
// for a per-test unique name, and one Evaluation row against v2.
//
// The `:memory:` sqlite DSN uses `cache=shared`, so every Open returns
// the same physical database within one test process — subtests share
// state. Defeating that requires either a per-test file:: DSN or
// per-test unique keys. We pick unique keys (per t.Name()) so the
// fixture remains race-clean across subtests without filesystem
// shenanigans.
func substrateFixture(t *testing.T) (*Context, store.Store, context.Context, string, string, string) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	// `:memory:` with cache=shared persists for the lifetime of any
	// open connection — without explicit Close, later tests that
	// reopen `:memory:` see stale rows. Registering the close on
	// t.Cleanup ensures the connection pool drains at test exit.
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Per-test unique IDs so subtests don't collide on the shared
	// in-memory DB. t.Name() is unique per subtest.
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	suffix = strings.ReplaceAll(suffix, "TestContextTool_", "")
	agentName := "researcher_" + suffix
	v1ID := "def_v1_" + suffix
	v2ID := "def_v2_" + suffix

	v1, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:      v1ID,
		Name:       agentName,
		Definition: json.RawMessage(`{"system_prompt":"v1"}`),
	})
	if err != nil {
		t.Fatalf("AgentDefCreate v1: %v", err)
	}
	v2, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:       v2ID,
		Name:        agentName,
		ParentDefID: v1.DefID,
		Definition:  json.RawMessage(`{"system_prompt":"v2"}`),
		Description: "experimental",
	})
	if err != nil {
		t.Fatalf("AgentDefCreate v2: %v", err)
	}
	if err := s.AgentDefSetActive(ctx, "", agentName, v2.DefID, ""); err != nil {
		t.Fatalf("AgentDefSetActive: %v", err)
	}

	sess, _ := s.CreateSession(ctx, "t", agentName, "alice")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_eval", AgentDefID: v2.DefID})
	_, _ = s.EvaluationSubmit(ctx, store.EvaluationRow{
		EvalID:      "eval_" + suffix,
		RunID:       run.ID,
		DefID:       v2.DefID,
		Score:       0.8,
		EmitterRole: "self",
	})

	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			agentName: {
				Tier:         "low",
				Provider:     "anthropic",
				Model:        "claude-haiku-4-5",
				AllowedTools: []string{"Read", "Context"},
			},
			"qa_" + suffix: {
				Tier:         "middle",
				AllowedTools: []string{"Read"},
			},
		},
	}
	tool := &Context{
		Tools: []tools.Tool{
			&fakeTool{NameVal: "Read", DescVal: "Read a file", SchemaVal: `{}`},
			&fakeTool{NameVal: "Context", DescVal: "Introspect", SchemaVal: `{}`},
		},
		Cfg:   cfg,
		Store: s,
	}
	runCtx := tools.WithAgentName(context.Background(), agentName)
	runCtx = tools.WithRunIdentity(runCtx, tools.RunIdentityValue{AgentID: "a_test", UserID: "alice"})
	runCtx = tools.WithAgentTools(runCtx, []string{"Read", "Context"})
	return tool, s, runCtx, agentName, v1.DefID, v2.DefID
}

// ---- agents ----

func TestContextTool_AgentsListsAll(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"agents"}`))
	if res.IsError {
		t.Fatalf("agents: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2 (researcher + qa)", out["count"])
	}
	got := out["agents"].([]any)
	// Sorted alphabetically; qa_ prefix sorts before researcher_.
	first := got[0].(map[string]any)
	if !strings.HasPrefix(first["name"].(string), "qa_") {
		t.Errorf("first agent = %q, want qa_* prefix", first["name"])
	}
}

func TestContextTool_AgentsPrefixFilter(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"agents","prefix":"researcher_"}`))
	out := decodeResult(t, res.Text)
	if out["count"].(float64) != 1 {
		t.Errorf("prefix=researcher_ count = %v, want 1", out["count"])
	}
	first := out["agents"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(first["name"].(string), "researcher_") {
		t.Errorf("name = %q, want researcher_ prefix", first["name"])
	}
}

func TestContextTool_AgentsIncludesActiveDefID(t *testing.T) {
	tool, _, ctx, _, _, v2ID := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"agents","prefix":"researcher_"}`))
	out := decodeResult(t, res.Text)
	first := out["agents"].([]any)[0].(map[string]any)
	if first["active_def_id"] != v2ID {
		t.Errorf("active_def_id = %v, want %s", first["active_def_id"], v2ID)
	}
}

func TestContextTool_AgentsRefusesWithoutCfg(t *testing.T) {
	tool := &Context{} // no Cfg
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"agents"}`))
	if !res.IsError {
		t.Fatal("agents without Cfg should refuse")
	}
}

// ---- lineage ----

func TestContextTool_LineageWalksAncestorsAndDescendants(t *testing.T) {
	tool, _, ctx, _, v1ID, v2ID := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"lineage","def_id":"`+v1ID+`"}`))
	if res.IsError {
		t.Fatalf("lineage: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	root := out["root"].(map[string]any)
	if root["def_id"] != v1ID {
		t.Errorf("root.def_id = %v, want %s", root["def_id"], v1ID)
	}
	// v1 has no ancestors (it's the root).
	if anc := out["ancestors"]; anc != nil {
		if a, _ := anc.([]any); len(a) != 0 {
			t.Errorf("ancestors = %v, want []", a)
		}
	}
	// v1 has one descendant: v2.
	desc := out["descendants"].([]any)
	if len(desc) != 1 {
		t.Fatalf("descendants len = %d, want 1", len(desc))
	}
	if d := desc[0].(map[string]any); d["def_id"] != v2ID {
		t.Errorf("descendant.def_id = %v, want %s", d["def_id"], v2ID)
	}
}

func TestContextTool_LineageFromChildShowsAncestor(t *testing.T) {
	tool, _, ctx, _, v1ID, v2ID := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"lineage","def_id":"`+v2ID+`"}`))
	out := decodeResult(t, res.Text)
	anc := out["ancestors"].([]any)
	if len(anc) != 1 {
		t.Fatalf("ancestors len = %d, want 1", len(anc))
	}
	if a := anc[0].(map[string]any); a["def_id"] != v1ID {
		t.Errorf("ancestor.def_id = %v, want %s", a["def_id"], v1ID)
	}
}

func TestContextTool_LineageMissingDefID(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"lineage"}`))
	if !res.IsError {
		t.Fatal("lineage without def_id should refuse")
	}
	if !strings.Contains(res.Text, "def_id") {
		t.Errorf("error should mention def_id; got %q", res.Text)
	}
}

func TestContextTool_LineageUnknownDefID(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"lineage","def_id":"def_nope_does_not_exist"}`))
	if !res.IsError {
		t.Fatal("lineage for unknown def_id should refuse")
	}
	if !strings.Contains(res.Text, "not found") {
		t.Errorf("error should mention not found; got %q", res.Text)
	}
}

// PR 2 review fix: lineage BFS caps total node count (not just
// depth) so a high-fan-out lineage doesn't blow up the response.
// Seed > maxDescendants children and verify truncated=true.
func TestContextTool_LineageTruncatesAtNodeCap(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")

	root, _ := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:      "def_root_" + suffix,
		Name:       "fanout_" + suffix,
		Definition: json.RawMessage(`{}`),
	})
	// Create > 500 children (the maxDescendants cap). Just barely
	// over the cap is enough to trigger truncation.
	for i := 0; i < 510; i++ {
		_, err := s.AgentDefCreate(ctx, store.AgentDefRow{
			DefID:       fmt.Sprintf("def_child_%d_%s", i, suffix),
			Name:        "fanout_" + suffix,
			ParentDefID: root.DefID,
			Definition:  json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("seed child %d: %v", i, err)
		}
	}

	tool := &Context{Cfg: &config.Config{}, Store: s}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"lineage","def_id":"`+root.DefID+`"}`))
	if res.IsError {
		t.Fatalf("lineage: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	desc := out["descendants"].([]any)
	if len(desc) > 500 {
		t.Errorf("descendants len = %d, want <= 500 (the cap)", len(desc))
	}
	if !out["truncated"].(bool) {
		t.Error("truncated should be true when cap hit")
	}
}

func TestContextTool_LineageRefusesWithoutStore(t *testing.T) {
	tool := &Context{Cfg: &config.Config{}} // no Store
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"lineage","def_id":"x"}`))
	if !res.IsError {
		t.Fatal("lineage without Store should refuse")
	}
}

// ---- evaluations ----

func TestContextTool_EvaluationsAggregate(t *testing.T) {
	tool, _, ctx, _, _, v2ID := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"evaluations","def_id":"`+v2ID+`"}`))
	if res.IsError {
		t.Fatalf("evaluations: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["def_id"] != v2ID {
		t.Errorf("def_id = %v, want %s", out["def_id"], v2ID)
	}
	if out["count"].(float64) != 1 {
		t.Errorf("count = %v, want 1", out["count"])
	}
	score := out["score"].(map[string]any)
	if score["mean"].(float64) != 0.8 {
		t.Errorf("mean = %v, want 0.8", score["mean"])
	}
}

func TestContextTool_EvaluationsEmptyDef(t *testing.T) {
	tool, _, ctx, _, v1ID, _ := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"evaluations","def_id":"`+v1ID+`"}`))
	if res.IsError {
		t.Fatalf("evaluations: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["count"].(float64) != 0 {
		t.Errorf("count = %v, want 0 (no evaluations for v1)", out["count"])
	}
}

func TestContextTool_EvaluationsMissingDefID(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"evaluations"}`))
	if !res.IsError {
		t.Fatal("evaluations without def_id should refuse")
	}
}

func TestContextTool_EvaluationsRefusesWithoutStore(t *testing.T) {
	tool := &Context{Cfg: &config.Config{}} // no Store
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"evaluations","def_id":"x"}`))
	if !res.IsError {
		t.Fatal("evaluations without Store should refuse")
	}
}

// ---- channels / history (PR 3) ----

func TestContextTool_ChannelsListsAccessible(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	chCtx := tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"findings", "findings/*"},
		Subscribe: []string{"findings", "alerts"},
		Channels: map[string]tools.ChannelDef{
			"findings": {Name: "findings", Scope: "agent", Semantic: "queue"},
			"alerts":   {Name: "alerts", Scope: "global", DefaultTTL: 3600},
		},
	})
	res, _ := tool.Execute(chCtx, json.RawMessage(`{"op":"channels"}`))
	if res.IsError {
		t.Fatalf("channels: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2", out["count"])
	}
	// Wildcards surface separately.
	w := out["publish_wildcards"].([]any)
	if len(w) != 1 || w[0].(string) != "findings/*" {
		t.Errorf("publish_wildcards = %v, want [findings/*]", w)
	}
	// Per-channel bools.
	got := out["channels"].([]any)
	for _, c := range got {
		m := c.(map[string]any)
		switch m["name"].(string) {
		case "findings":
			if !m["publish"].(bool) || !m["subscribe"].(bool) {
				t.Errorf("findings: publish=%v subscribe=%v, want true/true", m["publish"], m["subscribe"])
			}
		case "alerts":
			if m["publish"].(bool) || !m["subscribe"].(bool) {
				t.Errorf("alerts: publish=%v subscribe=%v, want false/true", m["publish"], m["subscribe"])
			}
		}
	}
}

func TestContextTool_HistorySelfScope(t *testing.T) {
	tool, s, ctx, agentName, _, _ := substrateFixture(t)
	// Seed a real run + an event under THIS caller's agent_id.
	sess, _ := s.CreateSession(context.Background(), "t", agentName, "alice")
	run, _ := s.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_caller", UserID: "alice"})
	_ = s.AppendEvent(context.Background(), run.ID, "text", []byte(`{"text":"hello"}`))

	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_caller", UserID: "alice"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"self"}})

	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history"}`))
	if res.IsError {
		t.Fatalf("history: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["agent_id"] != "a_caller" {
		t.Errorf("agent_id = %v, want a_caller", out["agent_id"])
	}
	if count := out["count"].(float64); count < 1 {
		t.Errorf("count = %v, want >= 1", count)
	}
}

func TestContextTool_HistoryRefusesOtherAgentUnderSelfScope(t *testing.T) {
	tool, s, ctx, agentName, _, _ := substrateFixture(t)
	sess, _ := s.CreateSession(context.Background(), "t", agentName, "alice")
	_, _ = s.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_other", UserID: "alice"})

	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_caller"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"self"}})

	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history","agent_id":"a_other"}`))
	if !res.IsError {
		t.Fatalf("history of other agent under self scope should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "scope check") {
		t.Errorf("error should mention scope check; got %q", res.Text)
	}
}

func TestContextTool_HistoryAnyScopeAllowsOther(t *testing.T) {
	tool, s, ctx, agentName, _, _ := substrateFixture(t)
	sess, _ := s.CreateSession(context.Background(), "t", agentName, "alice")
	run, _ := s.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_other", UserID: "alice"})
	_ = s.AppendEvent(context.Background(), run.ID, "text", []byte(`{"text":"hi"}`))

	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_caller"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"any"}})

	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history","agent_id":"a_other"}`))
	if res.IsError {
		t.Fatalf("history under `any` scope should succeed; got %s", res.Text)
	}
}

// TestContextTool_HistorySinceTsFiltersOlder pins the v0.8.17 PR 3.5
// addendum: an RFC3339 since_ts filters out events older than the
// timestamp. Two events seeded — one before since_ts, one after —
// only the recent one appears in the result.
func TestContextTool_HistorySinceTsFiltersOlder(t *testing.T) {
	tool, s, ctx, agentName, _, _ := substrateFixture(t)
	bg := context.Background()
	sess, _ := s.CreateSession(bg, "t", agentName, "alice")
	run, _ := s.CreateRun(bg, sess.ID, store.RunIdentity{AgentID: "a_caller", UserID: "alice"})

	// First event NOW; second event 100ms later. The since_ts will
	// be 50ms after the first so the filter excludes it.
	_ = s.AppendEvent(bg, run.ID, "text", []byte(`{"text":"old"}`))
	t0 := time.Now()
	time.Sleep(100 * time.Millisecond)
	_ = s.AppendEvent(bg, run.ID, "text", []byte(`{"text":"new"}`))

	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_caller", UserID: "alice"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"self"}})

	// since_ts at t0+50ms — between the two events.
	since := t0.Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"op":"history","since_ts":%q}`, since)
	res, _ := tool.Execute(histCtx, json.RawMessage(body))
	if res.IsError {
		t.Fatalf("history with since_ts: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	count := out["count"].(float64)
	if count != 1 {
		t.Errorf("count = %v, want 1 (older event excluded by since_ts)", count)
	}
}

// TestContextTool_HistorySinceTsInvalidFormat — bad RFC3339 string
// returns a clear error.
func TestContextTool_HistorySinceTsInvalidFormat(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_caller"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"self"}})
	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history","since_ts":"not-a-date"}`))
	if !res.IsError {
		t.Fatal("history with bad since_ts should error")
	}
	if !strings.Contains(res.Text, "RFC3339") {
		t.Errorf("error should mention RFC3339; got %q", res.Text)
	}
}

func TestContextTool_HistoryRefusesEmptyScopes(t *testing.T) {
	tool, _, ctx, _, _, _ := substrateFixture(t)
	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_caller"})
	// No WithHistoryPolicy attached.
	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history"}`))
	if !res.IsError {
		t.Fatal("history without any scope should refuse (default-deny)")
	}
}

func TestContextTool_HistoryRefusesWithoutStore(t *testing.T) {
	tool := &Context{}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a"})
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"any"}})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"history"}`))
	if !res.IsError {
		t.Fatal("history without Store should refuse")
	}
}

// ---- permissions surfaces history_scope ----

func TestContextTool_PermissionsSurfacesHistoryScope(t *testing.T) {
	tool, ctx := contextFixture(t)
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: []string{"self", "any"}})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"permissions"}`))
	out := decodeResult(t, res.Text)
	scopes := out["history_scope"].([]any)
	if len(scopes) != 2 || scopes[0] != "self" || scopes[1] != "any" {
		t.Errorf("history_scope = %v, want [self any]", scopes)
	}
}

// PR 3 review fix: truncated must be true ONLY when there are more
// filter-matching events than the limit allows. Old code compared
// limit to raw transcript size — false positive when event_types
// filter excluded enough events that matchCount <= limit.
func TestContextTool_HistoryTruncatedRespectsTypeFilter(t *testing.T) {
	tool, s, ctx, agentName, _, _ := substrateFixture(t)
	sess, _ := s.CreateSession(context.Background(), "t", agentName, "alice")
	run, _ := s.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_filtered", UserID: "alice"})
	// Mix: 3 text events + 50 usage events. With a `text` filter +
	// limit=10, only 3 events match — truncated MUST be false.
	for i := 0; i < 3; i++ {
		_ = s.AppendEvent(context.Background(), run.ID, "text", []byte(`{"text":"hi"}`))
	}
	for i := 0; i < 50; i++ {
		_ = s.AppendEvent(context.Background(), run.ID, "usage", []byte(`{"tokens":1}`))
	}

	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_filtered"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"any"}})

	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history","agent_id":"a_filtered","event_types":["text"],"limit":10}`))
	if res.IsError {
		t.Fatalf("history: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if c := out["count"].(float64); c != 3 {
		t.Errorf("count = %v, want 3 (only text events match)", c)
	}
	if out["truncated"].(bool) {
		t.Error("truncated = true; want false (only 3 matching events, all returned)")
	}
}

// Same fixture but with limit=2 — now 3 matches exceeds limit, so
// truncated MUST be true.
func TestContextTool_HistoryTruncatedTrueWhenMatchesExceedLimit(t *testing.T) {
	tool, s, ctx, agentName, _, _ := substrateFixture(t)
	sess, _ := s.CreateSession(context.Background(), "t", agentName, "alice")
	run, _ := s.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_match", UserID: "alice"})
	for i := 0; i < 3; i++ {
		_ = s.AppendEvent(context.Background(), run.ID, "text", []byte(`{"text":"hi"}`))
	}

	histCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_match"})
	histCtx = tools.WithHistoryPolicy(histCtx, tools.HistoryPolicyValue{Scopes: []string{"any"}})

	res, _ := tool.Execute(histCtx, json.RawMessage(`{"op":"history","agent_id":"a_match","event_types":["text"],"limit":2}`))
	out := decodeResult(t, res.Text)
	if c := out["count"].(float64); c != 2 {
		t.Errorf("count = %v, want 2 (limit)", c)
	}
	if !out["truncated"].(bool) {
		t.Error("truncated = false; want true (3 matches > limit 2)")
	}
}

// ---- help ----

func TestContextTool_HelpRefusesWithoutHelpRegistry(t *testing.T) {
	tool, ctx := contextFixture(t)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"help"}`))
	if !res.IsError {
		t.Fatalf("help with nil Help should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "not configured") {
		t.Errorf("error text = %q, want 'not configured'", res.Text)
	}
}

func TestContextTool_HelpIndexListsAllTopics(t *testing.T) {
	tool, ctx := contextFixture(t)
	helpSet, err := loadTestHelpSet(t)
	if err != nil {
		t.Fatalf("loadTestHelpSet: %v", err)
	}
	tool.Help = helpSet
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"help"}`))
	if res.IsError {
		t.Fatalf("help index: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	topics, ok := out["topics"].([]any)
	if !ok || len(topics) == 0 {
		t.Fatalf("topics missing/empty: %v", out)
	}
	first := topics[0].(map[string]any)
	if first["name"] == "" || first["description"] == "" {
		t.Errorf("topic entry missing name/description: %v", first)
	}
	if _, has := first["content"]; has {
		t.Error("index entries must NOT include content")
	}
	if out["hint"] == "" {
		t.Error("hint missing from index response")
	}
}

func TestContextTool_HelpDetailReturnsContent(t *testing.T) {
	tool, ctx := contextFixture(t)
	helpSet, err := loadTestHelpSet(t)
	if err != nil {
		t.Fatalf("loadTestHelpSet: %v", err)
	}
	tool.Help = helpSet
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"help","topic":"scopes"}`))
	if res.IsError {
		t.Fatalf("help detail: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "scopes" {
		t.Errorf("name = %v, want scopes", out["name"])
	}
	content, _ := out["content"].(string)
	if !strings.Contains(content, "scope") {
		t.Errorf("content didn't include scope topic body: %q", content)
	}
}

func TestContextTool_HelpUnknownTopic(t *testing.T) {
	tool, ctx := contextFixture(t)
	helpSet, err := loadTestHelpSet(t)
	if err != nil {
		t.Fatalf("loadTestHelpSet: %v", err)
	}
	tool.Help = helpSet
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"help","topic":"does-not-exist"}`))
	if !res.IsError {
		t.Fatalf("unknown topic should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "available:") {
		t.Errorf("error should list available topics: %q", res.Text)
	}
}
