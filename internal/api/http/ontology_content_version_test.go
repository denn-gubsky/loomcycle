package http

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyAPI_ContentVersionSurvivesConfirmAndAdoption (RFC GQ P3.1).
//
// GET /v1/_ontology reports the content version, and a confirm does not move it: the
// confirm bumps the root's revision (ontology_version) and adopts the instances, which
// writes each one's registry key onto its chunk, yet nothing anyone declared changed.
// A version that moved here would mark every row written before the first confirm as
// written under an older vocabulary. The plan reports the same value.
func TestOntologyAPI_ContentVersionSurvivesConfirmAndAdoption(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, instancesLayout())

	before := s.getOntology(t, mi.Tenant)
	if len(before.ContentVersion) != 16 {
		t.Fatalf("content_version = %q, want 16 hex characters", before.ContentVersion)
	}
	confirmed := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if confirmed.InstanceAdoption == nil || confirmed.InstanceAdoption.Adopted != 2 {
		t.Fatalf("the confirm should adopt both instances: %#v", confirmed.InstanceAdoption)
	}
	after := s.getOntology(t, mi.Tenant)
	if after.OntologyVersion == before.OntologyVersion {
		t.Fatal("the confirm did not bump ontology_version; the document did not change")
	}
	if confirmed.ContentVersion != before.ContentVersion || after.ContentVersion != before.ContentVersion {
		t.Errorf("content_version moved on a confirm: %s before, %s in the confirm's answer, %s after",
			before.ContentVersion, confirmed.ContentVersion, after.ContentVersion)
	}

	plan, err := s.PlanOntology(tierOperator(mi.Tenant, "op"), connector.PlanOntologyRequest{Changes: []connector.OntologyChange{
		{Op: connector.OntologyPlanRetire, Entry: after.ClassChunks["OrganizationalUnit"]}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ContentVersion != after.ContentVersion {
		t.Errorf("the plan's content_version = %q, want the ontology's %q", plan.ContentVersion, after.ContentVersion)
	}
}

// TestOntologyAPI_CountsFactsAndStatementsByContentVersion (RFC GQ P3.2).
//
// GET /v1/_ontology splits the tenant scope's facts and statements into those written
// under the content version in force, under another one, and with none recorded.
func TestOntologyAPI_CountsFactsAndStatementsByContentVersion(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{{title: "incident"}}},
		{title: "Relations", body: "@section relations\n", kids: []ontologyTestChunk{{title: "cites"}}},
	})
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

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
	docID := exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "Notes", "path": "/kb/notes"})["document_id"]
	a := exec(map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": docID, "title": "a", "natural_key": "k:a", "body": "x"})["id"].(string)
	b := exec(map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": docID, "title": "b", "natural_key": "k:b", "body": "y"})["id"].(string)
	exec(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "cites"})
	// One fact as an older release wrote it: no version recorded.
	key := sqlmem.ScopeKey{Tenant: mi.Tenant, Scope: "tenant", ScopeID: mi.Tenant}
	if _, err := s.sqlMem.DocPlane().Exec(dctx, key, `UPDATE chunk_memory_meta SET ontology_content = NULL WHERE chunk_id = '`+b+`'`, nil, 0); err != nil {
		t.Fatalf("clear b's version: %v", err)
	}

	got := s.getOntology(t, mi.Tenant)
	if got.FactContent == nil || *got.FactContent != (builtin.ContentCount{Current: 1, Unknown: 1}) {
		t.Errorf("fact_content = %+v, want 1 current and 1 unknown", got.FactContent)
	}
	if got.StatementContent == nil || *got.StatementContent != (builtin.ContentCount{Current: 1}) {
		t.Errorf("statement_content = %+v, want the one statement current", got.StatementContent)
	}
}
