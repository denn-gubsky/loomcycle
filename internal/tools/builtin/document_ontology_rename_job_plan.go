package builtin

// document_ontology_rename_job_plan.go — the plan's `job` block for a class rename
// (RFC GQ plan P5.3, 4.2 "What the plan shows before"): the rows each step of the
// rename job would move, why the job cannot carry the rename in this phase, the stored
// keys that would collide under the new name, and the tenant's unfinished job. Like the
// rest of a plan, it writes nothing.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// renameJobPlan is the job block of a rename of a class something rests on. p is the
// planned rename and ch its counted result; read is the ontology before it.
func (d *Document) renameJobPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, p *plannedChange,
	ch connector.OntologyPlannedChange, complete bool, skipped int) (*connector.OntologyRenameJobPlan, error) {
	to := strings.TrimSpace(p.in.Name)
	job := &connector.OntologyRenameJobPlan{ScopesNotCounted: skipped}
	from := p.classes
	count := func(name string) connector.OntologyPlanCount { return ch.Counts[name] }
	other := 0
	for _, st := range renameSteps {
		s := connector.OntologyRenameJobStep{Name: st.name}
		switch st.name {
		case "documents_type":
			s.TenantScope, s.OtherScopes = count("typed_documents").TenantScope, count("typed_documents").OtherScopes
		case "chunks_type":
			s.TenantScope, s.OtherScopes = count("typed_chunks").TenantScope, count("typed_chunks").OtherScopes
		case "identity_keys", "subject_keys":
			s.TenantScope, s.OtherScopes = count(st.name).TenantScope, count(st.name).OtherScopes
		case "node_stats", "measure_points":
			s.TenantScope = count(st.name).TenantScope
		default:
			n, err := st.left(d, ctx, key, from, to)
			if err != nil {
				return nil, fmt.Errorf("rename job %s: %w", st.name, err)
			}
			s.TenantScope = n
		}
		other += s.OtherScopes
		job.Steps = append(job.Steps, s)
	}
	if !complete {
		job.Blocked = append(job.Blocked, "not every user and agent scope could be counted, so rows under the old name may lie where the job does not reach")
	}
	if other > 0 {
		job.Blocked = append(job.Blocked, strconv.Itoa(other)+" rows in user and agent scopes carry the old name, and the job rewrites the tenant scope only so far")
	}
	job.Blocked = append(job.Blocked, ontologyNamesClass(read, from)...)
	var err error
	if job.Collisions, job.CollisionsTotal, err = d.renameCollisions(ctx, key, from, to); err != nil {
		return nil, err
	}
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
		"definitions outside the ontology that name the class (agent prompts, source declarations) are not rewritten",
		"the classification record on documents (fields.classification) keeps the old name and reads through @renamed_from")
	job.Runnable = len(job.Blocked) == 0 && job.CollisionsTotal == 0
	return job, nil
}

// ontologyNamesClass lists where the ontology itself names one of the classes besides
// the class's own entry: mentions the rename would leave pointing at an old name, since
// rewriting them in the same change (RFC GQ 9b C12) is not built in this phase.
func ontologyNamesClass(read OntologyRead, classes []string) []string {
	var out []string
	for _, in := range read.Instances {
		if containsFold(classes, in.Class) {
			out = append(out, "the instance "+strconv.Quote(in.Name)+" is of the class (@a)")
		}
	}
	for _, s := range read.Schemes {
		for _, c := range s.Projects {
			if containsFold(classes, c) {
				out = append(out, "the scheme "+strconv.Quote(s.Name)+" projects the class (@projects)")
				break
			}
		}
	}
	for _, r := range read.Relations {
		for _, c := range append(append([]string{}, r.Domain...), r.Range...) {
			if containsFold(classes, c) {
				out = append(out, "the relation "+strconv.Quote(r.Name)+" names the class in @domain or @range")
				break
			}
		}
	}
	for _, t := range read.Terms {
		for _, fs := range memrank.AllFieldSpecs(t) {
			if fs.Range != "" && containsFold(classes, fs.Range) {
				out = append(out, "the field "+strconv.Quote(fs.Name)+" of the class "+strconv.Quote(t.Name)+" ranges over the class")
				break
			}
		}
	}
	for i := range out {
		out[i] = "the ontology names the class elsewhere, which the rename does not rewrite yet: " + out[i]
	}
	return out
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
