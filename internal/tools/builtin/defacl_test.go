package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.2 — an in-run def-tool call asks its op's verb of the definition
// it names, against the run's ACL (the originating principal's).

func defACLSet(t *testing.T, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{ID: "alice"},
		Bindings: []authz.Binding{{ACL: "l", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"l": {ACL: authz.ACL{Name: "l", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// runJudgedBy makes ctx a run's, judged by eff.
func runJudgedBy(ctx context.Context, eff *authz.Effective) context.Context {
	ctx = authz.WithMode(ctx, authz.ModeCompat)
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.ACL = eff })
	return tools.WithRunID(ctx, "run_acl")
}

func aclCode(res tools.Result) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(res.Text), &b)
	return b.Code
}

func defIDOf(t *testing.T, res tools.Result) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	id, _ := decodeResult(t, res.Text)["def_id"].(string)
	if id == "" {
		t.Fatalf("no def_id in %s", res.Text)
	}
	return id
}

func TestAgentDefTool_InRunAuthorDenyRefusesWritesNotReads(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	defID := defIDOf(t, mustExec(t, tool, ctx, `{"op":"create","name":"reviewer","overlay":{"provider":"openai","system_prompt":"p"}}`))

	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"author"}, Resources: []string{"def:agent/*"}}))
	for _, in := range []string{
		`{"op":"create","name":"other","overlay":{"provider":"openai","system_prompt":"p"}}`,
		`{"op":"fork","name":"reviewer","overlay":{"system_prompt":"q"}}`,
		`{"op":"promote","def_id":"` + defID + `"}`,
		`{"op":"frobnicate","name":"reviewer"}`, // an unknown op asks author
	} {
		res, _ := tool.Execute(run, json.RawMessage(in))
		if !res.IsError || aclCode(res) != "acl_denied" {
			t.Errorf("%s: want acl_denied, got %s", in, res.Text)
		}
	}
	for _, in := range []string{`{"op":"get","def_id":"` + defID + `"}`, `{"op":"list","name":"reviewer"}`} {
		if res, _ := tool.Execute(run, json.RawMessage(in)); res.IsError {
			t.Errorf("%s: a deny on author refused a read: %s", in, res.Text)
		}
	}
}

func TestAgentDefTool_RetireByDefIDAsksTheDefinitionsOwnName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	locked := defIDOf(t, mustExec(t, tool, ctx, `{"op":"create","name":"locked","overlay":{"provider":"openai","system_prompt":"p"}}`))
	open := defIDOf(t, mustExec(t, tool, ctx, `{"op":"create","name":"open","overlay":{"provider":"openai","system_prompt":"p"}}`))

	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"retire"}, Resources: []string{"def:agent/locked"}}))
	// The op acts on the id, so naming an allowed definition beside it must
	// not carry it.
	res, _ := tool.Execute(run, json.RawMessage(`{"op":"retire","name":"open","def_id":"`+locked+`","retired":true}`))
	if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "def:agent/locked") {
		t.Errorf("retire of a denied definition by its id: %s", res.Text)
	}
	if res, _ := tool.Execute(run, json.RawMessage(`{"op":"retire","def_id":"`+open+`","retired":true}`)); res.IsError {
		t.Errorf("retire of an allowed definition: %s", res.Text)
	}
	if res, _ := tool.Execute(run, json.RawMessage(`{"op":"fork","name":"locked","overlay":{"system_prompt":"q"}}`)); res.IsError {
		t.Errorf("a deny on retire refused a fork: %s", res.Text)
	}
}

func TestAgentDefTool_UnresolvedDefIDAsksTheFamily(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	run := runJudgedBy(ctx, defACLSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"retire"}, Resources: []string{"def:agent/*"}}))
	res, _ := tool.Execute(run, json.RawMessage(`{"op":"retire","def_id":"ad_missing","retired":true}`))
	if aclCode(res) != "acl_denied" || !strings.Contains(res.Text, "def:agent/*") {
		t.Errorf("an id that does not resolve must be asked of the family: %s", res.Text)
	}
}

func mustExec(t *testing.T, tool tools.Tool, ctx context.Context, in string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("%s: %v", in, err)
	}
	return res
}
