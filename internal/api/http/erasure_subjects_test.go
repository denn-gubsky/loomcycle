package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// boundSubjectFixture is a server wired as boot wires it (a channel bus, an erasure
// audit sink) holding one tenant subject, person:ada, bound to the principal u-ada.
func boundSubjectFixture(t *testing.T) (s *Server, mi memInject, docID string, sink *recordingSink) {
	t.Helper()
	s, mi = ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	sink = &recordingSink{}
	s.SetEraseAudit(sink)

	dctx := s.ontologyDocCtx(context.Background(), memInject{Tenant: mi.Tenant})
	doc := s.ontologyDocument()
	res, err := doc.Execute(dctx, json.RawMessage(`{"op":"create_document","scope":"tenant","title":"Ada",
		"path":"/facts/ada","type":"person","subject":"Ada","natural_key":"person:ada"}`))
	if err != nil || res.IsError {
		t.Fatalf("create subject: %v %s", err, res.Text)
	}
	var out struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	if _, err := doc.BindTenantSubjectPrincipal(dctx, "person:ada", "u-ada"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return s, mi, out.DocumentID, sink
}

func erasurePrincipal(tenant string, eff *authz.Effective) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant, Subject: "operator", Scopes: []string{auth.ScopeTenant}, Effective: eff,
	})
}

func eraseAs(t *testing.T, s *Server, ctx context.Context, tenant string, body map[string]any) erasure.Result {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_erasure?tenant="+tenant, bytes.NewReader(b)).WithContext(ctx)
	s.handleErasureExecute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/_erasure = %d: %s", rec.Code, rec.Body.String())
	}
	var res erasure.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func (s *Server) boundTo(t *testing.T, tenant, principal string) []builtin.SubjectBinding {
	t.Helper()
	got, err := s.ontologyDocument().TenantSubjectsBoundTo(s.ontologyDocCtx(context.Background(), memInject{Tenant: tenant}), principal)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestErasure_ReportsTheSubjectsBoundToThePrincipal: the report and the dry run name
// each tenant subject bound to the subject, and the dry run changes nothing.
func TestErasure_ReportsTheSubjectsBoundToThePrincipal(t *testing.T) {
	s, mi, docID, _ := boundSubjectFixture(t)
	ctx := erasurePrincipal(mi.Tenant, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant="+mi.Tenant+"&subject=u-ada", nil).WithContext(ctx)
	s.handleErasureReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/_erasure = %d: %s", rec.Code, rec.Body.String())
	}
	var rep erasure.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	want := erasure.BoundSubject{DocumentID: docID, NaturalKey: "person:ada", Path: "/facts/ada"}
	if len(rep.BoundSubjects) != 1 || rep.BoundSubjects[0].DocumentID != want.DocumentID ||
		rep.BoundSubjects[0].NaturalKey != want.NaturalKey || rep.BoundSubjects[0].Path != want.Path {
		t.Errorf("report bound_subjects = %+v, want [%+v]", rep.BoundSubjects, want)
	}
	if rep.Tier2.Counts["subject_bindings"] != 1 {
		t.Errorf("tier2 subject_bindings = %v", rep.Tier2.Counts)
	}

	res := eraseAs(t, s, ctx, mi.Tenant, map[string]any{"subject": "u-ada"})
	if !res.DryRun || len(res.BoundSubjects) != 1 || res.BoundSubjects[0].DocumentID != docID ||
		res.Deleted["subject_bindings"] != 1 {
		t.Errorf("dry run = bound %+v deleted %v", res.BoundSubjects, res.Deleted)
	}
	if got := s.boundTo(t, mi.Tenant, "u-ada"); len(got) != 1 {
		t.Errorf("a dry run changed the binding: %+v", got)
	}
	if msgs := s.subjectMessages(t); len(msgs) != 0 {
		t.Errorf("a dry run announced: %d message(s)", len(msgs))
	}
}

// TestErasure_UnbindsAndAnnouncesTheSubjectButKeepsIt.
//
// The real run clears the binding so no row keeps the erased id, publishes `erased`
// naming the subject and never the principal, deletes no subject, and says in
// Retained that deleting one is the operator's call. The audit record counts the
// subjects and names none of them.
func TestErasure_UnbindsAndAnnouncesTheSubjectButKeepsIt(t *testing.T) {
	s, mi, docID, sink := boundSubjectFixture(t)
	ctx := erasurePrincipal(mi.Tenant, nil)

	res := eraseAs(t, s, ctx, mi.Tenant, map[string]any{"subject": "u-ada", "dry_run": false, "confirm": "u-ada"})
	if res.Deleted["subject_bindings"] != 1 || len(res.BoundSubjects) != 1 || res.BoundSubjects[0].DocumentID != docID {
		t.Errorf("result = bound %+v deleted %v", res.BoundSubjects, res.Deleted)
	}
	if res.NotAnnounced != "" {
		t.Errorf("not announced for an unrestricted operator: %s", res.NotAnnounced)
	}
	retained := res.Retained["bound_subjects"]
	if !strings.Contains(retained, "operator's call") || !strings.Contains(retained, "delete_document") {
		t.Errorf("retained[bound_subjects] = %q, want the operator's-call sentence", retained)
	}
	if got := s.boundTo(t, mi.Tenant, "u-ada"); len(got) != 0 {
		t.Errorf("still bound after the erasure: %+v", got)
	}
	dctx := s.ontologyDocCtx(context.Background(), memInject{Tenant: mi.Tenant})
	if r, err := s.ontologyDocument().Execute(dctx, json.RawMessage(`{"op":"get_document","scope":"tenant","id":"`+docID+`"}`)); err != nil || r.IsError {
		t.Errorf("the erasure deleted the subject: %v %s", err, r.Text)
	}

	msgs := s.subjectMessages(t)
	if len(msgs) != 1 {
		t.Fatalf("memory/subjects messages = %d, want one erased", len(msgs))
	}
	if whole, _ := json.Marshal(msgs[0]); bytes.Contains(whole, []byte("u-ada")) {
		t.Errorf("the erased principal is on the channel: %s", whole)
	}
	if ev := subjectPayload(t, msgs[0]); ev["op"] != "erased" || ev["subject_natural_key"] != "person:ada" || ev["document_id"] != docID {
		t.Errorf("event = %v", ev)
	}

	var result []byte
	for _, ev := range sink.events {
		if ev.Action == "erase_result" {
			if ev.ErasePlanes["subject_bindings"] != 1 {
				t.Errorf("erase_result planes = %v, want subject_bindings 1", ev.ErasePlanes)
			}
			result, _ = json.Marshal(ev)
		}
	}
	if result == nil {
		t.Fatal("no erase_result record")
	}
	for _, name := range []string{docID, "person:ada", "/facts/ada"} {
		if bytes.Contains(result, []byte(name)) {
			t.Errorf("the audit record names a bound subject (%s): %s", name, result)
		}
	}
}

// TestErasure_ReadingLimitedOperatorUnbindsWithoutNamingOrAnnouncing.
//
// An operator whose rules hide the subject's document from reading still clears the
// binding (the erasure must not miss it), but the subject is counted rather than named,
// and `erased`, which every reader of the channel sees, is withheld with the reason.
func TestErasure_ReadingLimitedOperatorUnbindsWithoutNamingOrAnnouncing(t *testing.T) {
	s, mi, _, _ := boundSubjectFixture(t)
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: mi.Tenant, ID: "operator"},
		Bindings: []authz.Binding{{ACL: "limited", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"limited": {ACL: authz.ACL{Name: "limited", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:tenant"}},
			{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/facts/**"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := erasurePrincipal(mi.Tenant, eff)
	if tools.CallerACL(ctx) == nil {
		t.Fatal("the principal's rules are not the caller's")
	}

	res := eraseAs(t, s, ctx, mi.Tenant, map[string]any{"subject": "u-ada", "dry_run": false, "confirm": "u-ada"})
	if res.Deleted["subject_bindings"] != 1 || len(res.BoundSubjects) != 0 || res.BoundSubjectsWithheld != 1 {
		t.Errorf("result = bound %+v withheld %d deleted %v; want one unbound, counted, unnamed",
			res.BoundSubjects, res.BoundSubjectsWithheld, res.Deleted)
	}
	if res.NotAnnounced == "" {
		t.Error("the result does not say why `erased` was not announced")
	}
	if got := s.boundTo(t, mi.Tenant, "u-ada"); len(got) != 0 {
		t.Errorf("still bound after the erasure: %+v", got)
	}
	if msgs := s.subjectMessages(t); len(msgs) != 0 {
		t.Errorf("announced for a reading-limited operator: %d message(s)", len(msgs))
	}
}
