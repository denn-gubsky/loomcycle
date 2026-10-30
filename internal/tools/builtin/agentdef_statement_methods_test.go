package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func lookupStatementMethods(t *testing.T, tool *AgentDef, name string) string {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("%s not found", name)
	}
	return strings.Join(def.StatementMethods, ",")
}

// statement_methods authored over the substrate survives persist and resolve,
// a fork that does not send it keeps its parent's, and a create changing only
// it mints a new version (it is not part of the content hash, so the create
// dedup has to compare it).
func TestAgentDefTool_StatementMethodsRoundTrip(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)

	for _, tc := range []struct{ name, field, want string }{
		{"link-absent", ``, ""},
		{"link-rule", `,"statement_methods":["rule"]`, "rule"},
		{"link-both", `,"statement_methods":["rule","extractor"]`, "rule,extractor"},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+tc.name+`","promote":true,"overlay":{
			"provider":"code-js","system_prompt":"links","tools":["Document"]`+tc.field+`}}`))
		if res.IsError {
			t.Fatalf("create %s: %s", tc.name, res.Text)
		}
		if got := lookupStatementMethods(t, tool, tc.name); got != tc.want {
			t.Errorf("%s: statement_methods = %q, want %q", tc.name, got, tc.want)
		}
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"link-rule","promote":true,"overlay":{
		"provider":"code-js","system_prompt":"links","tools":["Document"],"statement_methods":["rule","extractor"]}}`))
	if res.IsError {
		t.Fatalf("re-create changing only statement_methods: %s", res.Text)
	}
	if got := lookupStatementMethods(t, tool, "link-rule"); got != "rule,extractor" {
		t.Errorf("a create changing only statement_methods deduplicated: %q, want rule,extractor", got)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"link-both","promote":true,"overlay":{"system_prompt":"links again"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := lookupStatementMethods(t, tool, "link-both"); got != "rule,extractor" {
		t.Errorf("after a fork that did not send it: statement_methods = %q, want rule,extractor", got)
	}
}

// A model-driven definition may not hold the grant, from any author: at
// create, and at a fork that moves a code-js agent onto a model while its
// grant rides along unsent.
func TestAgentDefTool_RefusesStatementMethodsOnAModelAgent(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"writer","overlay":{
		"model":"claude-haiku-4-5","system_prompt":"w","statement_methods":["rule"]}}`))
	wantRefused(t, res, "a model agent with statement_methods", "statement_methods", "code-js")

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"linker","promote":true,"overlay":{
		"provider":"code-js","system_prompt":"links","statement_methods":["rule"]}}`))
	if res.IsError {
		t.Fatalf("create code-js: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"linker","overlay":{"provider":"anthropic","model":"claude-haiku-4-5"}}`))
	wantRefused(t, res, "a fork moving the grant onto a model", "statement_methods", "code-js")

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"linker2","overlay":{
		"provider":"code-js","system_prompt":"links","statement_methods":["linker"]}}`))
	wantRefused(t, res, "an unknown method", "statement_methods[0]", `"linker"`)
}

// A fork of a YAML code-js agent keeps the grant the operator gave it.
func TestAgentDefTool_ForkOfAStaticAgentKeepsItsStatementMethods(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Agents["kb-linker"] = config.AgentDef{Provider: "code-js", SystemPrompt: "links", Tools: []string{"Document"}, StatementMethods: []string{"rule"}}

	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"fork","name":"kb-linker","promote":true,"overlay":{"system_prompt":"links carefully"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	row, err := tool.Store.AgentDefGetActive(context.Background(), tools.RunIdentity(ctx).TenantID, "kb-linker")
	if err != nil {
		t.Fatalf("AgentDefGetActive: %v", err)
	}
	var stored lookup.SubstrateAgentDef
	if err := json.Unmarshal(row.Definition, &stored); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stored.StatementMethods, ","); got != "rule" {
		t.Errorf("the stored fork of a static agent has statement_methods %q, want rule", got)
	}
}

// The ceiling: an in-run author may create only with the methods it may stamp
// itself, and may fork a version only narrowing what that version held —
// never widening it, even with a method the author holds.
func TestAgentDefCreate_StatementMethodsCeiling(t *testing.T) {
	tool, ctx := ceilingFixture(t) // in-run author holding no statement methods

	for name, list := range map[string]string{"sdlc/unset": ``, "sdlc/empty": `,"statement_methods":[]`} {
		if res := agentDefOp(t, tool, ctx, "create", name, `{"provider":"code-js","history_scope":["user"]`+list+`}`); res.IsError {
			t.Fatalf("%s: a grant of nothing was refused: %s", name, res.Text)
		}
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/r0", `{"provider":"code-js","history_scope":["user"],"statement_methods":["rule"]}`),
		"rule by an author that may stamp nothing", "statement_methods:", `"rule"`)

	ruler := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.StatementMethods = []string{"rule"} })
	if res := agentDefOp(t, tool, ruler, "create", "sdlc/r1", `{"provider":"code-js","history_scope":["user"],"statement_methods":["rule"]}`); res.IsError {
		t.Fatalf("rule within the author's own grant refused: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, ruler, "create", "sdlc/e1", `{"provider":"code-js","history_scope":["user"],"statement_methods":["rule","extractor"]}`),
		"extractor by an author that may stamp only rule", "statement_methods:", `"extractor"`)

	// An operator mints a rule-only agent. An author holding both methods may
	// fork it keeping rule, but not widen it to extractor.
	both := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.StatementMethods = []string{"rule", "extractor"} })
	op := operatorPlane(context.Background())
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) { rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}} })
	if res, _ := tool.Execute(op, json.RawMessage(`{"op":"create","name":"sdlc/linker","promote":true,
		"overlay":{"provider":"code-js","history_scope":["user"],"statement_methods":["rule"]}}`)); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	if res := agentDefOp(t, tool, ctx, "fork", "sdlc/linker", `{"system_prompt":"again"}`); res.IsError {
		t.Fatalf("fork keeping the base's rule by an author holding nothing: %s", res.Text)
	}
	if res := agentDefOp(t, tool, both, "fork", "sdlc/linker", `{"statement_methods":[]}`); res.IsError {
		t.Fatalf("fork narrowing to none: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, both, "fork", "sdlc/r1", `{"statement_methods":["rule","extractor"]}`),
		"fork widening to extractor", "statement_methods:", `"extractor"`)
}
