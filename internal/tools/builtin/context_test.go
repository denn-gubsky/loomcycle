package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

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
	if err := s.AgentDefSetActive(ctx, agentName, v2.DefID, ""); err != nil {
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
