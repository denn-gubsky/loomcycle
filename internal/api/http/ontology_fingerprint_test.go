package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyEdit_PlanRefusalIs409WithTheFingerprint (RFC GQ P2.4): through POST
// /v1/_document, a tenant operator's retire of a class something rests on, without a
// plan's fingerprint, answers 409 plan_required carrying the fingerprint POST
// /v1/_ontology/plan reports; a wrong one answers 409 plan_stale; the plan's own
// fingerprint applies it. Renaming that class while a user scope holds a row of it,
// which the rename job does not reach yet (RFC GQ P5.3), is refused rename_job_pending
// (the RFC GQ ruling of 2026-11-20), with the plan's fingerprint too.
func TestOntologyEdit_PlanRefusalIs409WithTheFingerprint(t *testing.T) {
	s, mi := tierFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes", kids: []ontologyTestChunk{{title: "Memo"}}},
	})
	s.confirmOntology(t, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	exec := func(payload map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(payload)
		res, err := doc.Execute(dctx, b)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", payload["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "A memo", "type": "Memo"})
	exec(map[string]any{"op": "create_document", "scope": "user", "title": "My memo", "type": "Memo"})
	memo := s.getOntology(t, mi.Tenant).ClassChunks["Memo"]
	op := tierOperator(mi.Tenant, "op")

	plan, err := s.PlanOntology(op, connector.PlanOntologyRequest{Changes: []connector.OntologyChange{
		{Op: connector.OntologyPlanRetire, Entry: memo}}})
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Risk != connector.OntologyRiskStales {
		t.Fatalf("plan: %v %+v, want the retire to stale the typed document", err, plan)
	}
	update := func(field, fp string) (int, map[string]any) {
		rev := int(exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": memo})["revision"].(float64))
		body := fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,%s,"plan_fingerprint":%q}`, memo, rev, field, fp)
		return s.postDocument(t, op, "", body)
	}
	rename, retire := `"title":"Memorandum"`, fmt.Sprintf(`"status":%q`, memrank.OntologyStatusRetired)
	if code, out := update(rename, plan.Fingerprint); code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "rename_job_pending") {
		t.Errorf("rename with the plan's fingerprint = %d %v, want 422 rename_job_pending", code, out)
	}
	for _, c := range []struct{ fp, code string }{{"", "plan_required"}, {"stale", "plan_stale"}} {
		code, out := update(retire, c.fp)
		if code != http.StatusConflict || out["code"] != c.code || out["fingerprint"] != plan.Fingerprint || out["tool"] != "Document" {
			t.Errorf("retire with fingerprint %q = %d %v, want 409 %s carrying %s", c.fp, code, out, c.code, plan.Fingerprint)
		}
	}
	if code, out := update(retire, plan.Fingerprint); code != http.StatusOK {
		t.Fatalf("retire with the plan's fingerprint = %d %v", code, out)
	}
	if c := s.getOntology(t, mi.Tenant).ClassChunks["Memo"]; c != memo {
		t.Errorf("Memo's chunk after the retire = %q, want %q (renamed?)", c, memo)
	}
}
