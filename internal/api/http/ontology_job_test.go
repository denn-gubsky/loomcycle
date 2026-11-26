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
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// RFC GQ P5.6 — the tenant's ontology job over HTTP: its status and its resume.

// jobRoute drives the job status or resume as the principal on ctx, through a mux
// holding the patterns the server registers.
func (s *Server) jobRoute(t *testing.T, ctx context.Context, method, target, body string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/_ontology/job", s.handleOntologyJob)
	mux.HandleFunc("POST /v1/_ontology/job/resume", s.handleOntologyJobResume)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// renameProvision renames the class Provision (the plan fixture's typed document rests
// on it) through the Document with the plan's fingerprint, which starts the rename job.
func (s *Server) renameProvision(t *testing.T, mi memInject, provision, name string) map[string]any {
	t.Helper()
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})
	plan, err := s.PlanOntology(op, connector.PlanOntologyRequest{Changes: []connector.OntologyChange{{Op: "rename", Entry: provision, Name: name}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	doc, dctx := s.ontologyDocument(), s.ontologyDocCtx(context.Background(), mi)
	get, _ := doc.Execute(dctx, []byte(`{"op":"get_chunk","scope":"tenant","id":"`+provision+`"}`))
	var row struct {
		Revision int `json:"revision"`
	}
	_ = json.Unmarshal([]byte(get.Text), &row)
	b, _ := json.Marshal(map[string]any{"op": "update_chunk", "scope": "tenant", "id": provision, "revision": row.Revision,
		"title": name, "plan_fingerprint": plan.Fingerprint})
	res, err := doc.Execute(dctx, b)
	if err != nil || res.IsError {
		t.Fatalf("rename Provision: %v %s", err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

// setJobState rewrites the tenant's job row's state, as a slice that ran out of time
// would have left it.
func (s *Server) setJobState(t *testing.T, mi memInject, state string) {
	t.Helper()
	key := sqlmem.ScopeKey{Tenant: mi.Tenant, Scope: "tenant", ScopeID: mi.Tenant}
	if _, err := s.sqlMem.DocPlane().Exec(context.Background(), key, `UPDATE ontology_jobs SET state = ?`, []any{state}, 0); err != nil {
		t.Fatalf("set the job's state: %v", err)
	}
}

// TestOpenAPISpec_PublishesTheOntologyJobRoutes — both routes are in the published spec,
// with the job's states, and the plan's change carries the rename job's block.
func TestOpenAPISpec_PublishesTheOntologyJobRoutes(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatal(err)
	}
	if dig(doc, "paths", "/v1/_ontology/job", "get") == nil || dig(doc, "paths", "/v1/_ontology/job/resume", "post") == nil {
		t.Error("api/openapi.yaml lacks GET /v1/_ontology/job or POST /v1/_ontology/job/resume")
	}
	if got := fmt.Sprint(dig(doc, "components", "schemas", "OntologyJob", "properties", "state", "enum")); got != "[pending running blocked done abandoned]" {
		t.Errorf("job state enum = %s", got)
	}
	job := dig(doc, "paths", "/v1/_ontology/plan", "post", "responses", "200", "content", "application/json", "schema",
		"properties", "changes", "items", "properties", "job", "$ref")
	if job != "#/components/schemas/OntologyRenameJobPlan" {
		t.Errorf("the plan's change job = %v, want the OntologyRenameJobPlan schema", job)
	}
}

// TestOntologyJobRoute_MemberIsRefused —the job rewrites stored rows in every scope of
// the tenant, so its status and its resume are the operator's: the gate refuses a member
// on both routes and admits the tenant operator, the status asks read document and the
// resume write document, and the connector refuses a member, an isolated user and a
// run-plane operator itself (the check the gRPC and MCP twins reach past the route's).
func TestOntologyJobRoute_MemberIsRefused(t *testing.T) {
	gate, st := tokenAuthServer(t, "legacy")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	seedToken(t, st, "lct_member", "acme", "bob", memberScopes, time.Time{})
	seedToken(t, st, "lct_operator", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	for _, r := range []struct{ method, path, action string }{
		{http.MethodGet, "/v1/_ontology/job", "read"},
		{http.MethodPost, "/v1/_ontology/job/resume", "write"},
	} {
		if reached, code := runMW(t, gate, r.method, r.path, "lct_member"); reached || code != http.StatusForbidden {
			t.Errorf("a member on %s %s = %d reached=%v, want 403", r.method, r.path, code, reached)
		}
		if reached, code := runMW(t, gate, r.method, r.path, "lct_operator"); !reached || code == http.StatusForbidden {
			t.Errorf("the tenant operator on %s %s = %d reached=%v, want admitted", r.method, r.path, code, reached)
		}
		if sf, ok := authz.HTTPSurface(r.method, r.path); !ok || sf.Action != r.action || sf.Type != "document" {
			t.Errorf("%s %s asks %+v (governed %v), want %s document", r.method, r.path, sf, ok, r.action)
		}
	}

	s, mi := ontologyFixture(t)
	installPlanOntology(t, s, mi)
	for what, p := range map[string]auth.Principal{
		"a member":             {TenantID: mi.Tenant, Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		"an isolated user":     {TenantID: mi.Tenant, Subject: "u", Scopes: []string{auth.ScopeUser}},
		"a run-plane operator": {TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}},
	} {
		ctx := auth.WithPrincipal(context.Background(), p)
		if code, out := s.jobRoute(t, ctx, http.MethodGet, "/v1/_ontology/job", ""); code != http.StatusForbidden || out["code"] != "forbidden" {
			t.Errorf("%s's status: %d %v, want 403 forbidden", what, code, out)
		}
		if code, out := s.jobRoute(t, ctx, http.MethodPost, "/v1/_ontology/job/resume", ""); code != http.StatusForbidden || out["code"] != "forbidden" {
			t.Errorf("%s's resume: %d %v, want 403 forbidden", what, code, out)
		}
	}
}

// TestOntologyJobRoute_StatusAndResumeOfTheTenantsJob — with no job the status is
// {"job": null} and a resume is 404 job_not_found; a class rename's finished job is the
// status; a job left unfinished is resumed to done, a resume naming another job is 404
// and leaves it unfinished, and a malformed body is 400 invalid_body.
func TestOntologyJobRoute_StatusAndResumeOfTheTenantsJob(t *testing.T) {
	s, mi := ontologyFixture(t)
	provision := installPlanOntology(t, s, mi)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})

	code, out := s.jobRoute(t, op, http.MethodGet, "/v1/_ontology/job", "")
	if job, present := out["job"]; code != http.StatusOK || out["tenant"] != mi.Tenant || !present || job != nil {
		t.Fatalf("status with no job = %d %v, want 200 job null", code, out)
	}
	if code, out := s.jobRoute(t, op, http.MethodPost, "/v1/_ontology/job/resume", ""); code != http.StatusNotFound || out["code"] != "job_not_found" {
		t.Errorf("resume with no job = %d %v, want 404 job_not_found", code, out)
	}

	renamed := s.renameProvision(t, mi, provision, "Clause")
	id := dig(renamed, "rename_job", "id")
	code, out = s.jobRoute(t, op, http.MethodGet, "/v1/_ontology/job", "")
	if code != http.StatusOK || dig(out, "job", "id") != id || dig(out, "job", "state") != "done" ||
		dig(out, "job", "kind") != "class_rename" || dig(out, "job", "to") != "Clause" {
		t.Fatalf("status after the rename = %d %v, want job %v done", code, out, id)
	}

	s.setJobState(t, mi, "running")
	if code, out := s.jobRoute(t, op, http.MethodPost, "/v1/_ontology/job/resume", `{"id":"another"}`); code != http.StatusNotFound || out["code"] != "job_not_found" {
		t.Errorf("resume of another id = %d %v, want 404 job_not_found", code, out)
	}
	if _, out := s.jobRoute(t, op, http.MethodGet, "/v1/_ontology/job", ""); dig(out, "job", "state") != "running" {
		t.Errorf("after a refused resume the job = %v, want still running", out)
	}
	if code, out := s.jobRoute(t, op, http.MethodPost, "/v1/_ontology/job/resume", `{"id":1}`); code != http.StatusBadRequest || out["code"] != "invalid_body" {
		t.Errorf("a malformed body = %d %v, want 400 invalid_body", code, out)
	}
	code, out = s.jobRoute(t, op, http.MethodPost, "/v1/_ontology/job/resume", `{"id":"`+id.(string)+`"}`)
	if code != http.StatusOK || dig(out, "job", "id") != id || dig(out, "job", "state") != "done" {
		t.Errorf("resume = %d %v, want job %v done", code, out, id)
	}
}
