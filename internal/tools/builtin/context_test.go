package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
		AgentID:  "a_test",
		UserID:   "alice",
		UserTier: "medium",
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
