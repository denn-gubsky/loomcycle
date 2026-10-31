package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// ontology op=merge_subjects hands its arguments to the connector as they are:
// who may merge, the tenant and the merge all live there. A refusal comes back
// as the HTTP error body's {code, error}, and an unknown op never reaches the
// connector.
func TestOntologyTool_DelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{mergeRes: connector.MergeSubjectsResult{Tenant: "acme", FactsMoved: 2, NotAnnounced: "why"}}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		res, err := handleOntology(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(`{"op":"merge_subjects","duplicate":"person:ada-king","survivor":"d2","tenant":"globex"}`)
	if res.IsError {
		t.Fatalf("merge: %s", res.Content[0].Text)
	}
	if got := mc.mergeReq; got.Duplicate != "person:ada-king" || got.Survivor != "d2" || got.Tenant != "globex" {
		t.Errorf("connector got %+v", got)
	}
	var out connector.MergeSubjectsResult
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil || out.FactsMoved != 2 || out.NotAnnounced != "why" {
		t.Errorf("result = %s (%v)", res.Content[0].Text, err)
	}

	mc.mergeReq = connector.MergeSubjectsRequest{}
	if res := call(`{"op":"split_subject","duplicate":"a","survivor":"b"}`); !res.IsError || mc.mergeReq.Duplicate != "" {
		t.Errorf("an unknown op reached the connector: is_error=%v req=%+v", res.IsError, mc.mergeReq)
	}

	mc.mergeErr = connector.WithMessage(connector.ErrSubjectMergeAlreadyMerged, "person:ada-king was already merged into another subject")
	res = call(`{"op":"merge_subjects","duplicate":"a","survivor":"b"}`)
	var refusal struct{ Code, Error string }
	if !res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &refusal) != nil ||
		refusal.Code != "already_merged" || !strings.Contains(refusal.Error, "already merged") {
		t.Errorf("a refusal was not surfaced with its code: %+v", res)
	}
}

// Who may call it on this transport: an admin and a tenant operator. A member,
// an isolated user and a token limited to certain agents may not, and do not
// see it listed.
func TestOntologyTool_IsOperatorOnly(t *testing.T) {
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	for what, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"an admin":           {as([]string{auth.ScopeAdmin}, auth.Restrictions{}), true},
		"a tenant operator":  {as([]string{auth.ScopeTenant}, auth.Restrictions{}), true},
		"a member":           {as([]string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, auth.Restrictions{}), false},
		"an isolated user":   {as([]string{auth.ScopeUser}, auth.Restrictions{}), false},
		"a restricted token": {as([]string{auth.ScopeTenant}, auth.Restrictions{Agents: []string{"judge"}}), false},
	} {
		if got := principalMayCallTool(tc.ctx, "ontology"); got != tc.want {
			t.Errorf("%s may call ontology = %v, want %v", what, got, tc.want)
		}
	}
}

// ontology op=bind_principal hands its arguments to the connector as they are. A
// refusal comes back as {code, error}; a call that leaves principal out never
// reaches the connector, because an empty principal clears a binding; and "" is
// passed through as the clear it asks for.
func TestOntologyTool_BindPrincipalDelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{bindRes: connector.BindSubjectPrincipalResult{Tenant: "acme", NaturalKey: "person:ada", Principal: "u-ada", Changed: true}}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		res, err := handleOntology(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(`{"op":"bind_principal","subject":"person:ada","principal":"u-ada","tenant":"globex"}`)
	if res.IsError {
		t.Fatalf("bind: %s", res.Content[0].Text)
	}
	if got := mc.bindReq; got.Subject != "person:ada" || got.Principal != "u-ada" || got.Tenant != "globex" {
		t.Errorf("connector got %+v", got)
	}
	var out connector.BindSubjectPrincipalResult
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil || out.Principal != "u-ada" || !out.Changed {
		t.Errorf("result = %s (%v)", res.Content[0].Text, err)
	}

	mc.bindReq = connector.BindSubjectPrincipalRequest{}
	if res := call(`{"op":"bind_principal","subject":"person:ada"}`); !res.IsError || mc.bindReq.Subject != "" {
		t.Errorf("a call without principal reached the connector: is_error=%v req=%+v", res.IsError, mc.bindReq)
	}
	if res := call(`{"op":"bind_principal","subject":"person:ada","principal":""}`); res.IsError || mc.bindReq.Subject != "person:ada" || mc.bindReq.Principal != "" {
		t.Errorf("a clear did not reach the connector: is_error=%v req=%+v", res.IsError, mc.bindReq)
	}

	mc.bindErr = connector.WithMessage(connector.ErrSubjectBindConflict, `principal "u-ada" is already bound to subject person:ada-king`)
	res = call(`{"op":"bind_principal","subject":"a","principal":"u-ada"}`)
	var refusal struct{ Code, Error string }
	if !res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &refusal) != nil ||
		refusal.Code != "binding_conflict" || !strings.Contains(refusal.Error, "person:ada-king") {
		t.Errorf("a refusal was not surfaced with its code: %+v", res)
	}
}
