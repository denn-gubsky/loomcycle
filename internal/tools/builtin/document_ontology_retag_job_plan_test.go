package builtin

// document_ontology_retag_job_plan_test.go — RFC GQ P5.5/P5.6: the plan of a concept
// rename or move, or a scheme rename, says what its concept re-tag job would do.
//
// From the existing state of the re-tag tests (newRetagFixture): documents and chunks
// in the tenant, user and agent scopes are filed under topics/risk and
// topics/risk/credit-risk before the plan. The minimal pair is a concept edit that moves
// no tag: an edit of Risk's meaning alone.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// retagPlanChange plans changes and returns the planned change at i.
func (f *retagFixture) retagPlanChange(i int, changes ...connector.OntologyChange) connector.OntologyPlannedChange {
	f.t.Helper()
	plan, err := f.d.PlanOntology(f.ctx, changes)
	if err != nil || len(plan.Changes) != len(changes) {
		f.t.Fatalf("plan: %v %+v", err, plan)
	}
	return plan.Changes[i]
}

func retagPlanSteps(job *connector.OntologyRenameJobPlan) map[string][2]int {
	out := map[string][2]int{}
	for _, s := range job.Steps {
		out[s.Name] = [2]int{s.TenantScope, s.OtherScopes}
	}
	return out
}

// TestConceptMove_PlanCarriesTheRetagJobBlock: the plan of moving Risk under Reporting
// carries a concept_retag job block — the topmost tag move alone (Credit risk follows as a
// prefix), as the move's own job then records it, and the rows per step in the tenant
// scope and in the user and agent scopes; a rename of Reporting planned in the same
// request carries its own move. An edit of Risk's meaning alone moves no tag and carries
// no job block.
func TestConceptMove_PlanCarriesTheRetagJobBlock(t *testing.T) {
	assertRetagPlan(t, newRetagFixture(t))
}

func TestConceptMove_PlanCarriesTheRetagJobBlockOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	assertRetagPlan(t, newRetagFixtureOn(t, d, base))
}

func assertRetagPlan(t *testing.T, f *retagFixture) {
	t.Helper()
	move := connector.OntologyChange{Op: connector.OntologyPlanMove, Entry: f.risk, Parent: f.reporting}
	ch := f.retagPlanChange(0, move)
	job := ch.Job
	if job == nil {
		t.Fatalf("the plan of the move carries no job block: %+v", ch)
	}
	if job.Kind != ontologyJobKindConceptRetag || !job.Runnable || job.ScopesNotCounted != 0 || job.Running != nil {
		t.Errorf("job = %+v, want a runnable concept_retag with every scope counted and nothing running", job)
	}
	if want := []connector.OntologyTagMove{{From: "topics/risk", To: "topics/reporting/risk"}}; !reflect.DeepEqual(job.TagMoves, want) {
		t.Errorf("tag_moves = %+v, want %+v", job.TagMoves, want)
	}
	// Per scope: Section one's chunk tag topics/risk and Credit memo's document tag
	// topics/risk/credit-risk; the pairs (topics/safety, topics/risk-management) and the
	// ontology's own mirror tags are not counted.
	want := map[string][2]int{"chunk_tags": {1, 2}, "document_tags": {1, 2}, "classification_tags": {0, 0}}
	if got := retagPlanSteps(job); !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}

	both := f.retagPlanChange(1, connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: f.risk, Body: "Losses and exposures."},
		connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: f.reporting, Name: "Filings"})
	if both.Job == nil || !reflect.DeepEqual(both.Job.TagMoves, []connector.OntologyTagMove{{From: "topics/reporting", To: "topics/filings"}}) {
		t.Errorf("rename of Reporting beside an edit: job %+v, want topics/reporting -> topics/filings", both.Job)
	}
	if got := retagPlanSteps(both.Job); !reflect.DeepEqual(got, map[string][2]int{"chunk_tags": {0, 0}, "document_tags": {0, 0}, "classification_tags": {0, 0}}) {
		t.Errorf("rename of Reporting (nothing filed under it) steps = %v, want none", got)
	}
	if edit := f.retagPlanChange(0, connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: f.risk, Body: "Losses and exposures."}); edit.Job != nil {
		t.Errorf("an edit of the meaning alone carries a job block: %+v", edit.Job)
	}
	// The same edit beside a move of the same entry: each is a write of its own, so the
	// edit still moves no tag.
	if edit := f.retagPlanChange(1, move, connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: f.risk, Body: "Losses and exposures."}); edit.Job != nil {
		t.Errorf("an edit of the meaning alone beside a move of the same entry carries a job block: %+v", edit.Job)
	}

	out := f.moveRisk()
	rj := retagJob(t, out)
	if !reflect.DeepEqual(rj["from"], []any{"topics/risk"}) || !reflect.DeepEqual(rj["to_tags"], []any{"topics/reporting/risk"}) {
		t.Errorf("the move's job moved %v -> %v, the plan said %+v", rj["from"], rj["to_tags"], job.TagMoves)
	}
	if done, _ := rj["done"].(map[string]any); asInt(done["chunk_tags"]) != 3 || asInt(done["document_tags"]) != 3 {
		t.Errorf("the move's job rewrote %v, the plan counted 1+2 chunk tags and 1+2 document tags", rj["done"])
	}
}

// TestConceptMove_PlanNamesTheUnfinishedJobAndTheScopesNotCounted: while a re-tag job is
// unfinished, the plan of another concept move names it (the change would be refused
// rename_job_running while it stays unfinished); a plan whose scope walk ran out of
// budget counts the tenant scope only and says how many scopes it left out.
func TestConceptMove_PlanNamesTheUnfinishedJobAndTheScopesNotCounted(t *testing.T) {
	f := newRetagFixture(t)
	f.d.renameJobStop = func(scope, step string) bool { return scope == "tenant" && step == "chunk_tags" }
	if job := retagJob(t, f.moveRisk()); asStr(job["state"]) != jobStateRunning {
		t.Fatalf("stopped move: job %v, want running", job)
	}
	f.d.renameJobStop = nil
	running, _ := f.d.unfinishedOntologyJob(f.ctx, f.key)

	ch := f.retagPlanChange(0, connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: f.reporting, Name: "Filings"})
	if ch.Job == nil || ch.Job.Running == nil || ch.Job.Running.ID != running.ID || ch.Job.Running.Kind != ontologyJobKindConceptRetag {
		t.Fatalf("job = %+v, want the unfinished re-tag %s named", ch.Job, running.ID)
	}
	if !strings.Contains(strings.Join(ch.Job.Notes, "\n"), renameJobRunningCode) {
		t.Errorf("notes = %v, want the %s refusal named", ch.Job.Notes, renameJobRunningCode)
	}

	f.d.planScopeBudget = 1
	ch = f.retagPlanChange(0, connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: f.reporting, Name: "Filings"})
	if ch.Job == nil || ch.Job.ScopesNotCounted != 2 {
		t.Fatalf("job with no budget = %+v, want 2 scopes not counted", ch.Job)
	}
	for _, s := range ch.Job.Steps {
		if s.OtherScopes != 0 {
			t.Errorf("step %s counted %d rows in scopes the plan did not count", s.Name, s.OtherScopes)
		}
	}
}
