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
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// reviewSeed is a tenant with one proposal under /board and one under /kb.
type reviewSeed struct{ board, kb string }

func seedReviewQueue(t *testing.T, s *Server, mi memInject) reviewSeed {
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
	chunk := func(path string) string {
		d := call(fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q}`, path, path))["document_id"].(string)
		return call(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"part","body":"text"}`, d))["id"].(string)
	}
	one, two, notes := chunk("/board/reg/one"), chunk("/board/reg/two"), chunk("/kb/notes/n")
	propose := func(from, to string) string {
		return call(fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, from, to))["id"].(string)
	}
	return reviewSeed{board: propose(one, two), kb: propose(notes, two)}
}

// reviewMember is a member of tenant with rules (plus read, write and link on
// every document and memory).
func reviewMember(t *testing.T, tenant string, rules ...authz.Rule) context.Context {
	t.Helper()
	base := []authz.Rule{
		{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"memory:*", "sqlmem:*"}},
		{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:*"}},
	}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: tenant, ID: "bob"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: append(base, rules...)}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: "bob",
		Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, Effective: eff})
}

func (s *Server) postReview(t *testing.T, ctx context.Context, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b))).WithContext(ctx)
	rec := httptest.NewRecorder()
	switch path {
	case "/v1/_statements/rule":
		s.handleStatementRule(rec, req)
	default:
		s.handleStatementRevert(rec, req)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (s *Server) getQueue(t *testing.T, ctx context.Context, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_statements/review?"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleStatementReviewQueue(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ruleBoth runs the dry run, then the real ruling with its count.
func (s *Server) ruleBoth(t *testing.T, ctx context.Context, body map[string]any) map[string]any {
	t.Helper()
	code, dry := s.postReview(t, ctx, "/v1/_statements/rule", body)
	if code != http.StatusOK {
		t.Fatalf("dry run = %d %v", code, dry)
	}
	body["dry_run"], body["expect_count"] = false, dry["matched"]
	code, out := s.postReview(t, ctx, "/v1/_statements/rule", body)
	if code != http.StatusOK {
		t.Fatalf("rule = %d %v", code, out)
	}
	return out
}

func TestStatementReviewAPI_RulingAndRevertWriteOneAuditRecordEach(t *testing.T) {
	s, mi := ontologyFixture(t)
	sink := &recordingSink{}
	s.SetStatementReviewAudit(sink)
	seed := seedReviewQueue(t, s, mi)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", TokenSuffix: "x9", Scopes: []string{auth.ScopeTenant}})

	// A dry run is not audited.
	if code, out := s.postReview(t, op, "/v1/_statements/rule", map[string]any{"action": "reject", "reason": "x", "ids": []string{seed.board}}); code != http.StatusOK || len(sink.events) != 0 {
		t.Fatalf("dry run = %d %v, %d audit records", code, out, len(sink.events))
	}
	out := s.ruleBoth(t, op, map[string]any{"action": "reject", "reason": "points at the amending act", "ids": []string{seed.board, seed.kb}})
	if len(sink.events) != 1 {
		t.Fatalf("audit records = %d, want one for the ruling", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Action != "statement_ruling" || ev.RulingID != out["ruling_id"] || ev.RulingID == "" || ev.ActorSubject != "ops" || ev.ActorTenant != mi.Tenant ||
		ev.ActorTokenSuffix != "x9" || ev.TargetTenant != mi.Tenant || ev.RulingAction != "reject" || ev.RulingReason != "points at the amending act" ||
		len(ev.RulingIDs) != 2 || ev.RulingCounts["ruled"] != 2 || ev.RulingCounts["matched"] != 2 || ev.RulingScope != "tenant" {
		t.Errorf("audit record = %+v (result %v)", ev, out)
	}
	code, rev := s.postReview(t, op, "/v1/_statements/revert", map[string]any{"ruling_id": out["ruling_id"]})
	if code != http.StatusOK || rev["reverted"] != float64(2) {
		t.Fatalf("revert = %d %v", code, rev)
	}
	if len(sink.events) != 2 || sink.events[1].Action != "statement_ruling_revert" || sink.events[1].RulingID != out["ruling_id"] ||
		sink.events[1].RulingCounts["reverted"] != 2 || sink.events[1].ActorSubject != "ops" {
		t.Errorf("revert audit = %+v", sink.events)
	}
}

func TestStatementReviewAPI_MemberRulesOnlyWhereItHoldsReview(t *testing.T) {
	s, mi := ontologyFixture(t)
	seed := seedReviewQueue(t, s, mi)
	reviewer := reviewMember(t, mi.Tenant, authz.Rule{Effect: authz.Allow, Actions: []string{"review"}, Resources: []string{"document:tenant:/board/**"}})
	out := s.ruleBoth(t, reviewer, map[string]any{"action": "accept", "filter": map[string]any{}})
	if out["ruled"] != float64(1) || out["skipped"] != float64(1) || strings.Contains(fmt.Sprint(out), seed.kb) {
		t.Errorf("reviewer ruling = %v, want the /board one ruled and the /kb one only counted", out)
	}
	// Without the grant anywhere, the member is refused outright on both writes;
	// the queue stays readable.
	plain := reviewMember(t, mi.Tenant)
	for _, path := range []string{"/v1/_statements/rule", "/v1/_statements/revert"} {
		code, body := s.postReview(t, plain, path, map[string]any{"action": "accept", "ids": []string{seed.kb}, "ruling_id": "r"})
		if code != http.StatusForbidden || body["code"] != "forbidden" {
			t.Errorf("%s by a member without review = %d %v, want 403 forbidden", path, code, body)
		}
	}
	if code, body := s.getQueue(t, plain, ""); code != http.StatusOK {
		t.Errorf("queue for a member = %d %v", code, body)
	}
}

func TestStatementReviewAPI_IsolatedUsersAndRunPlaneTokensAreRefused(t *testing.T) {
	s, mi := ontologyFixture(t)
	seedReviewQueue(t, s, mi)
	for what, p := range map[string]auth.Principal{
		"an isolated user":     {TenantID: mi.Tenant, Subject: "u", Scopes: []string{auth.ScopeUser}},
		"a run-plane operator": {TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}},
	} {
		ctx := auth.WithPrincipal(context.Background(), p)
		if _, err := s.ListReviewQueue(ctx, connector.ListReviewQueueRequest{}); connector.StatementReviewCode(err) != "forbidden" {
			t.Errorf("%s: queue err = %v, want forbidden", what, err)
		}
		if _, err := s.RuleStatements(ctx, connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{}}); connector.StatementReviewCode(err) != "forbidden" {
			t.Errorf("%s: rule err = %v, want forbidden", what, err)
		}
	}
}

func TestStatementReviewAPI_QueueTakesItsFiltersFromTheQuery(t *testing.T) {
	s, mi := ontologyFixture(t)
	seed := seedReviewQueue(t, s, mi)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	code, out := s.getQueue(t, ctx, "from_under_path=/board&created_after="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if code != http.StatusOK || len(out["statements"].([]any)) != 1 || !strings.Contains(fmt.Sprint(out["statements"]), seed.board) || out["tenant"] != mi.Tenant {
		t.Errorf("filtered queue = %d %v", code, out)
	}
	code, out = s.getQueue(t, ctx, "summarize=true")
	if code != http.StatusOK || out["total"] != float64(2) || len(out["summary"].([]any)) != 2 {
		t.Errorf("summary = %d %v", code, out)
	}
	for _, q := range []string{"min_confidence=high", "created_before=yesterday", "summarize=maybe", "scope=agent"} {
		if code, out := s.getQueue(t, ctx, q); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", q, code, out)
		}
	}
}

func TestStatementReviewAPI_ATenantOperatorReviewsItsOwnTenant(t *testing.T) {
	s, mi := ontologyFixture(t)
	seedReviewQueue(t, s, mi)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	res, err := s.ListReviewQueue(op, connector.ListReviewQueueRequest{Tenant: "elsewhere"})
	if err != nil || res.Tenant != mi.Tenant || len(res.Statements) != 2 {
		t.Errorf("a tenant operator naming another tenant: %+v %v, want its own tenant's queue", res, err)
	}
}

// A ruling body's time bounds take a number or text, as the queue's query does:
// a JavaScript client sends a nanosecond created_at as a string.
func TestStatementReviewAPI_RuleFilterTakesTimeBoundsAsNumberOrText(t *testing.T) {
	s, mi := ontologyFixture(t)
	seedReviewQueue(t, s, mi)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	hourAgo := time.Now().Add(-time.Hour)
	for _, bound := range []any{hourAgo.UnixNano(), fmt.Sprint(hourAgo.UnixNano()), hourAgo.UTC().Format(time.RFC3339)} {
		code, out := s.postReview(t, op, "/v1/_statements/rule", map[string]any{"action": "accept", "filter": map[string]any{"created_after": bound}})
		if code != http.StatusOK || out["matched"] != float64(2) {
			t.Errorf("created_after %v = %d %v, want both proposals matched", bound, code, out)
		}
	}
	if code, out := s.postReview(t, op, "/v1/_statements/rule", map[string]any{"action": "accept", "filter": map[string]any{"created_after": "soon"}}); code != http.StatusBadRequest {
		t.Errorf("an unreadable bound = %d %v, want 400", code, out)
	}
}

// A summary of an empty queue still says so: summary [] and total 0, rather
// than a page with neither, which reads as "not summarized".
func TestStatementReviewAPI_SummaryOfAnEmptyQueueCarriesItsZeroCounts(t *testing.T) {
	s, mi := ontologyFixture(t)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	code, out := s.getQueue(t, op, "summarize=true")
	if code != http.StatusOK || out["total"] != float64(0) || fmt.Sprint(out["summary"]) != "[]" {
		t.Errorf("empty summary = %d %v, want summary [] and total 0", code, out)
	}
	if _, out := s.getQueue(t, op, ""); out["summary"] != nil || out["total"] != nil {
		t.Errorf("a page of rows carries summary fields: %v", out)
	}
}
