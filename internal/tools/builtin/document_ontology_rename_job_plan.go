package builtin

// document_ontology_rename_job_plan.go — the plan's `job` block for a class rename
// (RFC GQ plan P5.3–P5.4, 4.2 "What the plan shows before"): the rows each step of the
// rename job would move, what keeps it from running (a plan that could not count every
// scope), the stored keys that would collide under the new name in any scope, and the
// tenant's unfinished job. Like the rest of a plan, it writes nothing.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// renameJobPlan is the job block of a rename of a class something rests on. p is the
// planned rename and ch its counted result; read is the ontology before it.
func (d *Document) renameJobPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, p *plannedChange,
	ch connector.OntologyPlannedChange, complete bool, skipped int) (*connector.OntologyRenameJobPlan, error) {
	to := strings.TrimSpace(p.in.Name)
	job := &connector.OntologyRenameJobPlan{ScopesNotCounted: skipped}
	from := renameOwnNames(p)
	count := func(name string) connector.OntologyPlanCount { return ch.Counts[name] }
	tenant := renameScope{key: key, tenant: true, name: renameTenantScope}
	mentions := ontologyMentions(read, from)
	for _, st := range renameSteps {
		s := connector.OntologyRenameJobStep{Name: st.name}
		switch st.name {
		case "ontology_mentions":
			s.TenantScope = len(mentions)
		case "classification_records":
			// In document fields, not SQL: counting means reading every document's
			// fields, which a plan does not do. The job reads them.
		case "documents_type":
			s.TenantScope, s.OtherScopes = count("typed_documents").TenantScope, count("typed_documents").OtherScopes
		case "chunks_type":
			s.TenantScope, s.OtherScopes = count("typed_chunks").TenantScope, count("typed_chunks").OtherScopes
		case "identity_keys", "subject_keys":
			s.TenantScope, s.OtherScopes = count(st.name).TenantScope, count(st.name).OtherScopes
		case "node_stats", "measure_points":
			s.TenantScope = count(st.name).TenantScope
		default:
			n, err := st.left(d, ctx, tenant, from, to)
			if err != nil {
				return nil, fmt.Errorf("rename job %s: %w", st.name, err)
			}
			s.TenantScope = n
		}
		job.Steps = append(job.Steps, s)
	}
	if !complete {
		// Kept from P5.3 (plan 4.2: an incomplete plan refuses the rename): a scope not
		// counted is a scope whose colliding keys are not known before the job meets them.
		job.Blocked = append(job.Blocked, "not every user and agent scope could be counted, so keys under the old name that would collide may lie where the plan did not look")
	}
	var err error
	if job.Collisions, job.CollisionsTotal, err = d.renameCollisions(ctx, key, from, to); err != nil {
		return nil, err
	}
	job.CollisionsOtherScopes = p.otherCollisions
	job.CollisionsTotal += p.otherCollisions
	running, err := d.unfinishedOntologyJob(ctx, key)
	if err != nil {
		return nil, err
	}
	if running != nil {
		job.Running = running.status()
		job.Notes = append(job.Notes, "another rename job is unfinished ("+running.ID+", "+running.State+"); a rename carries it on first and is refused ("+
			renameJobRunningCode+") while it stays unfinished")
	}
	job.Notes = append(job.Notes,
		"the rename writes the new name and @renamed_from "+p.entry.Name+" in one write, so the rows read as the class while the job moves them",
		"the job rewrites the tenant scope, then every user and agent scope; a scope from before the document plane split is rewritten on its agent plane")
	for _, m := range mentions {
		job.Notes = append(job.Notes, "the ontology names the class elsewhere, and the job's first step rewrites it to the new name: "+m.what)
	}
	job.Notes = append(job.Notes,
		"the classification record on documents (fields.classification applied_type and proposed_type) is rewritten by the job's last step in each scope; the plan does not count it",
		"definitions outside the ontology that name the class (agent prompts, source declarations) are not rewritten")
	job.Runnable = len(job.Blocked) == 0 && job.CollisionsTotal == 0
	return job, nil
}

// renameOwnNames is the names a rename of the class moves rows off: its own, and the
// former name it reads rows under. Its subclasses keep their names.
func renameOwnNames(p *plannedChange) []string {
	from := []string{p.entry.Name}
	if p.entry.former != "" {
		from = appendUnique(from, p.entry.former)
	}
	return from
}

// renameCollisions finds the stored keys the rename would move onto a key another row
// already holds — an identity or subject key under the new name, or two old spellings
// that would become one key — in the tenant scope. Up to ontologyPlanSample are listed;
// total counts every one.
func (d *Document) renameCollisions(ctx context.Context, key sqlmem.ScopeKey, from []string, to string) ([]connector.OntologyRenameCollision, int, error) {
	var out []connector.OntologyRenameCollision
	total := 0
	for _, c := range []struct{ table, idCol, col, prefix string }{
		{"chunks", "id", "identity_key", identityPrefix(to)},
		{"chunk_memory_meta", "chunk_id", "natural_key", to},
	} {
		spell, err := d.renameSpellings(ctx, key, c.table, c.col, "", from, true, c.prefix)
		if err != nil {
			return nil, 0, fmt.Errorf("rename collisions: %s: %w", c.col, err)
		}
		// Each old spelling against the keys already under the new prefix, then each
		// pair of old spellings against each other.
		type pair struct{ a, b string }
		var pairs []pair
		for _, s := range spell {
			pairs = append(pairs, pair{s, c.prefix})
		}
		for i := range spell {
			for k := i + 1; k < len(spell); k++ {
				pairs = append(pairs, pair{spell[i], spell[k]})
			}
		}
		for _, pr := range pairs {
			la := runeLen(pr.a)
			// The key a's row would become is looked up by equality, so the column's unique
			// index serves each probe: one pass over the old-prefixed rows, not a pass over
			// the table per row. Positions are characters on both tiers (substr); the rest
			// of a key starts after its prefix and the colon.
			where := ` FROM ` + c.table + ` a JOIN ` + c.table + ` b ON b.` + c.col + ` = CAST(? AS TEXT) || substr(a.` + c.col + `, ?)` +
				` WHERE substr(a.` + c.col + `, 1, ?) = ? AND a.` + c.idCol + ` <> b.` + c.idCol
			args := []any{pr.b + ":", la + 2, la + 1, pr.a + ":"}
			n, err := d.planCount(ctx, key, `SELECT count(*)`+where, args...)
			if err != nil {
				return nil, 0, fmt.Errorf("rename collisions: %s: %w", c.col, err)
			}
			total += n
			if n == 0 || len(out) >= ontologyPlanSample {
				continue
			}
			res, err := d.planQuery(ctx, key, `SELECT a.`+c.col+`, b.`+c.idCol+where+` ORDER BY a.`+c.col+
				` LIMIT `+strconv.Itoa(ontologyPlanSample), args...)
			if err != nil {
				return nil, 0, fmt.Errorf("rename collisions: %s: %w", c.col, err)
			}
			for _, r := range res.Rows {
				if len(out) >= ontologyPlanSample || len(r) < 2 {
					break
				}
				into, _ := rekey(asStr(r[0]), []string{pr.a}, c.prefix)
				out = append(out, connector.OntologyRenameCollision{Column: c.col, Key: asStr(r[0]), Into: into, Holder: asStr(r[1])})
			}
		}
	}
	return out, total, nil
}
