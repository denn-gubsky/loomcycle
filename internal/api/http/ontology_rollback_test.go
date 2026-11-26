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
