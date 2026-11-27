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
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC GQ P5.7 — rolling the ontology back to a release over HTTP.

func (s *Server) postRollback(t *testing.T, ctx context.Context, tenant, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/rollback?tenant="+tenant, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleOntologyRollback(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestOntologyRollbackRoute_OperatorAppliesWithThePlansFingerprint — the route is the
// operator's: the gate refuses a member and admits the tenant operator, asking write
// document, and the handler refuses a member itself. A tenant operator's rollback with
// a stale fingerprint is 409 plan_stale carrying the current one and writes nothing; with
// the plan's fingerprint it unretires the class retired since the release.
func TestOntologyRollbackRoute_OperatorAppliesWithThePlansFingerprint(t *testing.T) {
	gate, st := tokenAuthServer(t, "legacy")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	seedToken(t, st, "lct_member", "acme", "bob", memberScopes, time.Time{})
	seedToken(t, st, "lct_operator", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	if reached, code := runMW(t, gate, http.MethodPost, "/v1/_ontology/rollback", "lct_member"); reached || code != http.StatusForbidden {
		t.Errorf("a member on POST /v1/_ontology/rollback = %d reached=%v, want 403", code, reached)
	}
	if reached, code := runMW(t, gate, http.MethodPost, "/v1/_ontology/rollback", "lct_operator"); !reached || code == http.StatusForbidden {
		t.Errorf("the tenant operator on POST /v1/_ontology/rollback = %d reached=%v, want admitted", code, reached)
	}
	if sf, ok := authz.HTTPSurface(http.MethodPost, "/v1/_ontology/rollback"); !ok || sf.Action != "write" || sf.Type != "document" {
		t.Errorf("POST /v1/_ontology/rollback asks %+v (governed %v), want write document", sf, ok)
	}

	s, mi := ontologyFixture(t)
	provision := installPlanOntology(t, s, mi)
	rel := s.takeTestRelease(t, mi, builtin.OntologyReleaseConfirm)
	member := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "m", Scopes: []string{auth.ScopeRunsCreate}})
	if code, out := s.postRollback(t, member, mi.Tenant, fmt.Sprintf(`{"seq":%d,"plan_fingerprint":"x"}`, rel.Seq)); code != http.StatusForbidden {
		t.Errorf("a member's rollback = %d %v, want 403", code, out)
	}

	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})
	plan := func() connector.PlanOntologyResult {
		t.Helper()
		res, err := s.PlanOntology(op, connector.PlanOntologyRequest{Changes: []connector.OntologyChange{{Op: connector.OntologyPlanRelease, Seq: rel.Seq}}})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		return res
	}
	stale := plan().Fingerprint
	doc, dctx := s.ontologyDocument(), s.ontologyDocCtx(context.Background(), mi)
	get, _ := doc.Execute(dctx, []byte(`{"op":"get_chunk","scope":"tenant","id":"`+provision+`"}`))
	var row struct {
		Revision int `json:"revision"`
	}
	_ = json.Unmarshal([]byte(get.Text), &row)
	retire, _ := json.Marshal(map[string]any{"op": "update_chunk", "scope": "tenant", "id": provision, "revision": row.Revision,
		"status": meminject.OntologyStatusRetired, "plan_fingerprint": stale})
	if res, err := doc.Execute(dctx, retire); err != nil || res.IsError {
		t.Fatalf("retire Provision: %v %s", err, res.Text)
	}
	now := plan().Fingerprint

	code, out := s.postRollback(t, op, mi.Tenant, fmt.Sprintf(`{"seq":%d,"plan_fingerprint":%q}`, rel.Seq, stale))
	if code != http.StatusConflict || out["code"] != "plan_stale" || out["fingerprint"] != now {
		t.Fatalf("rollback with a stale fingerprint = %d %v, want 409 plan_stale carrying %s", code, out, now)
	}
	code, out = s.postRollback(t, op, mi.Tenant, fmt.Sprintf(`{"seq":%d,"plan_fingerprint":%q}`, rel.Seq, now))
	applied := fmt.Sprint(dig(out, "rollback", "applied"))
	if code != http.StatusOK || out["tenant"] != mi.Tenant || dig(out, "rollback", "matches") != true || !strings.Contains(applied, provision) {
		t.Errorf("rollback with the plan's fingerprint = %d %v, want Provision unretired and the release's content version", code, out)
	}
	if code, out := s.postRollback(t, op, mi.Tenant, `{"seq":1,"dry_run":true}`); code != http.StatusBadRequest || out["code"] != "invalid_body" {
		t.Errorf("an unknown field = %d %v, want 400 invalid_body", code, out)
	}
}

// TestOntologyRollback_RefusalsKeepWhatTheirAnswerCarries — the connector names each
// way a rollback stops short, keeping what every transport answers beside the code (the
// writes made, the current fingerprint), and HTTP answers each as it did before the
// twins: 409 with applied or fingerprint, 409 rename_job_pending, 422 invalid_request,
// the plan's own refusals as the plan route, and 500 without the store's text.
func TestOntologyRollback_RefusalsKeepWhatTheirAnswerCarries(t *testing.T) {
	s, _ := ontologyFixture(t)
	applied := []builtin.RollbackStep{{Op: "retire", Entry: "c1", Name: "Memo"}}
	rep := builtin.RollbackReport{Release: 2, Applied: applied}
	for _, tc := range []struct {
		err    error
		status int
		body   map[string]any
	}{
		{fmt.Errorf("%w: applied 1 write, then: refused", builtin.ErrRollbackPartiallyApplied), http.StatusConflict,
			map[string]any{"code": "partially_applied", "error": "partially_applied: applied 1 write, then: refused",
				"applied": []any{map[string]any{"op": "retire", "entry": "c1", "name": "Memo"}}}},
		{&builtin.ChangePlanRefusal{Code: "plan_stale", Message: "rollback: plan_stale: changed", Fingerprint: "fp-now"}, http.StatusConflict,
			map[string]any{"code": "plan_stale", "error": "rollback: plan_stale: changed", "fingerprint": "fp-now"}},
		{fmt.Errorf("%w: cannot carry it", builtin.ErrChangeRenameJobPending), http.StatusConflict,
			map[string]any{"code": "rename_job_pending", "error": "rename_job_pending: cannot carry it"}},
		{fmt.Errorf("%w: refused", builtin.ErrChangeProposalInvalid), http.StatusUnprocessableEntity,
			map[string]any{"code": "invalid_request", "error": "invalid_proposal: refused"}},
		{connector.WithMessage(connector.ErrOntologyPlanNotFound, "no release 9"), http.StatusNotFound,
			map[string]any{"code": "entry_not_found", "error": "no release 9"}},
		{fmt.Errorf("open /var/lib/secret.db: denied"), http.StatusInternalServerError,
			map[string]any{"code": "ontology_write_failed", "error": "the rollback could not be applied; see the server log"}},
	} {
		rec := httptest.NewRecorder()
		writeOntologyRollbackError(rec, s.rollbackRefusal(context.Background(), "acme", rep, tc.err))
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != tc.status || fmt.Sprint(got) != fmt.Sprint(tc.body) {
			t.Errorf("%v: %d %v, want %d %v", tc.err, rec.Code, got, tc.status, tc.body)
		}
	}
}
