package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tenant holding no version of its own runs the shared ("") tier's, so the
// shared lineage's no-trace mode and read tool modes hold over the tenant's
// first version of the name, as the override lock does
// (TestAgentDefTool_ATenantForkOfASharedVersionBeforeTheLockIsRefused).

func acmeAs(base context.Context, p auth.Principal) context.Context {
	return tools.WithRunIdentity(auth.WithPrincipal(base, p), tools.RunIdentityValue{AgentID: "a_http-admin", TenantID: "acme"})
}

func TestAgentDefTool_ATenantVersionOfASharedNoTraceAgentKeepsTheMode(t *testing.T) {
	tool, base, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Audit = &noTraceAuditSink{}
	v1 := defIDOf(t, execOK(t, tool, base, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v1"}}`))
	execOK(t, tool, base, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v2","retention":"none"}}`)
	retentionIn := func(tenant string) string {
		t.Helper()
		def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, tenant, "quiet")
		if !ok {
			t.Fatal("quiet did not resolve")
		}
		return def.Retention
	}
	if got := retentionIn("acme"); got != "none" {
		t.Fatalf("setup: acme resolves retention %q, want the shared none", got)
	}
	member := acmeAs(base, auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	for what, input := range map[string]string{
		"fork of the shared version before": `{"op":"fork","name":"quiet","parent_def_id":"` + v1 + `","promote":true,"overlay":{"system_prompt":"acme"}}`,
		"create without the mode":           `{"op":"create","name":"quiet","overlay":{"system_prompt":"acme"}}`,
	} {
		res, _ := tool.Execute(member, json.RawMessage(input))
		if !res.IsError || !strings.Contains(res.Text, "no-trace") {
			t.Errorf("acme member, %s: %s, want it refused", what, res.Text)
		}
		if got := retentionIn("acme"); got != "none" {
			t.Fatalf("acme member, %s: acme now resolves retention %q", what, got)
		}
	}
	// Keeping the mode is fine, and acme's operator may drop it in acme.
	execOK(t, tool, member, `{"op":"fork","name":"quiet","parent_def_id":"`+v1+`","promote":false,"overlay":{"system_prompt":"acme","retention":"none"}}`)
	operator := acmeAs(base, auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	execOK(t, tool, operator, `{"op":"fork","name":"quiet","parent_def_id":"`+v1+`","promote":true,"overlay":{"system_prompt":"acme open"}}`)
	if got := retentionIn("acme"); got != "" {
		t.Errorf("after acme's operator dropped the mode: retention %q", got)
	}
	if got := retentionIn(""); got != "none" {
		t.Errorf("the shared agent's retention is now %q", got)
	}
}

func TestAgentDefTool_ATenantVersionOfASharedReadModeAgentKeepsTheMode(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	run := inRunAuthor(ctx)
	v1 := createWholeReader(t, tool, run, "reader")
	if text, isErr := execModeDef(t, tool, run, `{"op":"fork","name":"reader","promote":true,"overlay":{"tool_modes":{"Document":"read"}}}`); isErr {
		t.Fatalf("fork adding the read mode: %s", text)
	}
	acmeRun := tools.WithRunIdentity(run, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	modeIn := func(tenant string) string {
		t.Helper()
		def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, tenant, "reader")
		if !ok {
			t.Fatal("reader did not resolve")
		}
		return def.ToolModes["Document"]
	}
	for what, input := range map[string]string{
		"fork of the shared version before": `{"op":"fork","name":"reader","parent_def_id":"` + v1 + `","promote":true,"overlay":{"system_prompt":"acme"}}`,
		"create without the mode":           `{"op":"create","name":"reader","overlay":{"system_prompt":"acme","tools":["Document"],"volumes":[],"history_scope":["-*"],"skills":["-*"]}}`,
	} {
		text, isErr := execModeDef(t, tool, acmeRun, input)
		if !isErr || !strings.Contains(text, "read mode") {
			t.Errorf("acme, %s: %s, want it refused", what, text)
		}
		if got := modeIn("acme"); got != config.ToolModeRead {
			t.Fatalf("acme, %s: acme now resolves Document mode %q", what, got)
		}
	}
	// Keeping the mode is fine, and acme's operator plane may lift it in acme.
	if text, isErr := execModeDef(t, tool, acmeRun, `{"op":"fork","name":"reader","parent_def_id":"`+v1+`","promote":false,"overlay":{"tool_modes":{"Document":"read"}}}`); isErr {
		t.Fatalf("acme fork keeping the mode: %s", text)
	}
	op := operatorPlane(tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"}))
	if text, isErr := execModeDef(t, tool, op, `{"op":"fork","name":"reader","parent_def_id":"`+v1+`","promote":true,"overlay":{"system_prompt":"acme whole"}}`); isErr {
		t.Fatalf("acme operator lifting the mode: %s", text)
	}
	if got := modeIn("acme"); got == config.ToolModeRead {
		t.Errorf("after acme's operator lifted it: Document mode %q", got)
	}
}
