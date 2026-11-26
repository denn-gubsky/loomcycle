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
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// RFC GQ P5.6 — ontology ops job and resume_job, the MCP twins of GET
// /v1/_ontology/job and POST /v1/_ontology/job/resume.

// The job rewrites stored rows in every scope of the tenant, so its status and its
// resume are the operator's: each op's twin route is carved out of the member path
// and asks the route's question (read document for the status, write document for
// the resume), and a member's and an isolated user's session may not call the tool.
func TestOntologyTool_JobOpsAreRefusedAMemberAndAnIsolatedUser(t *testing.T) {
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	as := func(scopes []string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes})
	}
	for _, c := range []struct{ op, method, path, action string }{
		{"job", http.MethodGet, "/v1/_ontology/job", "read"},
		{"resume_job", http.MethodPost, "/v1/_ontology/job/resume", "write"},
	} {
		tw, ok := mcpToolTwins["ontology"].Ops[c.op]
		if !ok || tw.Method != c.method || tw.Path != c.path {
			t.Errorf("ontology %s twin = %+v (%v), want %s %s", c.op, tw, ok, c.method, c.path)
			continue
		}
		if !memberpath.CarvedOut(tw.Method, tw.Path) {
			t.Errorf("ontology %s: its route %s is not carved out of the member path", c.op, tw.Path)
		}
		q, ok, err := mcpTwinSurface("ontology", json.RawMessage(`{"op":"`+c.op+`"}`))
		route, routed := authz.HTTPSurface(c.method, c.path)
		if err != nil || !ok || !routed || q != route || q.Action != c.action || q.Type != "document" {
			t.Errorf("ontology %s asks %+v (%v %v), its route %+v; want %s document on both", c.op, q, ok, err, route, c.action)
		}
	}
	for what, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"a member":          {as(memberScopes), false},
		"an isolated user":  {as([]string{auth.ScopeUser}), false},
		"a tenant operator": {as([]string{auth.ScopeTenant}), true},
	} {
		if got := principalMayCallTool(tc.ctx, "ontology"); got != tc.want {
			t.Errorf("%s may call the ontology tool = %v, want %v", what, got, tc.want)
		}
	}
}

// ontology op=job and op=resume_job hand the tenant (and the resume's id) to the
// connector as HTTP's routes do and return its answer unchanged, {"job": null}
// included; a refusal comes back as HTTP's {code, error}, an internal failure without
// its text, and an id that is not a string is invalid_request before the connector.
func TestOntologyTool_JobOpsMatchTheConnector(t *testing.T) {
	res := connector.OntologyJobResult{Tenant: "acme", Job: &connector.OntologyJobStatus{
		ID: "j1", Kind: "concept_retag", Entry: "c1", From: []string{"topics/risk"}, To: "topics/reporting/risk",
		ToTags: []string{"topics/reporting/risk"}, State: "running", Step: "document_tags", Done: map[string]int{"chunk_tags": 4},
		Remain: 7, Scope: "user", ScopesDone: 1, ScopesTotal: 3, CreatedAt: 10, UpdatedAt: 20,
	}}
	mc := &mockConnector{jobRes: res}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		out, err := handleOntology(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	answer := func(r *loommcp.CallToolResult) connector.OntologyJobResult {
		t.Helper()
		var got connector.OntologyJobResult
		if r.IsError || json.Unmarshal([]byte(r.Content[0].Text), &got) != nil {
			t.Fatalf("answer %s", r.Content[0].Text)
		}
		return got
	}

	if got := answer(call(`{"op":"job","tenant":"globex"}`)); !reflect.DeepEqual(got, res) {
		t.Errorf("job = %+v, want the connector's", got)
	}
	if mc.jobGetReq != (connector.GetOntologyJobRequest{Tenant: "globex"}) {
		t.Errorf("job asked the connector %+v", mc.jobGetReq)
	}
	if got := answer(call(`{"op":"resume_job","id":"j1","tenant":"globex"}`)); !reflect.DeepEqual(got, res) {
		t.Errorf("resume_job = %+v, want the connector's", got)
	}
	if mc.jobResumeReq != (connector.ResumeOntologyJobRequest{Tenant: "globex", ID: "j1"}) {
		t.Errorf("resume_job asked the connector %+v", mc.jobResumeReq)
	}
	mc.jobRes = connector.OntologyJobResult{Tenant: "acme"}
	if r := call(`{"op":"job"}`); r.IsError || !strings.Contains(r.Content[0].Text, `"job":null`) {
		t.Errorf("job with none = %s, want job null", r.Content[0].Text)
	}

	refusal := func(r *loommcp.CallToolResult) (code, msg string) {
		var e struct{ Code, Error string }
		if !r.IsError || json.Unmarshal([]byte(r.Content[0].Text), &e) != nil {
			return "", r.Content[0].Text
		}
		return e.Code, e.Error
	}
	mc.jobResumeReq = connector.ResumeOntologyJobRequest{}
	if code, msg := refusal(call(`{"op":"resume_job","id":7}`)); code != "invalid_request" || mc.jobResumeReq != (connector.ResumeOntologyJobRequest{}) {
		t.Errorf("an id that is not a string = %s %q, connector got %+v; want invalid_request before the connector", code, msg, mc.jobResumeReq)
	}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{connector.WithMessage(connector.ErrOntologyJobForbidden, "for an operator"), "forbidden"},
		{connector.WithMessage(connector.ErrOntologyJobNotFound, "no unfinished job"), "job_not_found"},
		{connector.WithMessage(connector.ErrOntologyJobBusy, "a rename is running"), "job_busy"},
		{connector.WithMessage(connector.ErrOntologyJobUnavailable, "no SQL Memory"), "unavailable"},
		{errors.New("open /var/lib/secret.db: denied"), "job_failed"},
	} {
		mc.jobErr = tc.err
		for _, args := range []string{`{"op":"job"}`, `{"op":"resume_job"}`} {
			code, msg := refusal(call(args))
			if code != tc.code {
				t.Errorf("%s with %v: code %q (%s), want %s", args, tc.err, code, msg, tc.code)
			}
			if strings.Contains(msg, "/var/lib") {
				t.Errorf("an internal failure's text reached the caller: %s", msg)
			}
		}
	}
}
