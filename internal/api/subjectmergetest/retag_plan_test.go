package subjectmergetest

// The plan's concept re-tag block (RFC GQ P5.5): a concept move planned over HTTP, gRPC
// and MCP carries the same job block, kind and tag moves included.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
)

const retagPlanOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n\n#### Risk\n\n##### Credit risk\n\n#### Reporting\n"

// seedRetagPlan confirms an ontology with the scheme topics, and files a document under
// Credit risk and another under topics/risk-management (a tag sharing Risk's text, not
// under it) by their tags, before anything is planned. It returns Risk and Reporting.
func (e *env) seedRetagPlan() (risk, reporting string) {
	e.t.Helper()
	out := e.opDocCall(fmt.Sprintf(`{"op":"import_md","scope":"tenant","markdown":%q}`, retagPlanOntologyMD))
	id, root := out["document_id"].(string), out["root_chunk_id"].(string)
	e.opDocCall(fmt.Sprintf(`{"op":"set_path","scope":"tenant","id":%q,"path":%q}`, id, meminject.OntologyPath))
	rev := e.opDocCall(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, root))["revision"]
	e.opDocCall(fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%v}`, root, meminject.OntologyConfirmedStatus, rev))
	chunks, _ := e.docCall(fmt.Sprintf(`{"op":"query_chunks","scope":"tenant","document_id":%q,"limit":50}`, id))["chunks"].([]any)
	for _, c := range chunks {
		m := c.(map[string]any)
		switch m["title"] {
		case "Risk":
			risk = m["id"].(string)
		case "Reporting":
			reporting = m["id"].(string)
		}
	}
	if risk == "" || reporting == "" {
		e.t.Fatalf("ontology chunks not found in %v", chunks)
	}
	for title, tag := range map[string]string{"Credit memo": "topics/risk/credit-risk", "Management note": "topics/risk-management"} {
		doc := e.docCall(fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q}`, title))
		e.docCall(fmt.Sprintf(`{"op":"add_tags","scope":"tenant","document_id":%q,"tags":[%q]}`, doc["document_id"], tag))
	}
	return risk, reporting
}

// TestPlanOntology_RetagJobBlockIsTheSameOnEveryTransport: the plan of moving Risk under
// Reporting carries the concept re-tag job block — kind concept_retag, the one topmost
// tag move, the rows per step — and HTTP, gRPC and MCP give the same one, every field.
// An edit of Risk's meaning alone moves no tag and carries none on all three.
func TestPlanOntology_RetagJobBlockIsTheSameOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	risk, reporting := e.seedRetagPlan()
	changes := []connector.OntologyChange{
		{Op: connector.OntologyPlanMove, Entry: risk, Parent: reporting},
		{Op: connector.OntologyPlanEdit, Entry: risk, Body: "Losses and exposures."},
	}
	var first connector.PlanOntologyResult
	for i, s := range planSurfaces {
		p, o := s.plan(e, operator, changes)
		if !o.ok {
			t.Fatalf("%s: plan refused: %s %s", s.name, o.code, o.raw)
		}
		p = normalizedPlan(p)
		for _, c := range p.Changes {
			if c.Job != nil && len(c.Job.Blocked) == 0 {
				c.Job.Blocked = nil
			}
		}
		if i == 0 {
			first = p
			job := p.Changes[0].Job
			if job == nil || job.Kind != "concept_retag" ||
				!reflect.DeepEqual(job.TagMoves, []connector.OntologyTagMove{{From: "topics/risk", To: "topics/reporting/risk"}}) {
				t.Fatalf("http plan's move job = %+v, want concept_retag topics/risk -> topics/reporting/risk", job)
			}
			if job.Steps[1].Name != "document_tags" || job.Steps[1].TenantScope != 1 {
				t.Errorf("http plan's steps = %+v, want Credit memo's document tag counted (and not the pair's)", job.Steps)
			}
			if p.Changes[1].Job != nil {
				t.Errorf("an edit of the meaning alone carries a job block: %+v", p.Changes[1].Job)
			}
			continue
		}
		if !reflect.DeepEqual(p, first) {
			a, _ := json.Marshal(p.Changes)
			b, _ := json.Marshal(first.Changes)
			t.Errorf("%s changes\n%s\nhttp changes\n%s", s.name, a, b)
		}
	}
}
