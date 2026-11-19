package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// ontology op=plan hands its changes and the tenant to the connector as they are
// and returns the connector's plan; a refusal comes back as {code, error}, an
// internal failure without its text, and arguments over the plan's bound never
// reach the connector.
func TestOntologyTool_PlanReturnsTheConnectorsPlan(t *testing.T) {
	plan := connector.PlanOntologyResult{
		Tenant: "acme", Fingerprint: strings.Repeat("a", 64), Complete: true, ScopesCounted: 2,
		Changes: []connector.OntologyPlannedChange{{
			Op: "delete", Risk: connector.OntologyRiskDestroys,
			Entry: connector.OntologyPlanEntry{ChunkID: "c1", Name: "Provision", Section: "classes", Kind: "class"},
			Counts: map[string]connector.OntologyPlanCount{
				"typed_documents": {N: 3, Effect: connector.OntologyEffectOrphaned, TenantScope: 1, OtherScopes: 2, Sample: []string{"d1"}},
			},
		}},
		Issues: []string{}, NewIssues: []string{"x"},
	}
	mc := &mockConnector{planRes: plan}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		res, err := handleOntology(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(`{"op":"plan","tenant":"globex","changes":[{"op":"delete","entry":"c1"},{"op":"rename","entry":"c2","name":"Clause"},` +
		`{"op":"move","entry":"c3","parent":"c4"},{"op":"edit","entry":"c5","body":"b"}]}`)
	if res.IsError {
		t.Fatalf("plan: %s", res.Content[0].Text)
	}
	want := connector.PlanOntologyRequest{Tenant: "globex", Changes: []connector.OntologyChange{
		{Op: "delete", Entry: "c1"}, {Op: "rename", Entry: "c2", Name: "Clause"},
		{Op: "move", Entry: "c3", Parent: "c4"}, {Op: "edit", Entry: "c5", Body: "b"},
	}}
	if got := mc.planReq; !reflect.DeepEqual(got, want) {
		t.Errorf("connector got %+v, want %+v", got, want)
	}
	var out connector.PlanOntologyResult
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil || !reflect.DeepEqual(out, plan) {
		t.Errorf("result = %s (%v), want the connector's plan", res.Content[0].Text, err)
	}

	for _, tc := range []struct {
		err  error
		code string
	}{
		{connector.WithMessage(connector.ErrOntologyPlanInvalid, "changes is required"), "invalid_request"},
		{connector.WithMessage(connector.ErrOntologyPlanNotFound, "no such entry"), "entry_not_found"},
		{connector.WithMessage(connector.ErrOntologyPlanForbidden, "planning is for an operator"), "forbidden"},
		{connector.WithMessage(connector.ErrOntologyPlanUnavailable, "no SQL Memory"), "unavailable"},
		{errors.New("open /var/lib/secret.db: denied"), "plan_failed"},
	} {
		mc.planErr = tc.err
		res := call(`{"op":"plan","changes":[{"op":"retire","entry":"c1"}]}`)
		var refusal struct{ Code, Error string }
		if !res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &refusal) != nil || refusal.Code != tc.code {
			t.Errorf("%v: got %+v, want code %s", tc.err, res.Content, tc.code)
		}
		if strings.Contains(refusal.Error, "/var/lib") {
			t.Errorf("an internal failure's text reached the caller: %s", refusal.Error)
		}
	}

	mc.planErr, mc.planReq = nil, connector.PlanOntologyRequest{}
	big := `{"op":"plan","changes":[{"op":"edit","entry":"c1","body":"` +
		strings.Repeat("a", connector.OntologyPlanMaxRequestBytes) + `"}]}`
	res = call(big)
	var refusal struct{ Code, Error string }
	if !res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &refusal) != nil ||
		refusal.Code != "invalid_body" || mc.planReq.Changes != nil {
		t.Errorf("arguments over the bound: %+v, connector got %+v; want invalid_body before the connector", res.Content, mc.planReq)
	}
}

// The plan asks the access-control question of its HTTP twin, POST
// /v1/_ontology/plan: a read of documents, since it writes nothing.
func TestOntologyTool_PlanAsksItsRoutesQuestion(t *testing.T) {
	q, ok, err := mcpTwinSurface("ontology", json.RawMessage(`{"op":"plan"}`))
	if err != nil || !ok {
		t.Fatalf("ontology plan has no twin question: %v %v", ok, err)
	}
	route, ok := authz.HTTPSurface(http.MethodPost, "/v1/_ontology/plan")
	if !ok || q != route || q.Action != "read" || q.Type != "document" {
		t.Errorf("ontology plan asks %+v, its route %+v; want read document on both", q, route)
	}
}

// ontology op=plan takes a proposal item {op: proposal, id} — its schema names id, and
// the decoder that refuses unknown keys admits it — and hands it to the connector as
// it is (RFC GQ P4.3).
func TestOntologyTool_PlanPassesAProposalItem(t *testing.T) {
	mc := &mockConnector{planRes: connector.PlanOntologyResult{Changes: []connector.OntologyPlannedChange{{Op: "retire", Proposal: "p1"}}}}
	res, err := handleOntology(context.Background(), &handlerEnv{connector: mc}, json.RawMessage(`{"op":"plan","changes":[{"op":"proposal","id":"p1"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("plan: %v %+v", err, res)
	}
	want := []connector.OntologyChange{{Op: connector.OntologyPlanProposal, ID: "p1"}}
	if !reflect.DeepEqual(mc.planReq.Changes, want) {
		t.Errorf("connector got %+v, want %+v", mc.planReq.Changes, want)
	}
	if !strings.Contains(res.Content[0].Text, `"proposal":"p1"`) {
		t.Errorf("result %s does not name the proposal", res.Content[0].Text)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(ontologyToolSchema), &schema); err != nil {
		t.Fatal(err)
	}
	items, _ := schema["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)
	props, _ := items["properties"].(map[string]any)
	ops, _ := props["op"].(map[string]any)["enum"].([]any)
	if _, ok := props["id"]; !ok || !reflect.DeepEqual(items["required"], []any{"op"}) || len(ops) == 0 || ops[len(ops)-1] != connector.OntologyPlanProposal {
		t.Errorf("changes items schema %v: want id, op proposal, and only op required", items)
	}
}
