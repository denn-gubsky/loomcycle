package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	loomapi "github.com/denn-gubsky/loomcycle/api"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

const planRouteOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\n\n" +
	"## Relations\n@section relations\n\n### cites\n"

// installPlanOntology writes planRouteOntologyMD as mi's tenant ontology, confirmed,
// with one Provision document, and returns the Provision class's chunk id.
func installPlanOntology(t *testing.T, s *Server, mi memInject) string {
	t.Helper()
	doc := s.ontologyDocument()
	ctx := s.ontologyDocCtx(context.Background(), mi)
	call := func(v map[string]any) map[string]any {
		b, _ := json.Marshal(v)
		res, err := doc.Execute(ctx, b)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", v["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	out := call(map[string]any{"op": "import_md", "scope": "tenant", "markdown": planRouteOntologyMD})
	id, root := out["document_id"].(string), out["root_chunk_id"].(string)
	call(map[string]any{"op": "set_path", "scope": "tenant", "id": id, "path": meminject.OntologyPath})
	rev := call(map[string]any{"op": "get_chunk", "scope": "tenant", "id": root})["revision"]
	call(map[string]any{"op": "update_chunk", "scope": "tenant", "id": root, "status": meminject.OntologyConfirmedStatus, "revision": rev})
	call(map[string]any{"op": "create_document", "scope": "tenant", "title": "Rule", "type": "Provision"})
	return provisionChunk(t, doc, ctx, id)
}

// provisionChunk is the chunk titled Provision in the ontology document.
func provisionChunk(t *testing.T, doc *builtin.Document, ctx context.Context, docID string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "query_chunks", "scope": "tenant", "document_id": docID, "limit": 50})
	res, err := doc.Execute(ctx, b)
	if err != nil || res.IsError {
		t.Fatalf("query_chunks: %v %s", err, res.Text)
	}
	var out struct {
		Chunks []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	for _, c := range out.Chunks {
		if c.Title == "Provision" {
			return c.ID
		}
	}
	t.Fatalf("no Provision chunk in %s", res.Text)
	return ""
}

// postPlan drives POST /v1/_ontology/plan as the principal on ctx.
func (s *Server) postPlan(t *testing.T, ctx context.Context, tenant, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/plan?tenant="+tenant, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleOntologyPlan(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestOntologyPlanRoute_MemberIsRefused — the route is the operator's: the gate
// refuses a member and admits the tenant operator, and the connector refuses a member
// and a run-plane operator itself (the check gRPC and MCP will reach past the route's).
func TestOntologyPlanRoute_MemberIsRefused(t *testing.T) {
	gate, st := tokenAuthServer(t, "legacy")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	seedToken(t, st, "lct_member", "acme", "bob", memberScopes, time.Time{})
	seedToken(t, st, "lct_operator", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	if reached, code := runMW(t, gate, http.MethodPost, "/v1/_ontology/plan", "lct_member"); reached || code != http.StatusForbidden {
		t.Errorf("a member on POST /v1/_ontology/plan = %d reached=%v, want 403", code, reached)
	}
	if reached, code := runMW(t, gate, http.MethodPost, "/v1/_ontology/plan", "lct_operator"); !reached || code == http.StatusForbidden {
		t.Errorf("the tenant operator on POST /v1/_ontology/plan = %d reached=%v, want admitted", code, reached)
	}
	// A plan writes nothing: the access-control question is a read of documents.
	if sf, ok := authz.HTTPSurface(http.MethodPost, "/v1/_ontology/plan"); !ok || sf.Action != "read" || sf.Type != "document" {
		t.Errorf("POST /v1/_ontology/plan asks %+v (governed %v), want read document", sf, ok)
	}

	s, mi := ontologyFixture(t)
	entry := installPlanOntology(t, s, mi)
	body := fmt.Sprintf(`{"changes":[{"op":"retire","entry":%q}]}`, entry)
	for what, p := range map[string]auth.Principal{
		"a member":             {TenantID: mi.Tenant, Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		"an isolated user":     {TenantID: mi.Tenant, Subject: "u", Scopes: []string{auth.ScopeUser}},
		"a run-plane operator": {TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}},
	} {
		code, out := s.postPlan(t, auth.WithPrincipal(context.Background(), p), mi.Tenant, body)
		if code != http.StatusForbidden || out["code"] != "forbidden" {
			t.Errorf("%s: %d %v, want 403 forbidden", what, code, out)
		}
	}
}

// TestOntologyPlanRoute_TenantOperatorSeesOwnTenantOnly — a tenant operator naming
// another tenant gets its own tenant's plan, and an entry of the other tenant's
// ontology is not found in its own.
func TestOntologyPlanRoute_TenantOperatorSeesOwnTenantOnly(t *testing.T) {
	s, mi := ontologyFixture(t)
	own := installPlanOntology(t, s, mi)
	other := memInject{Tenant: "globex", UserID: "gina", AgentName: "curator"}
	foreign := installPlanOntology(t, s, other)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})

	code, out := s.postPlan(t, op, other.Tenant, fmt.Sprintf(`{"changes":[{"op":"delete","entry":%q}]}`, own))
	if code != http.StatusOK || out["tenant"] != mi.Tenant {
		t.Fatalf("own entry with ?tenant=%s = %d %v, want 200 for %s", other.Tenant, code, out, mi.Tenant)
	}
	if fp, _ := out["fingerprint"].(string); len(fp) != 64 {
		t.Errorf("fingerprint = %v, want a sha256 hex", out["fingerprint"])
	}
	changes, _ := out["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("changes = %v", out["changes"])
	}
	c := changes[0].(map[string]any)
	typed := c["counts"].(map[string]any)["typed_documents"].(map[string]any)
	if c["risk"] != connector.OntologyRiskDestroys || typed["n"] != float64(1) {
		t.Errorf("plan = %v, want destroys with its own one Provision document (not the other tenant's)", c)
	}
	code, out = s.postPlan(t, op, other.Tenant, fmt.Sprintf(`{"changes":[{"op":"delete","entry":%q}]}`, foreign))
	if code != http.StatusNotFound || out["code"] != "entry_not_found" {
		t.Errorf("the other tenant's entry = %d %v, want 404 entry_not_found", code, out)
	}
}

// TestOntologyPlanRoute_RefusalsCarryTheirCodes — a malformed body, too many changes
// and an unknown field are 400 with their codes.
func TestOntologyPlanRoute_RefusalsCarryTheirCodes(t *testing.T) {
	s, mi := ontologyFixture(t)
	entry := installPlanOntology(t, s, mi)
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf(`{"op":"retire","entry":%q}`, entry)
	}
	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"not json":      {`{`, "invalid_body"},
		"unknown field": {`{"changes":[],"dry_run":true}`, "invalid_body"},
		"no changes":    {`{"changes":[]}`, "invalid_request"},
		"51 changes":    {`{"changes":[` + strings.Join(many, ",") + `]}`, "invalid_request"},
		"unknown op":    {fmt.Sprintf(`{"changes":[{"op":"merge","entry":%q}]}`, entry), "invalid_request"},
		"too large":     {`{"changes":[{"op":"edit","entry":"x","body":"` + strings.Repeat("a", 70<<10) + `"}]}`, "invalid_body"},
	} {
		code, out := s.postPlan(t, context.Background(), mi.Tenant, tc.body)
		if code != http.StatusBadRequest || out["code"] != tc.code {
			t.Errorf("%s: %d %v, want 400 %s", name, code, out, tc.code)
		}
	}
}

// TestOpenAPISpec_PublishesTheOntologyPlanRoute — POST /v1/_ontology/plan is in the
// published contract, with the ops the Document plans and the risk classes it answers.
func TestOpenAPISpec_PublishesTheOntologyPlanRoute(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatal(err)
	}
	post, _ := dig(doc, "paths", "/v1/_ontology/plan", "post").(map[string]any)
	if post == nil {
		t.Fatal("api/openapi.yaml has no POST /v1/_ontology/plan")
	}
	ops := dig(post, "requestBody", "content", "application/json", "schema", "properties", "changes", "items", "properties", "op", "enum")
	want := []any{connector.OntologyPlanRetire, connector.OntologyPlanRename, connector.OntologyPlanMove, connector.OntologyPlanEdit, connector.OntologyPlanDelete}
	if fmt.Sprint(ops) != fmt.Sprint(want) {
		t.Errorf("op enum = %v, want %v", ops, want)
	}
	risk := dig(post, "responses", "200", "content", "application/json", "schema", "properties", "changes", "items", "properties", "risk", "enum")
	wantRisk := []any{connector.OntologyRiskDestroys, connector.OntologyRiskStales, connector.OntologyRiskNone}
	if fmt.Sprint(risk) != fmt.Sprint(wantRisk) {
		t.Errorf("risk enum = %v, want %v", risk, wantRisk)
	}
}

// dig walks nested maps by key; nil when a step is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
