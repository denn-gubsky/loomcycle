package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type noTraceAuditSink struct {
	mu   sync.Mutex
	evs  []audit.Event
	fail bool
}

func (s *noTraceAuditSink) Record(ev audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	s.evs = append(s.evs, ev)
	return nil
}

// An agent whose runs are no-trace stays no-trace unless an operator says
// otherwise: a tenant member, or an agent inside a run, cannot drop the mode
// by creating a new version, forking an older one or promoting one — and an
// operator's drop is audited, or refused when it cannot be.
func TestAgentDefTool_OnlyAnOperatorMayDropANoTraceAgentsMode(t *testing.T) {
	tool, base, cleanup := agentDefFixture(t)
	defer cleanup()
	sink := &noTraceAuditSink{}
	tool.Audit = sink
	as := func(p auth.Principal) context.Context {
		ctx := auth.WithPrincipal(base, p)
		return tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_http-admin", TenantID: "acme"})
	}
	operator := as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	member := as(auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	inRun := tools.WithRunID(operator, "r_parent") // an agent's own call, under an operator's run
	exec := func(ctx context.Context, input string) tools.Result {
		t.Helper()
		res, _ := tool.Execute(ctx, json.RawMessage(input))
		return res
	}
	retentionOf := func() string {
		t.Helper()
		def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "acme", "quiet")
		if !ok {
			t.Fatal("quiet did not resolve")
		}
		return def.Retention
	}
	defID := func(res tools.Result) string {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("%s: %v", res.Text, err)
		}
		id, _ := out["def_id"].(string)
		return id
	}

	open := exec(operator, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v1"}}`)
	if open.IsError {
		t.Fatalf("create v1: %s", open.Text)
	}
	if res := exec(operator, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v2","retention":"none"}}`); res.IsError {
		t.Fatalf("create v2: %s", res.Text)
	}
	attempts := map[string]string{
		"create without the mode":    `{"op":"create","name":"quiet","overlay":{"system_prompt":"v3"}}`,
		"fork of the version before": `{"op":"fork","name":"quiet","parent_def_id":"` + defID(open) + `","promote":true,"overlay":{"system_prompt":"v3"}}`,
		"promote the version before": `{"op":"promote","def_id":"` + defID(open) + `"}`,
	}
	for who, ctx := range map[string]context.Context{"a member": member, "an agent in a run": inRun} {
		for what, input := range attempts {
			if res := exec(ctx, input); !res.IsError || !strings.Contains(res.Text, "no-trace") {
				t.Errorf("%s, %s: %s, want it refused", who, what, res.Text)
			}
			if got := retentionOf(); got != "none" {
				t.Fatalf("%s, %s: the agent's mode is now %q", who, what, got)
			}
		}
	}
	// A member may still author versions that keep the mode.
	if res := exec(member, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v3","retention":"none"}}`); res.IsError {
		t.Fatalf("a member keeping the mode: %s", res.Text)
	}

	// An operator may, once it is recorded; not when it cannot be.
	sink.fail = true
	if res := exec(operator, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v4"}}`); !res.IsError {
		t.Errorf("an operator's drop that could not be recorded: %s, want it refused", res.Text)
	}
	sink.fail = false
	if res := exec(operator, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v4"}}`); res.IsError {
		t.Fatalf("an operator's drop: %s", res.Text)
	}
	if got := retentionOf(); got != "" {
		t.Errorf("after the operator's drop: retention = %q", got)
	}
	if len(sink.evs) != 1 || sink.evs[0].TargetName != "quiet" || sink.evs[0].TargetTenant != "acme" || sink.evs[0].ActorSubject != "ops" {
		t.Errorf("audit records = %+v, want one naming quiet, acme and ops", sink.evs)
	}
}

// A hook is sent what a run says, so on a no-trace agent it is a way to
// store — elsewhere — what the agent promised to store nowhere. Adding or
// changing one is the operator's, as dropping the mode is, and audited.
func TestAgentDefTool_OnlyAnOperatorMayChangeANoTraceAgentsHooks(t *testing.T) {
	tool, base, cleanup := agentDefFixture(t)
	defer cleanup()
	sink := &noTraceAuditSink{}
	tool.Audit = sink
	as := func(p auth.Principal) context.Context {
		ctx := auth.WithPrincipal(base, p)
		return tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_http-admin", TenantID: "acme"})
	}
	operator := as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	member := as(auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	exec := func(ctx context.Context, input string) tools.Result {
		t.Helper()
		res, _ := tool.Execute(ctx, json.RawMessage(input))
		return res
	}
	hooksOf := func() string {
		t.Helper()
		def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "acme", "quiet")
		if !ok {
			t.Fatal("quiet did not resolve")
		}
		b, _ := json.Marshal(def.Hooks)
		return string(b)
	}
	const hookA = `"hooks":{"agent_stop":[{"name":"check","url":"https://app.example/a"}]}`
	const hookB = `"hooks":{"agent_stop":[{"name":"check","url":"https://collector.example/b"}]}`

	if res := exec(operator, `{"op":"create","name":"quiet","overlay":{"system_prompt":"v1","retention":"none",`+hookA+`}}`); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	staged := exec(operator, `{"op":"create","name":"quiet","promote":false,"overlay":{"system_prompt":"v2","retention":"none",`+hookB+`}}`)
	if staged.IsError {
		t.Fatalf("staged version: %s", staged.Text)
	}
	sink.evs = nil
	var out map[string]any
	_ = json.Unmarshal([]byte(staged.Text), &out)
	stagedID, _ := out["def_id"].(string)
	before := hooksOf()

	for what, input := range map[string]string{
		"create with another hook":       `{"op":"create","name":"quiet","overlay":{"system_prompt":"v3","retention":"none",` + hookB + `}}`,
		"create with the hook gone":      `{"op":"create","name":"quiet","overlay":{"system_prompt":"v3","retention":"none"}}`,
		"fork with another hook":         `{"op":"fork","name":"quiet","promote":true,"overlay":{` + hookB + `}}`,
		"promote another hook's version": `{"op":"promote","def_id":"` + stagedID + `"}`,
	} {
		if res := exec(member, input); !res.IsError || !strings.Contains(res.Text, "no-trace") {
			t.Errorf("a member, %s: %s, want it refused", what, res.Text)
		}
		if got := hooksOf(); got != before {
			t.Fatalf("a member, %s: the agent's hooks are now %s", what, got)
		}
	}
	// The same hooks, another prompt: nothing to decide.
	if res := exec(member, `{"op":"fork","name":"quiet","promote":true,"overlay":{"system_prompt":"v4"}}`); res.IsError {
		t.Errorf("a member's fork that keeps the hooks: %s", res.Text)
	}
	if len(sink.evs) != 0 {
		t.Errorf("audit records for refused or unchanged hooks: %+v", sink.evs)
	}
	// An operator may, recorded.
	if res := exec(operator, `{"op":"promote","def_id":"`+stagedID+`"}`); res.IsError {
		t.Fatalf("an operator's promote: %s", res.Text)
	}
	if got := hooksOf(); !strings.Contains(got, "collector.example") {
		t.Errorf("after the operator's promote: hooks %s", got)
	}
	if len(sink.evs) != 1 || sink.evs[0].Action != "retention_none_hooks_changed" || sink.evs[0].TargetName != "quiet" || sink.evs[0].ActorSubject != "ops" {
		t.Errorf("audit records = %+v, want one retention_none_hooks_changed naming quiet and ops", sink.evs)
	}
}
