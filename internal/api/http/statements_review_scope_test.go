package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Reviewing another subject's user documents (scope user, scope_id): an
// operator's reach within its own tenant, never a member's, never another
// tenant's.

// seedUserProposal files one proposal in subject's user scope of tenant.
func seedUserProposal(t *testing.T, s *Server, tenant, subject string) string {
	t.Helper()
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	ctx := s.docToolCtx(context.Background(), memInject{Tenant: tenant, UserID: subject})
	call := func(body string) map[string]any {
		res, err := doc.Execute(ctx, json.RawMessage(body))
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", body, err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	chunk := func(path string) string {
		d := call(fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":%q}`, path, path))["document_id"].(string)
		return call(fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"part","body":"text"}`, d))["id"].(string)
	}
	a, b := chunk("/notes/a"), chunk("/notes/b")
	return call(fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))["id"].(string)
}

func (s *Server) getRulings(t *testing.T, ctx context.Context, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_statements/rulings?"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleStatementRulings(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// userStatementStatus reads a statement of subject's user scope as its owner.
func (s *Server) userStatementStatus(t *testing.T, tenant, subject, id string) string {
	t.Helper()
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	res, err := doc.Execute(s.docToolCtx(context.Background(), memInject{Tenant: tenant, UserID: subject}),
		json.RawMessage(`{"op":"get_edge","scope":"user","id":"`+id+`"}`))
	if err != nil || res.IsError {
		t.Fatalf("get_edge %s: %v %s", id, err, res.Text)
	}
	var out struct {
		Statement struct {
			Status string `json:"status"`
		} `json:"statement"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out.Statement.Status
}

// A tenant operator queues, rules, lists and reverts on a member's user
// scope; the ruling is recorded as the operator's, and audited with whose
// scope it was.
func TestStatementReviewAPI_AnOperatorReviewsAnotherSubjectsUserScope(t *testing.T) {
	s, mi := ontologyFixture(t)
	sink := &recordingSink{}
	s.SetStatementReviewAudit(sink)
	stmt := seedUserProposal(t, s, mi.Tenant, "bob")
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})

	if code, out := s.getQueue(t, op, "scope=user"); code != http.StatusOK || len(out["statements"].([]any)) != 0 {
		t.Fatalf("the operator's own user queue = %d %v, want empty", code, out)
	}
	code, out := s.getQueue(t, op, "scope=user&scope_id=bob")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out["statements"]), stmt) {
		t.Fatalf("bob's queue for the operator = %d %v", code, out)
	}
	ruled := s.ruleBoth(t, op, map[string]any{"action": "accept", "scope": "user", "scope_id": "bob", "ids": []string{stmt}})
	if ruled["ruled"] != float64(1) || s.userStatementStatus(t, mi.Tenant, "bob", stmt) != "asserted" {
		t.Fatalf("ruling on bob's scope = %v", ruled)
	}
	if len(sink.events) != 1 || sink.events[0].RulingScope != "user" || sink.events[0].RulingScopeID != "bob" || sink.events[0].ActorSubject != "ops" {
		t.Errorf("audit = %+v, want scope user of bob, by ops", sink.events)
	}
	code, list := s.getRulings(t, op, "scope=user&scope_id=bob")
	rulings, _ := list["rulings"].([]any)
	if code != http.StatusOK || len(rulings) != 1 || rulings[0].(map[string]any)["actor"] != "ops" || rulings[0].(map[string]any)["id"] != ruled["ruling_id"] {
		t.Errorf("bob's rulings for the operator = %d %v, want the ruling, made by ops", code, list)
	}
	code, rev := s.postReview(t, op, "/v1/_statements/revert", map[string]any{"ruling_id": ruled["ruling_id"], "scope": "user", "scope_id": "bob"})
	if code != http.StatusOK || rev["reverted"] != float64(1) || s.userStatementStatus(t, mi.Tenant, "bob", stmt) != "proposed" {
		t.Errorf("revert on bob's scope = %d %v", code, rev)
	}
}

// A member may not name another subject on any review route, even holding
// review everywhere; naming itself is its own scope. Nothing changes.
func TestStatementReviewAPI_AMemberMayNotNameAnotherSubject(t *testing.T) {
	s, mi := ontologyFixture(t)
	stmt := seedUserProposal(t, s, mi.Tenant, "carol")
	member := reviewMember(t, mi.Tenant, authz.Rule{Effect: authz.Allow, Actions: []string{"review"}, Resources: []string{"document:*"}})
	if code, out := s.getQueue(t, member, "scope=user&scope_id=carol"); code != http.StatusForbidden || out["code"] != "forbidden" {
		t.Errorf("a member's queue of carol = %d %v, want 403 forbidden", code, out)
	}
	if code, out := s.getRulings(t, member, "scope=user&scope_id=carol"); code != http.StatusForbidden || out["code"] != "forbidden" {
		t.Errorf("a member's rulings of carol = %d %v, want 403 forbidden", code, out)
	}
	for _, body := range []map[string]any{
		{"action": "accept", "scope": "user", "scope_id": "carol", "ids": []string{stmt}},
		{"action": "accept", "scope": "user", "scope_id": "carol", "ids": []string{stmt}, "dry_run": false, "expect_count": 1},
	} {
		if code, out := s.postReview(t, member, "/v1/_statements/rule", body); code != http.StatusForbidden || out["code"] != "forbidden" {
			t.Errorf("a member's ruling on carol %v = %d %v, want 403 forbidden", body, code, out)
		}
	}
	if code, out := s.postReview(t, member, "/v1/_statements/revert", map[string]any{"ruling_id": "r", "scope": "user", "scope_id": "carol"}); code != http.StatusForbidden || out["code"] != "forbidden" {
		t.Errorf("a member's revert on carol = %d %v, want 403 forbidden", code, out)
	}
	if got := s.userStatementStatus(t, mi.Tenant, "carol", stmt); got != "proposed" {
		t.Errorf("carol's statement = %s after a member's refused calls", got)
	}
	// Naming itself is no other subject.
	if code, out := s.getQueue(t, member, "scope=user&scope_id=bob"); code != http.StatusOK {
		t.Errorf("a member naming itself = %d %v", code, out)
	}
}

// scope_id never crosses tenants: a tenant operator naming another tenant is
// refused rather than focused, and that tenant's subject is untouched; an
// admin reaches it.
func TestStatementReviewAPI_ScopeIDNeverCrossesTenants(t *testing.T) {
	s, mi := ontologyFixture(t)
	stmt := seedUserProposal(t, s, "globex", "dave")
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	if code, out := s.getQueue(t, op, "scope=user&scope_id=dave&tenant=globex"); code != http.StatusForbidden || out["code"] != "forbidden" {
		t.Errorf("a tenant operator's queue of another tenant's subject = %d %v, want 403 forbidden", code, out)
	}
	if code, out := s.getRulings(t, op, "scope=user&scope_id=dave&tenant=globex"); code != http.StatusForbidden {
		t.Errorf("a tenant operator's rulings of another tenant's subject = %d %v, want 403", code, out)
	}
	if code, out := s.postReview(t, op, "/v1/_statements/rule", map[string]any{"action": "accept", "scope": "user", "scope_id": "dave", "tenant": "globex", "ids": []string{stmt}}); code != http.StatusForbidden {
		t.Errorf("a tenant operator's ruling in another tenant = %d %v, want 403", code, out)
	}
	if code, out := s.postReview(t, op, "/v1/_statements/revert", map[string]any{"ruling_id": "r", "scope": "user", "scope_id": "dave", "tenant": "globex"}); code != http.StatusForbidden {
		t.Errorf("a tenant operator's revert in another tenant = %d %v, want 403", code, out)
	}
	// The same subject name in its own tenant is another person, with nothing filed.
	if code, out := s.getQueue(t, op, "scope=user&scope_id=dave"); code != http.StatusOK || len(out["statements"].([]any)) != 0 {
		t.Errorf("dave of the operator's own tenant = %d %v, want an empty queue", code, out)
	}
	if got := s.userStatementStatus(t, "globex", "dave", stmt); got != "proposed" {
		t.Errorf("globex/dave's statement = %s", got)
	}
	admin := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	if code, out := s.getQueue(t, admin, "scope=user&scope_id=dave&tenant=globex"); code != http.StatusOK || !strings.Contains(fmt.Sprint(out["statements"]), stmt) {
		t.Errorf("an admin's queue of globex/dave = %d %v", code, out)
	}
}

// scope_id names a user scope: with the tenant scope it is refused.
func TestStatementReviewAPI_ScopeIDNeedsTheUserScope(t *testing.T) {
	s, mi := ontologyFixture(t)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	if _, err := s.ListReviewQueue(op, connector.ListReviewQueueRequest{ScopeID: "bob"}); connector.StatementReviewCode(err) != "invalid_request" {
		t.Errorf("scope_id with the tenant scope: %v, want invalid_request", err)
	}
}
