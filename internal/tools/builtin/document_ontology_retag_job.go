package builtin

// document_ontology_retag_job.go — the concept re-tag job (RFC GQ 4.6; plan
// /loomcycle/docs/rfc-gq-p3-p8-plan, 4.1 "Concept tags" and row P5.5).
//
// WHY. A concept's tag is its scheme and path, slash-joined (memrank.MirrorTag). A
// document is filed under a concept outside the tenant scope by carrying that tag
// (accept_classification, add_tags), and search concept:<tag> finds it by the tag. So a
// concept renamed or moved, or a scheme renamed, leaves every document filed under it
// (or under a concept below it) carrying a tag that no longer names anything.
//
// WHAT. The same job machinery as the class rename (ontology_jobs, the lease
// `ontology-rename`, 20 s slices, the walk over the tenant scope and every user and
// agent scope, idempotent steps, the finish check), as a second job kind,
// concept_retag. Its steps:
//
//	chunk_tags, document_tags: a tag equal to an old tag, or under it, becomes the
//	new tag with the same rest. The primary key is (id, tag), so the new row is
//	inserted only when absent and the old one is then deleted: a document already
//	carrying the new tag keeps one;
//	classification_tags: a root's fields.classification.proposed_tags, likewise.
//
// NOT THE ONTOLOGY'S OWN TAGS. The ontology document's concept chunks carry their mirror
// tags too; MirrorSchemeTags (run at confirm) owns those and rewrites them from the
// concepts themselves, so the job leaves the ontology document alone.
//
// WHEN. An update_chunk or move_chunk of a chunk of the tenant ontology that changes any
// concept's or scheme's tag (conceptRetagGate, before the write; finishConceptRetag,
// after it). The change is found by reading the ontology before and after: one write can
// move several tags (a chunk between a concept and its narrower concepts), and the
// narrower concepts of a moved one follow its tag as a prefix, so only the topmost moves
// are kept. One job at a time per tenant, as for a class rename: a second change is
// refused (rename_job_running) while one is unfinished, after it is given a slice.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const ontologyJobKindConceptRetag = "concept_retag"

var retagSteps = []renameStep{
	{"chunk_tags", "chunk_tags", "tag", false, retagTagRows("chunk_tags", "chunk_id"), leftRetagRows("chunk_tags", "chunk_id")},
	{"document_tags", "document_tags", "tag", false, retagTagRows("document_tags", "document_id"), leftRetagRows("document_tags", "document_id")},
	{"classification_tags", "", "", false, retagClassificationRecords, nil},
}

// steps is the job's kind's steps.
func (j *ontologyJob) steps() []renameStep {
	if j.Kind == ontologyJobKindConceptRetag {
		return retagSteps
	}
	return renameSteps
}

// tagMove is one tag a change moves: every tag equal to from, or under it, moves to to.
type tagMove struct{ from, to string }

// ontologyConceptTags is every scheme's and concept's tag in the read, by chunk id.
func ontologyConceptTags(read OntologyRead) map[string]string {
	out := map[string]string{}
	for _, s := range read.Schemes {
		if s.ChunkID != "" {
			out[s.ChunkID] = memrank.MirrorTag(s.Name, nil)
		}
		for _, c := range s.Concepts {
			if c.ChunkID != "" {
				out[c.ChunkID] = c.Tag
			}
		}
	}
	return out
}

// conceptTagMoves is the tags a change moved, from the tags before and after it: per
// entry whose tag changed, its old tag to its new one, the longest old tag first (so a
// narrower concept moved apart from its broader one is carried before the broader one's
// move could take it), less the moves a broader move already makes.
//
// Left out, so nothing is rewritten for them: an old tag another entry still holds (two
// concepts of one name; the documents' tag still names the one that stayed), and a move
// to a tag under its own old one, which no change of one chunk can make and which the
// steps could not finish (a moved row would match again).
func conceptTagMoves(before, after map[string]string) []tagMove {
	held := map[string]bool{}
	for _, tag := range after {
		held[tag] = true
	}
	var moves []tagMove
	for id, old := range before {
		now, ok := after[id]
		if !ok || old == "" || now == "" || now == old || held[old] || strings.HasPrefix(now, old+"/") {
			continue
		}
		moves = append(moves, tagMove{old, now})
	}
	sort.Slice(moves, func(i, k int) bool {
		if len(moves[i].from) != len(moves[k].from) {
			return len(moves[i].from) > len(moves[k].from)
		}
		return moves[i].from < moves[k].from
	})
	var out []tagMove
	for _, m := range moves {
		implied := false
		for _, b := range moves {
			if b != m && strings.HasPrefix(m.from, b.from+"/") && m.to == b.to+m.from[len(b.from):] {
				implied = true
				break
			}
		}
		if !implied {
			out = append(out, m)
		}
	}
	return out
}

// retagged is tag carried by the move from → to; ok is false for a tag not under from.
func retagged(tag, from, to string) (string, bool) {
	if tag == from {
		return to, true
	}
	if strings.HasPrefix(tag, from+"/") {
		return to + tag[len(from):], true
	}
	return "", false
}

// retagSkipDoc is the document whose tags the job leaves alone in a scope: the tenant
// ontology, in the tenant scope; "" (no document) elsewhere.
func (d *Document) retagSkipDoc(ctx context.Context, sc renameScope) (string, error) {
	if !sc.tenant {
		return "", nil
	}
	return d.resolveOntologyID(ctx, sc.key)
}

// retagMatch is the condition matching table's rows tagged from or under it, on
// documents other than skip. table and idCol are literals of retagSteps.
func retagMatch(table, idCol, from, skip string) (string, []any) {
	cond := `(tag = ? OR substr(tag, 1, ?) = ?)`
	args := []any{from, runeLen(from + "/"), from + "/"}
	if table == "chunk_tags" {
		return cond + ` AND ` + idCol + ` NOT IN (SELECT id FROM chunks WHERE document_id = ?)`, append(args, skip)
	}
	return cond + ` AND ` + idCol + ` <> ?`, append(args, skip)
}

// retagTagRows moves table's tags, one row at a time: the new row is inserted when the
// target does not carry it yet, then the old row is deleted. A row moved drops out of the
// match, so running the step again redoes only what is left; a crash between the two
// statements leaves both rows, and the re-run deletes the old one.
func retagTagRows(table, idCol string) renameRunFn {
	return func(d *Document, ctx context.Context, sc renameScope, j *ontologyJob, deadline time.Time) (int, bool, error) {
		skip, err := d.retagSkipDoc(ctx, sc)
		if err != nil {
			return 0, false, err
		}
		moved := 0
		for i, from := range j.From {
			cond, args := retagMatch(table, idCol, from, skip)
			for {
				if pastDeadline(ctx, deadline) {
					return moved, false, nil
				}
				res, err := d.query(ctx, sc.key, `SELECT `+idCol+`, tag FROM `+table+` WHERE `+cond+` ORDER BY `+idCol+`, tag LIMIT `+
					strconv.Itoa(renameBatch), args...)
				if schemaMissing(err) {
					return moved, true, nil
				}
				if err != nil {
					return moved, false, err
				}
				if len(res.Rows) == 0 {
					break
				}
				for _, r := range res.Rows {
					id, tag := asStr(r[0]), asStr(r[1])
					to, ok := retagged(tag, from, j.ToTags[i])
					if !ok {
						return moved, false, fmt.Errorf("%s: a row matched %q but is not under it", table, from)
					}
					if err := d.exec(ctx, sc.key, `INSERT INTO `+table+` (`+idCol+`, tag) SELECT ?, ? WHERE NOT EXISTS
						(SELECT 1 FROM `+table+` WHERE `+idCol+` = ? AND tag = ?)`, id, to, id, to); err != nil {
						return moved, false, err
					}
					if err := d.exec(ctx, sc.key, `DELETE FROM `+table+` WHERE `+idCol+` = ? AND tag = ?`, id, tag); err != nil {
						return moved, false, err
					}
					moved++
				}
			}
		}
		return moved, true, nil
	}
}

// leftRetagRows counts table's rows still tagged an old tag or under it.
func leftRetagRows(table, idCol string) renameLeftFn {
	return func(d *Document, ctx context.Context, sc renameScope, from []string, _ string) (int, error) {
		skip, err := d.retagSkipDoc(ctx, sc)
		if err != nil {
			return 0, err
		}
		total := 0
		for _, f := range from {
			cond, args := retagMatch(table, idCol, f, skip)
			n, err := d.planCount(ctx, sc.key, `SELECT count(*) FROM `+table+` WHERE `+cond, args...)
			if schemaMissing(err) {
				return total, nil
			}
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	}
}

// retagClassificationRecords carries the concepts a classification record proposes
// (proposed_tags) to their new tags.
func retagClassificationRecords(d *Document, ctx context.Context, sc renameScope, j *ontologyJob, deadline time.Time) (int, bool, error) {
	moves := make([]tagMove, len(j.From))
	for i := range j.From {
		moves[i] = tagMove{j.From[i], j.ToTags[i]}
	}
	return walkClassificationRecords(d, ctx, sc, j, deadline, func(fields json.RawMessage) (json.RawMessage, bool) {
		return retaggedClassification(fields, moves)
	})
}

// retaggedClassification is fields with the record's proposed_tags carried by moves, a
// tag the record already proposes kept once; changed is false when none moved.
func retaggedClassification(fields json.RawMessage, moves []tagMove) (json.RawMessage, bool) {
	var f map[string]json.RawMessage
	if len(fields) == 0 || json.Unmarshal(fields, &f) != nil {
		return fields, false
	}
	var rec map[string]json.RawMessage
	if raw, ok := f[classifyField]; !ok || json.Unmarshal(raw, &rec) != nil {
		return fields, false
	}
	var tags []string
	if raw, ok := rec["proposed_tags"]; !ok || json.Unmarshal(raw, &tags) != nil {
		return fields, false
	}
	changed := false
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		for _, m := range moves {
			if to, ok := retagged(t, m.from, m.to); ok {
				t, changed = to, true
				break
			}
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if !changed {
		return fields, false
	}
	var err error
	if rec["proposed_tags"], err = json.Marshal(out); err != nil {
		return fields, false
	}
	if f[classifyField], err = json.Marshal(rec); err != nil {
		return fields, false
	}
	b, err := json.Marshal(f)
	if err != nil {
		return fields, false
	}
	return b, true
}

// conceptRetag is an ontology write the retag gate admitted: the lease it holds, the
// tags as they stood before the write, and the job row recorded for it.
type conceptRetag struct {
	owner  string
	jobID  string
	entry  string
	before map[string]string
}

// release lets go of the lease. Best effort: it expires on its own.
func (r *conceptRetag) release(ctx context.Context, d *Document, key sqlmem.ScopeKey) {
	if r != nil && r.owner != "" {
		_, _ = d.releaseDocumentLease(ctx, key, ontologyRenameLeaseName, r.owner)
	}
}

// conceptRetagGate decides a write to chunk row of the tenant ontology that ov stands
// for: nil, nil when it moves no concept's tag; a refusal while another job is
// unfinished; otherwise the retag, holding the lease, with a pending job row recorded
// before the write (so an interruption leaves it to resume, as a class rename's does).
func (d *Document) conceptRetagGate(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op string,
	row chunkRow, ov *ontologyOverlay) (*conceptRetag, *tools.Result) {
	if key.Scope != "tenant" {
		return nil, nil
	}
	fail := func(err error) (*conceptRetag, *tools.Result) {
		r := errFrom(op+": could not tell whether the change moves a concept's tag, so nothing was changed: "+err.Error(), err)
		return nil, &r
	}
	rctx := noWrites(ctx)
	ontID, err := d.resolveOntologyID(rctx, key)
	if err != nil {
		return fail(err)
	}
	if ontID == "" || ontID != row.DocumentID {
		return nil, nil
	}
	tagsAt := func(ov *ontologyOverlay) (map[string]string, *tools.Result) {
		read, err := d.ontologyForKey(rctx, key, mscope, memrank.OntologyPath, ov)
		if err != nil {
			_, r := fail(err)
			return nil, r
		}
		if read.partial {
			return nil, lockReadPartial(op)
		}
		return ontologyConceptTags(read), nil
	}
	before, r := tagsAt(nil)
	if r != nil {
		return nil, r
	}
	after, r := tagsAt(ov)
	if r != nil {
		return nil, r
	}
	if len(conceptTagMoves(before, after)) == 0 {
		return nil, nil
	}
	refuse := func(why, next string) (*conceptRetag, *tools.Result) {
		r := errBusiness(op+": "+renameJobRunningCode+": "+why+". Nothing was changed.", next)
		return nil, &r
	}
	owner := "runtime:" + ontologyRenameLeaseName + ":" + newDocID()
	lease, err := d.acquireDocumentLease(ctx, key, ontologyRenameLeaseName, owner, time.Now(), renameLeaseTTL)
	if err != nil {
		return fail(err)
	}
	if !lease.Acquired {
		return refuse("another ontology job is being checked or carried out right now", "Try again in a minute.")
	}
	rt := &conceptRetag{owner: owner, entry: row.ID}
	stop := func(res *tools.Result) (*conceptRetag, *tools.Result) {
		rt.release(ctx, d, key)
		return nil, res
	}
	if job, err := d.unfinishedOntologyJob(ctx, key); err != nil {
		_, r := fail(err)
		return stop(r)
	} else if job != nil {
		if job = d.continueOntologyJob(ctx, key, mscope, job); job.unfinished() {
			_, r := refuse("the "+job.Kind+" job "+job.ID+" is still being carried through stored rows ("+job.State+" at "+job.Step+")",
				"Change it again once that job has finished; each such change also carries it on.")
			return stop(r)
		}
	}
	// Read again under the lease: the tags the job compares the written ontology with.
	if rt.before, r = tagsAt(nil); r != nil {
		return stop(r)
	}
	if after, r = tagsAt(ov); r != nil {
		return stop(r)
	}
	moves := conceptTagMoves(rt.before, after)
	if len(moves) == 0 {
		return stop(nil)
	}
	rt.jobID = newDocID()
	if err := d.recordRetagJob(ctx, key, &ontologyJob{ID: rt.jobID, Entry: rt.entry, State: jobStatePending}, moves, true); err != nil {
		_, r := fail(err)
		return stop(r)
	}
	return rt, nil
}

// recordRetagJob writes the job row for moves: inserted (create) or its tags replaced.
func (d *Document) recordRetagJob(ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, moves []tagMove, create bool) error {
	j.Kind, j.From, j.ToTags = ontologyJobKindConceptRetag, nil, nil
	for _, m := range moves {
		j.From, j.ToTags = append(j.From, m.from), append(j.ToTags, m.to)
	}
	j.To = strings.Join(j.ToTags, ", ")
	from, _ := json.Marshal(j.From)
	to, _ := json.Marshal(j.ToTags)
	now := time.Now().UnixNano()
	if !create {
		return d.exec(ctx, key, `UPDATE ontology_jobs SET from_names = ?, to_name = ?, updated_at = ? WHERE id = ?`, string(from), string(to), now, j.ID)
	}
	return d.exec(ctx, key, `INSERT INTO ontology_jobs (id, kind, entry, from_names, to_name, state, step, scope_cursor, counts, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		j.ID, j.Kind, j.Entry, string(from), string(to), j.State, retagSteps[0].name, (&ontologyJob{}).cursor(), "{}", now, now)
}

// finishConceptRetag runs the first slice of the retag after its write, and adds its state
// to the answer as retag_job. The moves are found again from the ontology as written, so a
// write that landed differently from the gate's reading is carried as it landed; one that
// moved no tag after all abandons the job. A failed write leaves the job pending, for the
// next change to resume or abandon (continueOntologyJob).
func (d *Document) finishConceptRetag(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rt *conceptRetag,
	res tools.Result, err error) (tools.Result, error) {
	if rt == nil || err != nil || res.IsError {
		return res, err
	}
	j := &ontologyJob{ID: rt.jobID, Entry: rt.entry, State: jobStateRunning, Step: retagSteps[0].name, Counts: map[string]int{}}
	read, rerr := d.ontologyForKey(noWrites(ctx), key, mscope, memrank.OntologyPath, nil)
	if rerr != nil || read.partial {
		// The job stays pending; the next change resumes it once the ontology reads.
		return res, nil
	}
	moves := conceptTagMoves(rt.before, ontologyConceptTags(read))
	if len(moves) == 0 {
		j.State = jobStateAbandoned
		_ = d.saveOntologyJob(ctx, key, j)
		return res, nil
	}
	if err := d.recordRetagJob(ctx, key, j, moves, false); err != nil {
		st := j.status()
		st.State, st.Error = jobStatePending, "could not record the job's start"
		return withOntologyJob(res, "retag_job", st), nil
	}
	if err := d.saveOntologyJob(ctx, key, j); err != nil {
		st := j.status()
		st.State, st.Error = jobStatePending, "could not record the job's start"
		return withOntologyJob(res, "retag_job", st), nil
	}
	return withOntologyJob(res, "retag_job", d.runOntologyJob(ctx, key, mscope, j).status()), nil
}

// retagOverlay is what an update_chunk of row reads as in the ontology: its new title and
// body (stored fields kept unless the update sends them). nil when the update changes
// neither, so it cannot move a tag.
func (d *Document) retagOverlay(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, row chunkRow, in docInput,
	present map[string]json.RawMessage) (*ontologyOverlay, error) {
	_, hasTitle := present["title"]
	_, hasBody := present["body"]
	if key.Scope != "tenant" || (!hasTitle && !hasBody) {
		return nil, nil
	}
	ov := newOntologyOverlay()
	if hasTitle {
		ov.titles[row.ID] = in.Title
	}
	if hasBody {
		cur, err := d.readBody(noWrites(ctx), mscope, key, row.ID)
		if err != nil {
			return nil, err
		}
		cur.Body = in.Body
		if _, has := present["fields"]; has {
			cur.Fields = in.Fields
		}
		ov.bodies[row.ID] = cur
	}
	return ov, nil
}

// ontologyRetagForUpdate is update_chunk's retag gate (conceptRetagGate over retagOverlay).
func (d *Document) ontologyRetagForUpdate(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, row chunkRow, in docInput,
	present map[string]json.RawMessage) (*conceptRetag, *tools.Result) {
	ov, err := d.retagOverlay(ctx, key, mscope, row, in, present)
	if err != nil {
		r := errFrom("update_chunk: could not tell whether the change moves a concept's tag, so nothing was changed: "+err.Error(), err)
		return nil, &r
	}
	if ov == nil {
		return nil, nil
	}
	return d.conceptRetagGate(ctx, key, mscope, "update_chunk", row, ov)
}

// ontologyRetagForMove is move_chunk's retag gate.
func (d *Document) ontologyRetagForMove(ctx context.Context, key sqlmem.ScopeKey, row chunkRow, newParent string) (*conceptRetag, *tools.Result) {
	if key.Scope != "tenant" || newParent == row.ParentID {
		return nil, nil
	}
	ov := newOntologyOverlay()
	ov.parents[row.ID] = newParent
	return d.conceptRetagGate(ctx, key, store.MemoryScopeTenant, "move_chunk", row, ov)
}
