package builtin

// document_ontology_rename_job.go — the class-rename job (RFC GQ D9, 4.6; plan
// /loomcycle/docs/rfc-gq-p3-p8-plan, 4.1–4.2 and row P5.3).
//
// WHAT IT DOES. A class is renamed in the ontology in one write that sets its new name
// and records the old one as `@renamed_from <old>`; from then on every reader takes the
// old name as the class (P5.1, internal/memory/ontology_class_former.go). The job then
// rewrites the stored rows that still carry the old name, step by step:
//
//	documents_type, chunks_type, chunk_types (the define_type registry),
//	identity_keys (`<lower class>:<values>`), subject_keys (`<class>:<slug>`, with an
//	alias row so the old key still reaches the same subject — owner ruling Q1),
//	annotation_classes (chunk_annotations.class), node_stats and measure_points (a
//	class's rows are keyed by its name).
//
// THIS PHASE REWRITES THE TENANT SCOPE ONLY. A rename whose rows reach a user or agent
// scope, or that the ontology itself names elsewhere (an instance's @a, a scheme's
// @projects, a relation's @domain or @range, a field's range), is still refused with
// rename_job_pending: carrying those is later work (P5.4 and the C12 rewrite), and
// starting a job that cannot finish would leave the tenant's only job slot taken.
//
// ONE JOB AT A TIME PER TENANT. The job row lives in the tenant scope (ontology_jobs).
// A rename is refused (rename_job_running) while an unfinished one exists, after the
// refusing call has first tried to finish it: an interrupted job is resumed by the next
// rename, so it never needs a separate trigger to make progress. The named lease
// `ontology-rename` serialises the check, the write and the job's slice across replicas;
// it is the runtime's alone (documentLeaseInput refuses it to a caller).
//
// RESUMABLE BY CONSTRUCTION. Each batch is one statement (or, for keys, one statement
// per row) whose predicate is "still under an old name", so a rewritten row drops out of
// it and running a step again is a no-op. The row only records which step is next and
// what was done. No transaction spans batches: on SQLite a scope has one connection, so
// every read is materialised before the writes it drives.
//
// IT FAILS CLOSED, NEVER HALFWAY UNREADABLE. Until it finishes, the class reads its rows
// under both names. A key the job cannot move because another row already holds the new
// key stops it as blocked, with the key named; the plan lists such collisions first, and
// a rename that has any is refused (rename_identity_collision).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ontologyJobsDDL holds the jobs carrying an ontology change through stored rows. In
// the tenant scope only; from_names and counts are JSON.
const ontologyJobsDDL = `CREATE TABLE IF NOT EXISTS ontology_jobs (
		id TEXT PRIMARY KEY, kind TEXT NOT NULL, entry TEXT NOT NULL,
		from_names TEXT NOT NULL, to_name TEXT NOT NULL,
		state TEXT NOT NULL, step TEXT NOT NULL, scope_cursor TEXT NOT NULL,
		counts TEXT NOT NULL, error TEXT,
		created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`

// subjectKeyAliasesDDL maps a subject key the rename job rewrote to the key it became,
// so a writer still deriving the old key reaches the same subject (chunkIDByNaturalKey).
const subjectKeyAliasesDDL = `CREATE TABLE IF NOT EXISTS subject_key_aliases (
		from_key TEXT PRIMARY KEY, to_key TEXT NOT NULL, job_id TEXT NOT NULL,
		created_at BIGINT NOT NULL)`

const subjectKeyAliasesToDDL = `CREATE INDEX IF NOT EXISTS subject_key_aliases_to ON subject_key_aliases(to_key)`

const (
	// ontologyRenameLeaseName is the runtime's lease on the tenant scope while a rename
	// is checked, written and carried. Reserved: no caller may take it (op=lease).
	ontologyRenameLeaseName = "ontology-rename"
	// renameJobRunningCode refuses a rename while another rename job is unfinished.
	renameJobRunningCode = "rename_job_running"
	// renameCollisionCode refuses a rename that would give a stored key to a row while
	// another row holds it.
	renameCollisionCode = "rename_identity_collision"

	ontologyJobKindClassRename = "class_rename"

	jobStatePending   = "pending"   // the row is written, the rename write not yet
	jobStateRunning   = "running"   // the rename is written; rows are being moved
	jobStateBlocked   = "blocked"   // a step cannot go on (a key collision); resumed by the next rename
	jobStateDone      = "done"      // nothing in the tenant scope holds the old name
	jobStateAbandoned = "abandoned" // the rename write never happened

	// renameJobSlice is how long one call works on the job before it returns, as the
	// plan's scope pass is bounded (document_ontology_plan_counts.go).
	renameJobSlice = 20 * time.Second
	// renameLeaseTTL outlives a slice, the plan before it and the write, and still
	// frees the scope soon after a crash.
	renameLeaseTTL = 2 * time.Minute
	// renameBatch is the rows one statement moves.
	renameBatch = 500
	// renameFinishPasses bounds the re-runs the finish check may ask for when rows
	// under the old name were written while the job ran.
	renameFinishPasses = 3
)

// renameStep is one step of the job, in the order of the plan's table 4.1.
type renameStep struct {
	name string
	// table.column is where the step finds the old name (the census of every column
	// holding a class name is TestClassRename_EveryColumnHoldingAClassNameIsListed).
	table, column string
	// run moves rows until none is left or the deadline passes; it returns the rows it
	// moved and whether the step is finished.
	run func(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, deadline time.Time) (int, bool, error)
	// left counts the rows still under an old name (the finish check, and the plan).
	left func(d *Document, ctx context.Context, key sqlmem.ScopeKey, from []string, to string) (int, error)
}

var renameSteps = []renameStep{
	{"documents_type", "documents", "type", renameTypeColumn("documents"), leftTypeColumn("documents")},
	{"chunks_type", "chunks", "type", renameTypeColumn("chunks"), leftTypeColumn("chunks")},
	{"chunk_types", "chunk_types", "name", renameChunkTypes, leftNamed("chunk_types", "name", "")},
	{"identity_keys", "chunks", "identity_key", renameIdentityKeys, leftIdentityKeys},
	{"subject_keys", "chunk_memory_meta", "natural_key", renameSubjectKeys, leftSubjectKeys},
	{"annotation_classes", "chunk_annotations", "class", renameAnnotationClasses, leftNamed("chunk_annotations", "class", "")},
	{"node_stats", "node_stats", "node_id", renameNodeStats, leftNamed("node_stats", "node_id", `kind = 'class'`)},
	{"measure_points", "measure_points", "node_id", renameMeasurePoints, leftMeasurePoints},
}

// ontologyJob is a row of ontology_jobs.
type ontologyJob struct {
	ID, Kind, Entry string
	From            []string
	To              string
	State, Step     string
	Counts          map[string]int
	Error           string
	// Remain is the tenant-scope rows still under an old name when the slice ended.
	Remain int
}

func (j *ontologyJob) status() *connector.OntologyJobStatus {
	return &connector.OntologyJobStatus{ID: j.ID, Kind: j.Kind, Entry: j.Entry, From: j.From, To: j.To,
		State: j.State, Step: j.Step, Done: j.Counts, Error: j.Error, Remain: j.Remain}
}

func (j *ontologyJob) unfinished() bool {
	return j.State == jobStatePending || j.State == jobStateRunning || j.State == jobStateBlocked
}

// renameJobStart is a class rename the gate admitted: the lease it holds and the job it
// will start once the rename is written.
type renameJobStart struct {
	owner   string
	jobID   string
	entry   string
	oldName string
	from    []string
	to      string
}

// release lets go of the lease. Best effort: it expires on its own.
func (s *renameJobStart) release(ctx context.Context, d *Document, key sqlmem.ScopeKey) {
	if s != nil && s.owner != "" {
		_, _ = d.releaseDocumentLease(ctx, key, ontologyRenameLeaseName, s.owner)
	}
}

// classRenameGate decides a rename of a class: nil, nil when nothing rests on it (it
// renames directly, as before); a refusal when the job cannot carry it now; otherwise
// the start, holding the lease until the caller releases it. ch is the rename as the
// update plans it.
func (d *Document) classRenameGate(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op string,
	ch connector.OntologyChange) (*renameJobStart, *tools.Result) {
	refuse := func(code, why, next string) *tools.Result {
		r := errBusiness(op+": "+code+": "+why+". Nothing was changed.", next)
		return &r
	}
	owner := "runtime:" + ontologyRenameLeaseName + ":" + newDocID()
	lease, err := d.acquireDocumentLease(ctx, key, ontologyRenameLeaseName, owner, time.Now(), renameLeaseTTL)
	if err != nil {
		r := errFrom(op+": could not take the ontology rename lease, so nothing was changed: "+err.Error(), err)
		return nil, &r
	}
	if !lease.Acquired {
		return nil, refuse(renameJobRunningCode, "another class rename is being checked or carried out right now",
			"Try again in a minute.")
	}
	start := &renameJobStart{owner: owner}
	fail := func(r *tools.Result) (*renameJobStart, *tools.Result) {
		start.release(ctx, d, key)
		return nil, r
	}
	// An unfinished job first gets a slice of its own: it may finish here.
	if job, err := d.unfinishedOntologyJob(ctx, key); err != nil {
		r := errFrom(op+": could not read the rename job, so nothing was changed: "+err.Error(), err)
		return fail(&r)
	} else if job != nil {
		job = d.continueOntologyJob(ctx, key, mscope, job)
		if job.unfinished() {
			why := "the rename of " + strings.Join(quoteAll(job.From), ", ") + " to " + strconv.Quote(job.To) +
				" is still being carried through stored rows (job " + job.ID + ", " + job.State + " at " + job.Step + ")"
			if job.Error != "" {
				why += ": " + job.Error
			}
			return fail(refuse(renameJobRunningCode, why, "Rename again once it has finished; each rename call also carries it on."))
		}
	}
	plan, err := d.planOntologyAt(ctx, key, mscope, []connector.OntologyChange{ch})
	if err != nil {
		return fail(planGateError(op, err))
	}
	if len(plan.Changes) != 1 {
		start.release(ctx, d, key)
		return nil, nil
	}
	pc := plan.Changes[0]
	if why, pending := renameJobPending(pc, plan.Complete); pending {
		return fail(refuse(renameJobPendingCode, why, "Keep the name until the rename job can carry it, or retire the class and declare the new name as a new class."))
	}
	if pc.Job == nil {
		start.release(ctx, d, key) // nothing rests on it: a direct rename
		return nil, nil
	}
	if pc.Job.CollisionsTotal > 0 {
		var named []string
		for _, c := range pc.Job.Collisions {
			named = append(named, c.Column+" "+strconv.Quote(c.Key)+" would become "+strconv.Quote(c.Into)+", held by "+c.Holder)
		}
		return fail(refuse(renameCollisionCode, strconv.Itoa(pc.Job.CollisionsTotal)+" stored keys would collide under the new name ("+
			strings.Join(named, "; ")+")", "Merge or re-key the colliding rows first; the plan lists them under job.collisions."))
	}
	// Every name the job rewrites: the class's own, and the former name it read rows
	// under, which the new @renamed_from replaces.
	read, err := d.ontologyForKey(noWrites(ctx), key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		r := errFrom(op+": could not read the tenant ontology, so nothing was changed: "+err.Error(), err)
		return fail(&r)
	}
	e := planEntries(read)[pc.Entry.ChunkID]
	start.entry, start.oldName, start.to = pc.Entry.ChunkID, pc.Entry.Name, strings.TrimSpace(ch.Name)
	start.from = []string{pc.Entry.Name}
	if e.former != "" {
		start.from = appendUnique(start.from, e.former)
	}
	return start, nil
}

// withRenamedFrom is body with its @renamed_from line set to old: any earlier one is
// replaced, since a class records one former name (P5.1 reads the whole value).
func withRenamedFrom(body, old string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "@renamed_from") {
			continue
		}
		kept = append(kept, line)
	}
	rest := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if strings.TrimSpace(rest) == "" {
		return "@renamed_from " + old + "\n"
	}
	return rest + "\n@renamed_from " + old + "\n"
}

// createRenameJob writes the job row, pending until the rename is written.
func (d *Document) createRenameJob(ctx context.Context, key sqlmem.ScopeKey, s *renameJobStart) error {
	s.jobID = newDocID()
	from, _ := json.Marshal(s.from)
	now := time.Now().UnixNano()
	return d.exec(ctx, key, `INSERT INTO ontology_jobs (id, kind, entry, from_names, to_name, state, step, scope_cursor, counts, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		s.jobID, ontologyJobKindClassRename, s.entry, string(from), s.to, jobStatePending, renameSteps[0].name, "tenant", "{}", now, now)
}

// withRenameJob adds the rename job's state to update_chunk's answer as rename_job.
func withRenameJob(res tools.Result, st *connector.OntologyJobStatus) tools.Result {
	if st == nil || res.IsError {
		return res
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(res.Text))
	dec.UseNumber() // unix-nano timestamps exceed a float64's exact range
	if dec.Decode(&out) != nil {
		return res
	}
	out["rename_job"] = st
	b, err := json.Marshal(out)
	if err != nil {
		return res
	}
	res.Text = string(b)
	return res
}

// startRenameJob runs the first slice of a job whose rename was just written.
func (d *Document) startRenameJob(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, s *renameJobStart) *connector.OntologyJobStatus {
	j := &ontologyJob{ID: s.jobID, Kind: ontologyJobKindClassRename, Entry: s.entry, From: s.from, To: s.to,
		State: jobStateRunning, Step: renameSteps[0].name, Counts: map[string]int{}}
	if err := d.saveOntologyJob(ctx, key, j); err != nil {
		// The rename is written and reads under both names; the next rename finds the
		// row pending and resumes it.
		st := j.status()
		st.State, st.Error = jobStatePending, "could not record the job's start: "+err.Error()
		return st
	}
	return d.runOntologyJob(ctx, key, j).status()
}

// continueOntologyJob gives an unfinished job one slice. A pending job whose rename
// never reached the ontology is abandoned; one whose rename did is resumed.
func (d *Document) continueOntologyJob(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, j *ontologyJob) *ontologyJob {
	if j.State == jobStatePending {
		read, err := d.ontologyForKey(noWrites(ctx), key, mscope, memrank.OntologyPath, nil)
		if err != nil {
			return j
		}
		if e, ok := planEntries(read)[j.Entry]; !ok || !strings.EqualFold(e.Name, j.To) {
			j.State = jobStateAbandoned
			_ = d.saveOntologyJob(ctx, key, j)
			return j
		}
		j.State = jobStateRunning
	}
	return d.runOntologyJob(ctx, key, j)
}

// runOntologyJob works on j for one slice, from the step it is at. It returns j as it
// left it: done, still running (out of time, or stopped by a test), or blocked.
func (d *Document) runOntologyJob(ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob) *ontologyJob {
	budget := d.renameJobBudget
	if budget <= 0 {
		budget = renameJobSlice
	}
	deadline := time.Now().Add(budget)
	if j.Counts == nil {
		j.Counts = map[string]int{}
	}
	j.State, j.Error = jobStateRunning, ""
	defer func() {
		// What the caller is told is left; best effort, the job row is what resumes.
		if j.State == jobStateDone {
			j.Remain = 0
		} else if n, err := d.renameRowsLeft(ctx, key, j.From, j.To); err == nil {
			j.Remain = n
		}
	}()
	for pass := 0; pass < renameFinishPasses; pass++ {
		at := stepIndex(j.Step)
		for i := at; i < len(renameSteps); i++ {
			st := renameSteps[i]
			j.Step = st.name
			n, finished, err := st.run(d, ctx, key, j, deadline)
			j.Counts[st.name] += n
			if err != nil {
				j.State, j.Error = jobStateBlocked, st.name+": "+err.Error()
				_ = d.saveOntologyJob(ctx, key, j)
				return j
			}
			if !finished {
				_ = d.saveOntologyJob(ctx, key, j)
				return j
			}
			if i+1 < len(renameSteps) {
				j.Step = renameSteps[i+1].name
			}
			_ = d.saveOntologyJob(ctx, key, j)
			if d.renameJobStop != nil && d.renameJobStop(st.name) {
				return j
			}
		}
		// The finish check: nothing in the tenant scope may hold an old name (RFC GQ
		// check P5). Rows written under it while the job ran send it round again.
		left, err := d.renameRowsLeft(ctx, key, j.From, j.To)
		if err != nil {
			j.State, j.Error = jobStateBlocked, "finish check: "+err.Error()
			_ = d.saveOntologyJob(ctx, key, j)
			return j
		}
		if left == 0 {
			j.State, j.Step = jobStateDone, ""
			_ = d.saveOntologyJob(ctx, key, j)
			return j
		}
		if time.Now().After(deadline) {
			break
		}
		j.Step = renameSteps[0].name
	}
	j.Step = renameSteps[0].name
	_ = d.saveOntologyJob(ctx, key, j)
	return j
}

func stepIndex(name string) int {
	for i, s := range renameSteps {
		if s.name == name {
			return i
		}
	}
	return 0
}

// renameRowsLeft counts the tenant-scope rows still under an old name, measure points a
// collision kept aside excepted (they are never deleted, RFC GQ 9b).
func (d *Document) renameRowsLeft(ctx context.Context, key sqlmem.ScopeKey, from []string, to string) (int, error) {
	total := 0
	for _, st := range renameSteps {
		if st.name == "measure_points" {
			continue
		}
		n, err := st.left(d, ctx, key, from, to)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", st.name, err)
		}
		total += n
	}
	return total, nil
}

// saveOntologyJob writes j's state back to its row.
func (d *Document) saveOntologyJob(ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob) error {
	counts, _ := json.Marshal(j.Counts)
	var errText any
	if j.Error != "" {
		errText = j.Error
	}
	err := d.exec(ctx, key, `UPDATE ontology_jobs SET state = ?, step = ?, counts = ?, error = ?, updated_at = ? WHERE id = ?`,
		j.State, j.Step, string(counts), errText, time.Now().UnixNano(), j.ID)
	if err != nil {
		// The rows already moved stay moved and every step is idempotent, so a lost
		// save only makes the next slice redo a step. The store error is returned, not
		// logged: its text can carry whatever the failing statement was bound with.
		slog.WarnContext(ctx, "ontology rename job: could not save its state", "tenant", key.Tenant, "job", j.ID, "state", j.State, "step", j.Step)
	}
	return err
}

// unfinishedOntologyJob is the tenant's unfinished job, nil when there is none.
func (d *Document) unfinishedOntologyJob(ctx context.Context, key sqlmem.ScopeKey) (*ontologyJob, error) {
	res, err := d.planQuery(ctx, key, `SELECT id, kind, entry, from_names, to_name, state, step, counts, coalesce(error, '')
		FROM ontology_jobs WHERE state IN (?, ?, ?) ORDER BY created_at LIMIT 1`, jobStatePending, jobStateRunning, jobStateBlocked)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 9 {
		return nil, err
	}
	r := res.Rows[0]
	j := &ontologyJob{ID: asStr(r[0]), Kind: asStr(r[1]), Entry: asStr(r[2]), To: asStr(r[4]),
		State: asStr(r[5]), Step: asStr(r[6]), Error: asStr(r[8]), Counts: map[string]int{}}
	if err := json.Unmarshal([]byte(asStr(r[3])), &j.From); err != nil || len(j.From) == 0 {
		return nil, fmt.Errorf("job %s: unreadable from_names", j.ID)
	}
	_ = json.Unmarshal([]byte(asStr(r[7])), &j.Counts)
	return j, nil
}

// renameSpellings is the stored spellings of the old names in table.col (with prefix,
// the "<spelling>:" prefixes), matched under case folding as the plan matches them,
// less the new name's own spelling (newSpelling) — a case-only rename must not
// rewrite a row onto itself forever.
func (d *Document) renameSpellings(ctx context.Context, key sqlmem.ScopeKey, table, col, where string, from []string, prefix bool, newSpelling string) ([]string, error) {
	vals, err := d.planSpellings(ctx, key, table, col, where, from, prefix)
	if err != nil {
		return nil, err
	}
	out := vals[:0]
	for _, v := range vals {
		if v != newSpelling {
			out = append(out, v)
		}
	}
	return out, nil
}

func pastDeadline(ctx context.Context, deadline time.Time) bool {
	return ctx.Err() != nil || time.Now().After(deadline)
}

// renameTypeColumn moves table.type from an old name to the new one, in batches.
func renameTypeColumn(table string) func(*Document, context.Context, sqlmem.ScopeKey, *ontologyJob, time.Time) (int, bool, error) {
	return func(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, deadline time.Time) (int, bool, error) {
		spell, err := d.renameSpellings(ctx, key, table, "type", "", j.From, false, j.To)
		if err != nil || len(spell) == 0 {
			return 0, err == nil, err
		}
		ph, args := inPlaceholders(spell)
		moved := 0
		for {
			if pastDeadline(ctx, deadline) {
				return moved, false, nil
			}
			n, err := d.execCount(ctx, key, `UPDATE `+table+` SET type = ? WHERE id IN (SELECT id FROM `+table+
				` WHERE type IN (`+ph+`) LIMIT `+strconv.Itoa(renameBatch)+`)`, append([]any{j.To}, args...)...)
			if err != nil {
				return moved, false, err
			}
			moved += int(n)
			if n == 0 {
				return moved, true, nil
			}
		}
	}
}

func leftTypeColumn(table string) func(*Document, context.Context, sqlmem.ScopeKey, []string, string) (int, error) {
	return leftNamed(table, "type", "")
}

// leftNamed counts the rows of table whose col is an old name.
func leftNamed(table, col, where string) func(*Document, context.Context, sqlmem.ScopeKey, []string, string) (int, error) {
	return func(d *Document, ctx context.Context, key sqlmem.ScopeKey, from []string, to string) (int, error) {
		spell, err := d.renameSpellings(ctx, key, table, col, where, from, false, to)
		if err != nil || len(spell) == 0 {
			return 0, err
		}
		ph, args := inPlaceholders(spell)
		stmt := `SELECT count(*) FROM ` + table + ` WHERE ` + col + ` IN (` + ph + `)`
		if where != "" {
			stmt += ` AND ` + where
		}
		return d.planCount(ctx, key, stmt, args...)
	}
}

// renameChunkTypes moves the define_type registry: where a document already defines the
// new name, its old definition is dropped (the primary key is (document_id, name)).
func renameChunkTypes(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, _ time.Time) (int, bool, error) {
	spell, err := d.renameSpellings(ctx, key, "chunk_types", "name", "", j.From, false, j.To)
	if err != nil || len(spell) == 0 {
		return 0, err == nil, err
	}
	moved := 0
	for _, s := range spell {
		if err := d.exec(ctx, key, `DELETE FROM chunk_types WHERE name = ? AND EXISTS (SELECT 1 FROM chunk_types n
			WHERE n.document_id = chunk_types.document_id AND n.name = ?)`, s, j.To); err != nil {
			return moved, false, err
		}
		n, err := d.execCount(ctx, key, `UPDATE chunk_types SET name = ? WHERE name = ?`, j.To, s)
		if err != nil {
			return moved, false, err
		}
		moved += int(n)
	}
	return moved, true, nil
}

// identityPrefix is the prefix an identity key carries for class name (IdentityKey
// lower-cases it).
func identityPrefix(class string) string { return strings.ToLower(class) }

// renameKeyColumn moves the keys of table.col whose prefix is an old name to the new
// prefix, one row at a time (each key is unique per scope), reading a batch before it
// writes. alias, when set, records each move first.
func renameKeyColumn(d *Document, ctx context.Context, key sqlmem.ScopeKey, table, idCol, col string, from []string,
	newPrefix string, deadline time.Time, alias func(oldKey, newKey string) error) (int, bool, error) {
	spell, err := d.renameSpellings(ctx, key, table, col, "", from, true, newPrefix)
	if err != nil || len(spell) == 0 {
		return 0, err == nil, err
	}
	cond, args := prefixAny(col, spell)
	moved := 0
	for {
		if pastDeadline(ctx, deadline) {
			return moved, false, nil
		}
		res, err := d.query(ctx, key, `SELECT `+idCol+`, `+col+` FROM `+table+` WHERE `+cond+` ORDER BY `+idCol+
			` LIMIT `+strconv.Itoa(renameBatch), args...)
		if err != nil {
			return moved, false, err
		}
		if len(res.Rows) == 0 {
			return moved, true, nil
		}
		for _, r := range res.Rows {
			id, oldKey := asStr(r[0]), asStr(r[1])
			newKey, ok := rekey(oldKey, spell, newPrefix)
			if !ok {
				continue
			}
			if alias != nil {
				if err := alias(oldKey, newKey); err != nil {
					return moved, false, err
				}
			}
			n, err := d.execCount(ctx, key, `UPDATE `+table+` SET `+col+` = ? WHERE `+idCol+` = ? AND `+col+` = ?`, newKey, id, oldKey)
			if err != nil {
				return moved, false, fmt.Errorf("%s %q cannot become %q, which another row holds: %w", col, oldKey, newKey, err)
			}
			moved += int(n)
		}
	}
}

// rekey is key with its old prefix (one of spell) replaced by newPrefix.
func rekey(key string, spell []string, newPrefix string) (string, bool) {
	for _, s := range spell {
		if strings.HasPrefix(key, s+":") {
			return newPrefix + key[len(s):], true
		}
	}
	return "", false
}

func renameIdentityKeys(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, deadline time.Time) (int, bool, error) {
	return renameKeyColumn(d, ctx, key, "chunks", "id", "identity_key", j.From, identityPrefix(j.To), deadline, nil)
}

func leftIdentityKeys(d *Document, ctx context.Context, key sqlmem.ScopeKey, from []string, to string) (int, error) {
	return leftPrefixed(d, ctx, key, "chunks", "identity_key", from, identityPrefix(to))
}

// renameSubjectKeys moves subject keys, recording each as an alias first: an old key
// then reaches the subject whether or not its own row has moved yet. An alias that
// already led to the old key is pointed on to the new one, so a chain of renames keeps
// every older key working.
func renameSubjectKeys(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, deadline time.Time) (int, bool, error) {
	alias := func(oldKey, newKey string) error {
		now := time.Now().UnixNano()
		if err := d.exec(ctx, key, `UPDATE subject_key_aliases SET to_key = ? WHERE to_key = ?`, newKey, oldKey); err != nil {
			return err
		}
		if err := d.exec(ctx, key, `UPDATE subject_key_aliases SET to_key = ?, job_id = ? WHERE from_key = ?`, newKey, j.ID, oldKey); err != nil {
			return err
		}
		return d.exec(ctx, key, `INSERT INTO subject_key_aliases (from_key, to_key, job_id, created_at)
			SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM subject_key_aliases WHERE from_key = ?)`, oldKey, newKey, j.ID, now, oldKey)
	}
	return renameKeyColumn(d, ctx, key, "chunk_memory_meta", "chunk_id", "natural_key", j.From, j.To, deadline, alias)
}

func leftSubjectKeys(d *Document, ctx context.Context, key sqlmem.ScopeKey, from []string, to string) (int, error) {
	return leftPrefixed(d, ctx, key, "chunk_memory_meta", "natural_key", from, to)
}

func leftPrefixed(d *Document, ctx context.Context, key sqlmem.ScopeKey, table, col string, from []string, newPrefix string) (int, error) {
	spell, err := d.renameSpellings(ctx, key, table, col, "", from, true, newPrefix)
	if err != nil || len(spell) == 0 {
		return 0, err
	}
	cond, args := prefixAny(col, spell)
	return d.planCount(ctx, key, `SELECT count(*) FROM `+table+` WHERE `+cond, args...)
}

func renameAnnotationClasses(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, _ time.Time) (int, bool, error) {
	spell, err := d.renameSpellings(ctx, key, "chunk_annotations", "class", "", j.From, false, j.To)
	if err != nil || len(spell) == 0 {
		return 0, err == nil, err
	}
	ph, args := inPlaceholders(spell)
	n, err := d.execCount(ctx, key, `UPDATE chunk_annotations SET class = ? WHERE class IN (`+ph+`)`, append([]any{j.To}, args...)...)
	return int(n), err == nil, err
}

// renameNodeStats moves a class's stored statistics; where the new name already has a
// row, the old one is dropped (it is recomputed on the next pass anyway).
func renameNodeStats(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, _ time.Time) (int, bool, error) {
	spell, err := d.renameSpellings(ctx, key, "node_stats", "node_id", `kind = 'class'`, j.From, false, j.To)
	if err != nil || len(spell) == 0 {
		return 0, err == nil, err
	}
	moved := 0
	for _, s := range spell {
		if err := d.exec(ctx, key, `DELETE FROM node_stats WHERE kind = 'class' AND node_id = ? AND EXISTS
			(SELECT 1 FROM node_stats n WHERE n.kind = 'class' AND n.node_id = ?)`, s, j.To); err != nil {
			return moved, false, err
		}
		n, err := d.execCount(ctx, key, `UPDATE node_stats SET node_id = ? WHERE kind = 'class' AND node_id = ?`, j.To, s)
		if err != nil {
			return moved, false, err
		}
		moved += int(n)
	}
	return moved, true, nil
}

// measureClassRows narrows measure_points to class nodes: a concept's node id is an
// ontology chunk's id (as the plan counts them).
const measureClassRows = `node_id NOT IN (SELECT id FROM chunks)`

// renameMeasurePoints moves a class's series. A point is never deleted (RFC GQ 9b): one
// whose twin under the new name has the same key is left where it is and not counted
// as left over.
func renameMeasurePoints(d *Document, ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob, _ time.Time) (int, bool, error) {
	spell, err := d.renameSpellings(ctx, key, "measure_points", "node_id", measureClassRows, j.From, false, j.To)
	if err != nil || len(spell) == 0 {
		return 0, err == nil, err
	}
	moved := 0
	for _, s := range spell {
		n, err := d.execCount(ctx, key, `UPDATE measure_points SET node_id = ? WHERE node_id = ? AND `+measureClassRows+
			` AND NOT EXISTS (SELECT 1 FROM measure_points n WHERE n.measure = measure_points.measure AND n.node_id = ?
			  AND n.subject_id = measure_points.subject_id AND n.period_start = measure_points.period_start
			  AND n.computed_at = measure_points.computed_at)`, j.To, s, j.To)
		if err != nil {
			return moved, false, err
		}
		moved += int(n)
	}
	return moved, true, nil
}

func leftMeasurePoints(d *Document, ctx context.Context, key sqlmem.ScopeKey, from []string, to string) (int, error) {
	return leftNamed("measure_points", "node_id", measureClassRows)(d, ctx, key, from, to)
}

// ---- the plan's job block ----

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strconv.Quote(n)
	}
	return out
}

// runeLen is utf8.RuneCountInString, for substr positions (characters on both tiers).
func runeLen(s string) int { return utf8.RuneCountInString(s) }
