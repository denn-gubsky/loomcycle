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

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// seedMergeSubjects files two tenant subjects for one person and a report that
// cites the duplicate, as the operator's Document would. Returns the duplicate's,
// the survivor's and the report's document ids.
func seedMergeSubjects(t *testing.T, s *Server, mi memInject) (dup, surv, report string) {
	t.Helper()
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	ctx := withTenantDocGrants(s.docToolCtx(context.Background(), mi))
	call := func(body string) map[string]any {
		res, err := doc.Execute(ctx, json.RawMessage(body))
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", body, err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	subject := func(title, slug, key string) (string, string) {
		out := call(fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":"/facts/%s","type":"person","subject":%q,"natural_key":%q}`,
			title, slug, title, key))
		return out["document_id"].(string), out["root_chunk_id"].(string)
	}
	dup, dupRoot := subject("Ada King", "ada-king", "person:ada-king")
	surv, _ = subject("Ada Lovelace", "ada-lovelace", "person:ada-lovelace")
	call(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Wrote the notes","body":"She wrote the notes."}`, dup))
	report = call(`{"op":"create_document","scope":"tenant","title":"Report","path":"/kb/report"}`)["document_id"].(string)
	cite := call(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Summary","body":"Ada King wrote the notes."}`, report))["id"].(string)
	call(fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, cite, dupRoot))
	return dup, surv, report
}

// postMerge drives POST /v1/_ontology/subjects/merge as the principal on ctx.
func (s *Server) postMerge(t *testing.T, ctx context.Context, tenant, dup, surv string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"duplicate": dup, "survivor": surv})
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/subjects/merge?tenant="+tenant, strings.NewReader(string(body))).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleOntologySubjectMerge(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestOntologyAPI_MergeAnnouncesTheMerge (RFC FR §3, P2).
//
// The route runs the merge through the operator plane's Document, which carries
// the server's channel bus: the `merged` event lands on memory/subjects naming
// both subjects, after the duplicate's root records the survivor. Driven on a
// server wired as boot wires it (a channel bus), read from the store — a test
// that set the bus by hand is how the adoption event went unnoticed.
func TestOntologyAPI_MergeAnnouncesTheMerge(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	dup, surv, _ := seedMergeSubjects(t, s, mi)

	code, out := s.postMerge(t, context.Background(), mi.Tenant, "person:ada-king", surv)
	if code != http.StatusOK {
		t.Fatalf("merge = %d: %v", code, out)
	}
	if out["tenant"] != mi.Tenant || out["facts_moved"] != float64(1) || out["statements_repointed"] != float64(1) || out["not_announced"] != nil {
		t.Errorf("response = %v", out)
	}
	msgs := s.subjectMessages(t)
	if len(msgs) != 1 {
		t.Fatalf("memory/subjects messages = %d, want the merge", len(msgs))
	}
	ev := subjectPayload(t, msgs[0])
	if ev["op"] != "merged" || ev["document_id"] != dup || ev["surviving_document_id"] != surv ||
		ev["subject_natural_key"] != "person:ada-king" || ev["surviving_key"] != "person:ada-lovelace" {
		t.Errorf("event = %v", ev)
	}
	if msgs[0].TenantID != mi.Tenant {
		t.Errorf("message tenant = %q, want %q", msgs[0].TenantID, mi.Tenant)
	}
}

// TestOntologyAPI_MergeRefusalsCarryTheirCodes: each refusal is its own status and
// code, so a client can tell "not a subject" from "already merged".
func TestOntologyAPI_MergeRefusalsCarryTheirCodes(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	dup, surv, report := seedMergeSubjects(t, s, mi)
	ctx := context.Background()

	for _, tc := range []struct {
		name, dup, surv string
		status          int
		code            string
	}{
		{"same", dup, dup, http.StatusBadRequest, "same_subject"},
		{"not a subject", report, surv, http.StatusBadRequest, "not_a_subject"},
		{"unknown key", "person:nobody", surv, http.StatusNotFound, "subject_not_in_scope"},
		{"missing", "", surv, http.StatusBadRequest, "invalid_request"},
	} {
		code, out := s.postMerge(t, ctx, mi.Tenant, tc.dup, tc.surv)
		if code != tc.status || out["code"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", tc.name, code, out, tc.status, tc.code)
		}
	}
	if code, out := s.postMerge(t, ctx, mi.Tenant, dup, surv); code != http.StatusOK {
		t.Fatalf("merge = %d %v", code, out)
	}
	if code, out := s.postMerge(t, ctx, mi.Tenant, dup, surv); code != http.StatusConflict || out["code"] != "already_merged" {
		t.Errorf("re-merge = %d %v, want 409 already_merged", code, out)
	}
	if n := len(s.subjectMessages(t)); n != 1 {
		t.Errorf("messages = %d, want only the one merge", n)
	}
}

// TestOntologyAPI_MergeIsForOperators (RFC FR §9): a token without the tenant
// operator's scope, or one held to the run plane, is refused by the connector
// itself — the gate gRPC and MCP reach past the HTTP route's.
func TestOntologyAPI_MergeIsForOperators(t *testing.T) {
	s, mi := ontologyFixture(t)
	dup, surv, _ := seedMergeSubjects(t, s, mi)
	for what, p := range map[string]auth.Principal{
		"a member":             {TenantID: mi.Tenant, Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		"an isolated user":     {TenantID: mi.Tenant, Subject: "u", Scopes: []string{auth.ScopeUser}},
		"a run-plane operator": {TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}},
	} {
		_, err := s.MergeSubjects(auth.WithPrincipal(context.Background(), p), connector.MergeSubjectsRequest{Duplicate: dup, Survivor: surv})
		if connector.SubjectMergeCode(err) != "forbidden" {
			t.Errorf("%s: err = %v, want forbidden", what, err)
		}
	}
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})
	if res, err := s.MergeSubjects(op, connector.MergeSubjectsRequest{Tenant: "elsewhere", Duplicate: dup, Survivor: surv}); err != nil || res.Tenant != mi.Tenant {
		t.Errorf("a tenant operator naming another tenant: %+v %v, want its own tenant's merge", res, err)
	}
}

// TestOntologyAPI_MergeByAReadingLimitedOperatorIsNotAnnounced (RFC FR §2, §10):
// an operator whose access-control rules hide part of the tenant scope from
// reading merges, and is told why nothing was announced.
func TestOntologyAPI_MergeByAReadingLimitedOperatorIsNotAnnounced(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	dup, surv, _ := seedMergeSubjects(t, s, mi)
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: mi.Tenant, ID: "o"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"memory:*", "sqlmem:*"}},
			{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:*"}},
			{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Effective: eff})

	code, out := s.postMerge(t, ctx, mi.Tenant, dup, surv)
	if code != http.StatusOK {
		t.Fatalf("merge = %d %v", code, out)
	}
	if why, _ := out["not_announced"].(string); why == "" {
		t.Errorf("response = %v, want the reason it was not announced", out)
	}
	if n := len(s.subjectMessages(t)); n != 0 {
		t.Errorf("announced under a partitioned scope: %d messages", n)
	}
}
