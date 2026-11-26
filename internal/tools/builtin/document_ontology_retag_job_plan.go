package builtin

// document_ontology_retag_job_plan.go — the plan's `job` block for a change that starts a
// concept re-tag job (RFC GQ plan P5.5, 4.2 "What the plan shows before"). A class
// rename's plan said what its job would do (document_ontology_rename_job_plan.go); a
// concept rename or move, or a scheme rename, started a job the plan never mentioned.
//
// The tag moves are the trigger's own: conceptTagMoves over the tags before the change
// and after it alone, as conceptRetagGate reads them before a write. The rows per step
// are the job's own count of what is left (leftRetagRows), in the tenant scope here and
// in every user and agent scope in the plan's scope walk (countPlanDependents). Like the
// rest of a plan, it writes nothing.

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// planRetagMoves is the tags the write of p would move, read as the trigger reads them:
// the ontology before, and with p's own change alone (another change of the request is a
// write of its own, which the trigger compares separately). Only a rename, a move and an
// edit write a chunk the trigger reads; nil for any other change, and for one that moves
// no tag.
func (d *Document) planRetagMoves(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, before, after OntologyRead,
	ov *ontologyOverlay, p *plannedChange, alone bool) ([]tagMove, error) {
	switch p.in.Op {
	case connector.OntologyPlanRename, connector.OntologyPlanMove, connector.OntologyPlanEdit:
	default:
		return nil, nil
	}
	read := after
	if !alone {
		// Only this op's own write: a move and an edit of one entry in the same request
		// share its chunk id in ov, and each is a write of its own.
		id := p.entry.ChunkID
		only := newOntologyOverlay()
		switch p.in.Op {
		case connector.OntologyPlanRename:
			only.titles[id] = ov.titles[id]
		case connector.OntologyPlanMove:
			only.parents[id] = ov.parents[id]
		default:
			only.bodies[id] = ov.bodies[id]
		}
		var err error
		if read, err = d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, only); err != nil {
			return nil, err
		}
	}
	return conceptTagMoves(ontologyConceptTags(before), ontologyConceptTags(read)), nil
}

// retagMoveFroms is the old tags of moves, in order.
func retagMoveFroms(moves []tagMove) []string {
	from := make([]string, len(moves))
	for i, m := range moves {
		from[i] = m.from
	}
	return from
}

// countRetagRows counts, in one scope, the rows each counted step of the re-tag job
// would rewrite for moves: what the job itself reads as left (leftRetagRows).
func (d *Document) countRetagRows(ctx context.Context, sc renameScope, moves []tagMove) (map[string]int, error) {
	out := map[string]int{}
	from := retagMoveFroms(moves)
	for _, st := range retagSteps {
		if st.left == nil {
			continue // classification_tags: in document fields, which a plan does not read
		}
		n, err := st.left(d, ctx, sc, from, "")
		if err != nil {
			return nil, fmt.Errorf("retag job %s: %w", st.name, err)
		}
		out[st.name] = n
	}
	return out, nil
}

// retagJobPlan is the job block of a change whose write moves concept tags: the kind, the
// tag moves, the rows per step in the tenant scope (other scopes counted by the plan's
// walk into p.retagOther), and the tenant's unfinished job, which the change would carry
// on first and be refused for (rename_job_running) while it stays unfinished.
func (d *Document) retagJobPlan(ctx context.Context, key sqlmem.ScopeKey, p *plannedChange, skipped int) (*connector.OntologyRenameJobPlan, error) {
	job := &connector.OntologyRenameJobPlan{Kind: ontologyJobKindConceptRetag, ScopesNotCounted: skipped, Runnable: true}
	for _, m := range p.retagMoves {
		job.TagMoves = append(job.TagMoves, connector.OntologyTagMove{From: m.from, To: m.to})
	}
	tenant, err := d.countRetagRows(ctx, renameScope{key: key, tenant: true, name: renameTenantScope}, p.retagMoves)
	if err != nil {
		return nil, err
	}
	for _, st := range retagSteps {
		job.Steps = append(job.Steps, connector.OntologyRenameJobStep{Name: st.name, TenantScope: tenant[st.name], OtherScopes: p.retagOther[st.name]})
	}
	running, err := d.unfinishedOntologyJob(ctx, key)
	if err != nil {
		return nil, err
	}
	if running != nil {
		job.Running = running.status()
		job.Notes = append(job.Notes, "another ontology job is unfinished ("+running.ID+", "+running.Kind+", "+running.State+
			"); the change gives it a slice first and is refused ("+renameJobRunningCode+") while it stays unfinished")
	}
	job.Notes = append(job.Notes,
		"the change starts a concept re-tag job: every tag equal to an old tag, or under it, becomes the new tag with the same rest, so the documents filed under a narrower concept follow it",
		"the job rewrites the tenant scope, then every user and agent scope; a scope from before the document plane split is rewritten on its agent plane",
		"the classification record on documents (fields.classification proposed_tags) is rewritten by the job's last step in each scope; the plan does not count it",
		"the tenant ontology's own concept tags are left to the next confirm, which rewrites them from the concepts")
	return job, nil
}
