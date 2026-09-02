package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// largeCodeBody returns compilable code-js source of roughly n bytes, sized to
// land between the shipped MaxDefinitionBytes (128 KiB) and MaxCodeBytes
// (256 KiB) defaults — the dead zone the shipped memory/consolidator occupies.
func largeCodeBody(n int) string {
	return "// " + strings.Repeat("x", n) + "\nfunction run() { return {}; }\n"
}

// codeAgentFixture mirrors agentDefFixture but the operator-blessed root is a
// code-js agent with a body larger than MaxDefinitionBytes, as the shipped
// memory bundle's consolidator is.
func codeAgentFixture(t *testing.T) (*AgentDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"memory/consolidator": {
				Provider:     "code-js",
				Model:        "memory/consolidator",
				Code:         largeCodeBody(140_000),
				Tools:        []string{"Memory", "Document", "AgentDef"},
				MemoryScopes: []string{"agent", "user"},
			},
		},
	}
	cfg.Env.CodeAgentsEnabled = true
	tool := &AgentDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
		MaxCodeBytes:        262144,
	}
	ctx := tools.WithAgentName(context.Background(), "memory/consolidator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "memory/consolidator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

// A capability-only overlay on a large code-js agent must be accepted. The
// inherited body is governed by MaxCodeBytes, which is deliberately larger;
// charging it to MaxDefinitionBytes too made overriding such an agent
// impossible, whatever the overlay actually changed.
func TestAgentDefTool_ForkOverridesGrantsOnALargeCodeAgent(t *testing.T) {
	tool, ctx, cleanup := codeAgentFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"memory/consolidator","overlay":{"memory_scopes":["agent","user","tenant"],"sql_scopes":["tenant"]},"description":"grant tenant placement"}`))
	if res.IsError {
		t.Fatalf("capability-only fork of a large code agent must be accepted; got %s", res.Text)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("unmarshal fork result: %v (%s)", err, res.Text)
	}
	defID, _ := out["def_id"].(string)
	if defID == "" {
		t.Fatalf("fork returned no def_id: %s", res.Text)
	}

	// The body must survive the override intact — it is the whole agent.
	got, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if got.IsError {
		t.Fatalf("get after fork: %s", got.Text)
	}
	if !strings.Contains(got.Text, "memory_scopes") || !strings.Contains(got.Text, "tenant") {
		t.Fatalf("fork did not record the new grants: %s", got.Text)
	}
	var gotDef struct {
		Definition struct {
			Code         string   `json:"code_body"`
			MemoryScopes []string `json:"memory_scopes"`
			SqlScopes    []string `json:"sql_scopes"`
		} `json:"definition"`
	}
	if err := json.Unmarshal([]byte(got.Text), &gotDef); err != nil {
		t.Fatalf("unmarshal get result: %v", err)
	}
	if len(gotDef.Definition.Code) < 140_000 {
		t.Fatalf("inherited code body truncated: %d bytes", len(gotDef.Definition.Code))
	}
	if strings.Join(gotDef.Definition.SqlScopes, ",") != "tenant" {
		t.Fatalf("sql_scopes = %v, want [tenant]", gotDef.Definition.SqlScopes)
	}
}

// The dedicated code cap must still bite: excluding the body from the
// definition cap must not leave it ungoverned.
func TestAgentDefTool_ForkStillRefusesCodeOverItsOwnCap(t *testing.T) {
	tool, ctx, cleanup := codeAgentFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"memory/consolidator","overlay":{"code_body":`+
			mustJSON(largeCodeBody(300_000))+`}}`))
	if !res.IsError {
		t.Fatalf("a body over MaxCodeBytes must be refused; got accepted")
	}
	if !strings.Contains(res.Text, "code_body") {
		t.Fatalf("refusal should name code_body, got: %s", res.Text)
	}
}

// A non-code definition is still measured whole — the exclusion is scoped to
// the body, not a general relaxation of the cap.
func TestAgentDefTool_ForkStillRefusesAnOversizedNonCodeDefinition(t *testing.T) {
	tool, ctx, cleanup := codeAgentFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"memory/consolidator","overlay":{"system_prompt":`+
			mustJSON(strings.Repeat("p", 200_000))+`}}`))
	if !res.IsError {
		t.Fatalf("an oversized non-code definition must still be refused; got accepted")
	}
	if !strings.Contains(res.Text, "definition") {
		t.Fatalf("refusal should name the definition cap, got: %s", res.Text)
	}
}
