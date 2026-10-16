package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedAgentVersion stores one active version of a runtime agent, as the
// AgentDef tool would have.
func seedAgentVersion(t *testing.T, st store.Store, tenant, name, def string) {
	t.Helper()
	ctx := context.Background()
	row, err := st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: mintDefID(), Name: name, Definition: json.RawMessage(def), TenantID: tenant, ContentSHA256: "sha256:" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AgentDefSetActive(ctx, tenant, name, row.DefID, ""); err != nil {
		t.Fatal(err)
	}
}

// A no-trace agent that names a HookDef without a version runs whatever
// version is active, so changing which version that is — create, fork,
// promote, retire, delete — is the operator's, as changing the agent's own
// hooks is, and audited. A HookDef no such agent names, or names pinned, is
// anyone's who may author hooks.
func TestHookDef_ChangingOneANoTraceAgentRunsIsTheOperators(t *testing.T) {
	tool, st := hookDefFixture(t)
	sink := &noTraceAuditSink{}
	tool.Audit = sink
	tool.Cfg = &config.Config{Agents: map[string]config.AgentDef{
		"static-quiet": {Retention: store.RetentionNone, Hooks: hooks.EventHooks{hooks.PhaseAgentStop: {{Ref: "shared/gate"}}}},
	}}
	as := func(tenant string, p auth.Principal) context.Context {
		return auth.WithPrincipal(hookTenantCtx(tenant), p)
	}
	operator := as("acme", auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	member := as("acme", auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	sharedMember := as("", auth.Principal{Subject: "carol", Scopes: []string{auth.ScopeRunsCreate}})

	for _, name := range []string{"gate", "tool/gate", "pinned", "other"} {
		mustHookDef(t, tool, operator, `{"op":"create","name":"`+name+`","overlay":`+httpHookOverlay+`}`)
	}
	seedAgentVersion(t, st, "acme", "quiet", `{"retention":"none","hooks":{"pre":["gate"]},"tool_hooks":{"WebFetch":{"pre":["tool/gate"]}}}`)
	seedAgentVersion(t, st, "acme", "quiet-pinned", `{"retention":"none","hooks":{"pre":["pinned@1"]}}`)
	seedAgentVersion(t, st, "acme", "loud", `{"hooks":{"pre":["other"]}}`)
	sink.evs = nil

	activeID := func(name string) string {
		t.Helper()
		row, err := st.HookDefGetActive(context.Background(), "acme", name)
		if err != nil {
			t.Fatal(err)
		}
		return row.DefID
	}
	staged := mustHookDef(t, tool, operator, `{"op":"fork","name":"gate","promote":false,"overlay":{"body":{"kind":"http","url":"https://collector.example/x"}}}`)
	stagedID, _ := staged["def_id"].(string)
	sink.evs = nil
	gateBefore, toolGateBefore := activeID("gate"), activeID("tool/gate")

	// In order, delete last: one that went through would hide the rest.
	for _, c := range []struct{ what, input string }{
		{"create", `{"op":"create","name":"gate","overlay":` + httpHookOverlay + `}`},
		{"fork", `{"op":"fork","name":"gate","overlay":{"timeout_ms":900}}`},
		{"fork of a tool hook", `{"op":"fork","name":"tool/gate","overlay":{"timeout_ms":900}}`},
		{"promote", `{"op":"promote","def_id":"` + stagedID + `"}`},
		{"retire the active", `{"op":"retire","def_id":"` + gateBefore + `","retired":true}`},
		{"delete", `{"op":"delete","name":"gate"}`},
	} {
		what, input := c.what, c.input
		if _, text, isErr := runHookDef(t, tool, member, input); !isErr || !strings.Contains(text, "no-trace") {
			t.Errorf("a member, %s: %s, want it refused", what, text)
		}
	}
	if activeID("gate") != gateBefore || activeID("tool/gate") != toolGateBefore {
		t.Fatal("a refused change moved the active version")
	}
	// The shared layer is what the operator's own (static) agents resolve
	// their references in.
	admin := as("", auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	mustHookDef(t, tool, admin, `{"op":"create","name":"shared/gate","overlay":`+httpHookOverlay+`}`)
	sink.evs = nil
	if _, text, isErr := runHookDef(t, tool, sharedMember, `{"op":"fork","name":"shared/gate","overlay":{"timeout_ms":900}}`); !isErr || !strings.Contains(text, "no-trace") {
		t.Errorf("a non-operator forking a shared HookDef a static no-trace agent names: %s, want it refused", text)
	}

	// Not named by a no-trace agent, or named pinned: unaffected, unrecorded.
	for what, input := range map[string]string{
		"a HookDef only an ordinary agent names": `{"op":"fork","name":"other","overlay":{"timeout_ms":900}}`,
		"a HookDef a no-trace agent pins":        `{"op":"fork","name":"pinned","overlay":{"timeout_ms":900}}`,
	} {
		if _, text, isErr := runHookDef(t, tool, member, input); isErr {
			t.Errorf("a member, %s: %s, want it allowed", what, text)
		}
	}
	if len(sink.evs) != 0 {
		t.Errorf("audit records for unaffected changes: %+v", sink.evs)
	}

	// An operator may, recorded; not when the record cannot be written.
	sink.fail = true
	if _, text, isErr := runHookDef(t, tool, operator, `{"op":"promote","def_id":"`+stagedID+`"}`); !isErr {
		t.Errorf("an operator's change that could not be recorded: %s, want it refused", text)
	}
	sink.fail = false
	mustHookDef(t, tool, operator, `{"op":"promote","def_id":"`+stagedID+`"}`)
	if activeID("gate") != stagedID {
		t.Error("the operator's promote did not take")
	}
	if len(sink.evs) != 1 || sink.evs[0].Action != "retention_none_hookdef_changed" || sink.evs[0].TargetName != "gate" ||
		sink.evs[0].TargetTenant != "acme" || sink.evs[0].ActorSubject != "ops" {
		t.Errorf("audit records = %+v, want one retention_none_hookdef_changed naming gate, acme and ops", sink.evs)
	}
}
