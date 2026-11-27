package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// RFC GQ P5.7 (its transports in P5.6) — ontology op rollback, the MCP twin of POST
// /v1/_ontology/rollback.

// A rollback rewrites the tenant's vocabulary, the operator's decision: the op's twin
// route is carved out of the member path and asks write document, as the route does.
// (A member's and an isolated user's session may not call the tool at all:
// TestOntologyTool_JobOpsAreRefusedAMemberAndAnIsolatedUser.)
func TestOntologyTool_RollbackIsTheOperatorsAsItsRoute(t *testing.T) {
	tw, ok := mcpToolTwins["ontology"].Ops["rollback"]
	if !ok || tw.Method != http.MethodPost || tw.Path != "/v1/_ontology/rollback" {
		t.Fatalf("ontology rollback twin = %+v (%v), want POST /v1/_ontology/rollback", tw, ok)
	}
	if !memberpath.CarvedOut(tw.Method, tw.Path) {
		t.Error("ontology rollback: its route is not carved out of the member path")
	}
	q, ok, err := mcpTwinSurface("ontology", json.RawMessage(`{"op":"rollback"}`))
	route, routed := authz.HTTPSurface(http.MethodPost, "/v1/_ontology/rollback")
	if err != nil || !ok || !routed || q != route || q.Action != "write" || q.Type != "document" {
		t.Errorf("ontology rollback asks %+v (%v %v), its route %+v; want write document on both", q, ok, err, route)
	}
}

// ontology op=rollback hands seq, the fingerprint and the tenant to the connector as
// HTTP does and returns its answer unchanged; a refusal is HTTP's 409 body — {code,
// error, fingerprint} for plan_required and plan_stale, {code, error, applied} for
// partially_applied — the plan's own refusals carry their codes, an internal failure
// carries no text, and a seq that is not a release's number is invalid_request
// before the connector.
func TestOntologyTool_RollbackMatchesTheConnector(t *testing.T) {
	res := connector.ApplyOntologyRollbackResult{Tenant: "acme", Rollback: connector.OntologyRollbackReport{
		Release: 2, ReleaseContentVersion: "aaaa", ContentVersion: "aaaa", Matches: true, Risk: "stales",
		Applied:         []connector.OntologyRollbackStep{{Op: "rename", Entry: "c1", Name: "Memo"}, {Op: "add", Entry: "c2", Name: "Ledger", NewID: "c9"}},
		RenameJobs:      []connector.OntologyJobStatus{{ID: "j1", Kind: "class_rename", From: []string{"Note"}, To: "Memo", State: "done"}},
		RollbackRelease: 3, Notes: []string{"n"}, Warnings: []string{"w"},
	}}
	mc := &mockConnector{rollbackRes: res}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		out, err := handleOntology(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	r := call(`{"op":"rollback","seq":2,"plan_fingerprint":"fp","tenant":"globex"}`)
	var got connector.ApplyOntologyRollbackResult
	if r.IsError || json.Unmarshal([]byte(r.Content[0].Text), &got) != nil || !reflect.DeepEqual(got, res) {
		t.Errorf("rollback = %s, want the connector's", r.Content[0].Text)
	}
	if mc.rollbackReq != (connector.ApplyOntologyRollbackRequest{Tenant: "globex", Seq: 2, PlanFingerprint: "fp"}) {
		t.Errorf("rollback asked the connector %+v", mc.rollbackReq)
	}

	body := func(r *loommcp.CallToolResult) map[string]any {
		var m map[string]any
		if !r.IsError || json.Unmarshal([]byte(r.Content[0].Text), &m) != nil {
			return map[string]any{"raw": r.Content[0].Text}
		}
		return m
	}
	for _, args := range []string{`{"op":"rollback"}`, `{"op":"rollback","seq":0}`, `{"op":"rollback","seq":"2"}`, `{"op":"rollback","seq":-1}`} {
		mc.rollbackReq = connector.ApplyOntologyRollbackRequest{}
		if b := body(call(args)); b["code"] != "invalid_request" || mc.rollbackReq != (connector.ApplyOntologyRollbackRequest{}) {
			t.Errorf("%s = %v, connector got %+v; want invalid_request before the connector", args, b, mc.rollbackReq)
		}
	}

	for _, tc := range []struct {
		err  error
		want map[string]any
	}{
		{&connector.OntologyRollbackRefusal{Code: "plan_stale", Message: "rollback: plan_stale: …", Fingerprint: "fp-now"},
			map[string]any{"code": "plan_stale", "error": "rollback: plan_stale: …", "fingerprint": "fp-now"}},
		{&connector.OntologyRollbackRefusal{Code: "plan_required", Message: "rollback: plan_required: …", Fingerprint: "fp-now"},
			map[string]any{"code": "plan_required", "error": "rollback: plan_required: …", "fingerprint": "fp-now"}},
		{&connector.OntologyRollbackRefusal{Code: "partially_applied", Message: "applied 1",
			Applied: []connector.OntologyRollbackStep{{Op: "retire", Entry: "c1", Name: "Memo"}}},
			map[string]any{"code": "partially_applied", "error": "applied 1",
				"applied": []any{map[string]any{"op": "retire", "entry": "c1", "name": "Memo"}}}},
		{&connector.OntologyRollbackRefusal{Code: "rename_job_pending", Message: "cannot carry"},
			map[string]any{"code": "rename_job_pending", "error": "cannot carry"}},
		{connector.WithMessage(connector.ErrOntologyPlanForbidden, "for an operator"), map[string]any{"code": "forbidden", "error": "for an operator"}},
		{connector.WithMessage(connector.ErrOntologyPlanNotFound, "no release 9"), map[string]any{"code": "entry_not_found", "error": "no release 9"}},
	} {
		mc.rollbackErr = tc.err
		if b := body(call(`{"op":"rollback","seq":2,"plan_fingerprint":"fp"}`)); !reflect.DeepEqual(b, tc.want) {
			t.Errorf("%v: answered %v, want %v", tc.err, b, tc.want)
		}
	}
	mc.rollbackErr = errors.New("open /var/lib/secret.db: denied")
	if b := body(call(`{"op":"rollback","seq":2,"plan_fingerprint":"fp"}`)); b["code"] != "ontology_write_failed" || strings.Contains(b["error"].(string), "/var/lib") {
		t.Errorf("an internal failure = %v, want ontology_write_failed without its text", b)
	}
}
