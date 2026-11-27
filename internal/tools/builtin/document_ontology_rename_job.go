package builtin

// document_ontology_rename_job.go — the class-rename job (RFC GQ D9, 4.6; plan
// /loomcycle/docs/rfc-gq-p3-p8-plan, 4.1–4.2 and rows P5.3, P5.4).
//
// WHAT IT DOES. A class is renamed in the ontology in one write that sets its new name
// and records the old one as `@renamed_from <old>`; from then on every reader takes the
// old name as the class (P5.1, internal/memory/ontology_class_former.go). The job then
// rewrites what still carries the old name, step by step:
//
//	ontology_mentions (tenant scope only): the ontology's own mentions of the class —
//	an instance's @a, a scheme's @projects, a relation's @domain and @range, a ref
//	field's class — first, so they follow the rename in the same change (RFC GQ 9b
//	C12, owner ruling 15.1);
//	documents_type, chunks_type, chunk_types (the define_type registry),
//	identity_keys (`<lower class>:<values>`), subject_keys (`<class>:<slug>`, with an
//	alias row so the old key still reaches the same subject — owner ruling Q1),
//	annotation_classes (chunk_annotations.class), node_stats and measure_points (a
//	class's rows are keyed by its name);
//	classification_records: a document root's fields.classification applied_type and
//	proposed_type, last (document_ontology_rename_steps.go).
//
// EVERY SCOPE OF THE TENANT. The tenant scope first, then each user and agent scope
// holding documents, in a fixed order (document_ontology_rename_scopes.go). A scope from
// before the document plane split is rewritten where its documents are, on its agent
// plane, and is not moved: the Document tool's next use of it moves the rewritten rows.
// The job row records the last scope it finished; a slice that runs out of time leaves
// the job unfinished at the scope it was in, which the next slice resumes.
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
// it and running a step again is a no-op. The row only records which scope and step are
// next and what was done. No transaction spans batches: on SQLite a scope has one
// connection, so every read is materialised before the writes it drives.
//
// IT FAILS CLOSED, NEVER HALFWAY UNREADABLE. Until it finishes, the class reads its rows
// under both names. A key the job cannot move because another row already holds the new
// key stops it as blocked, with the key named; the plan lists such collisions first, in
// every scope, and a rename that has any is refused (rename_identity_collision).
//
// A RELEASE WHEN IT IS DONE (plan 4.2 step 7). The finished job takes an `apply` release
// of the ontology it left, unless the newest release already holds that content version
// or the caller takes one itself (an accepted change proposal does).

import (
	"context"
	"encoding/json"
	"fmt"
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
	// holding a class name is TestClassRename_EveryColumnHoldingAClassNameIsListed). ""
	// for a step whose names live outside SQL (ontology bodies, document fields).
	table, column string
	// tenantOnly: the step has work in the tenant scope only (the ontology lives there).
	tenantOnly bool
	// run moves rows until none is left or the deadline passes; it returns the rows it
	// moved and whether the step is finished.
	run renameRunFn
	// left counts the rows still under an old name (the finish check, and the plan); nil
	// for a step the finish check does not recount (classification_records, which would
	// read every document's fields again).
	left renameLeftFn
}

var renameSteps = []renameStep{
	{"ontology_mentions", "", "", true, renameOntologyMentions, leftOntologyMentions},
	{"documents_type", "documents", "type", false, onKey(renameTypeColumn("documents")), leftOnKey(leftTypeColumn("documents"))},
	{"chunks_type", "chunks", "type", false, onKey(renameTypeColumn("chunks")), leftOnKey(leftTypeColumn("chunks"))},
	{"chunk_types", "chunk_types", "name", false, onKey(renameChunkTypes), leftOnKey(leftNamed("chunk_types", "name", ""))},
	{"identity_keys", "chunks", "identity_key", false, onKey(renameIdentityKeys), leftOnKey(leftIdentityKeys)},
	{"subject_keys", "chunk_memory_meta", "natural_key", false, onKey(renameSubjectKeys), leftOnKey(leftSubjectKeys)},
	{"annotation_classes", "chunk_annotations", "class", false, onKey(renameAnnotationClasses), leftOnKey(leftNamed("chunk_annotations", "class", ""))},
	{"node_stats", "node_stats", "node_id", false, onKey(renameNodeStats), leftOnKey(leftNamed("node_stats", "node_id", `kind = 'class'`))},
	{"measure_points", "measure_points", "node_id", false, onKey(renameMeasurePoints), leftOnKey(leftMeasurePoints)},
	{"classification_records", "", "", false, renameClassificationRecords, nil},
}

type (
	renameRunFn  func(*Document, context.Context, renameScope, *ontologyJob, time.Time) (int, bool, error)
	renameLeftFn func(*Document, context.Context, renameScope, []string, string) (int, error)
)

// onKey and leftOnKey run a step that works on the scope's SQL rows alone.
func onKey(f func(*Document, context.Context, sqlmem.ScopeKey, *ontologyJob, time.Time) (int, bool, error)) renameRunFn {
	return func(d *Document, ctx context.Context, sc renameScope, j *ontologyJob, deadline time.Time) (int, bool, error) {
		return f(d, ctx, sc.key, j, deadline)
	}
}

func leftOnKey(f func(*Document, context.Context, sqlmem.ScopeKey, []string, string) (int, error)) renameLeftFn {
	return func(d *Document, ctx context.Context, sc renameScope, from []string, to string) (int, error) {
		return f(d, ctx, sc.key, from, to)
	}
}

// ontologyJob is a row of ontology_jobs.
type ontologyJob struct {
	ID, Kind, Entry string
	From            []string
	To              string
	// ToTags is a concept re-tag's new tags, index-aligned with From (the old ones);
	// stored as to_name's JSON array (document_ontology_retag_job.go).
	ToTags      []string
	State, Step string
	Counts      map[string]int
	Error       string
	// The walk's place, stored as scope_cursor: the last scope finished ("" for none, so
	// the tenant scope is next), how many are finished, and the last document whose
	// classification record was read in the scope after Done.
	Done       string
	ScopesDone int
	Doc        string
	// Scope is the kind of scope the job is at (tenant, user, agent); not stored.
	Scope string
	// Remain is the rows of the scope the job is at still under an old name when the
	// slice ended.
	Remain int
	// ScopesTotal (the walk's length) and the row's times are set by the status read and the
	// resume only (document_ontology_job_status.go).
	ScopesTotal          int
	CreatedAt, UpdatedAt int64
}

func (j *ontologyJob) status() *connector.OntologyJobStatus {
	return &connector.OntologyJobStatus{ID: j.ID, Kind: j.Kind, Entry: j.Entry, From: j.From, To: j.To,
		ToTags: j.ToTags, State: j.State, Step: j.Step, Done: j.Counts, Error: j.Error, Remain: j.Remain,
		Scope: j.Scope, ScopesDone: j.ScopesDone, ScopesTotal: j.ScopesTotal, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt}
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
		if n := pc.Job.CollisionsOtherScopes; n > 0 {
			named = append(named, strconv.Itoa(n)+" in user and agent scopes")
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
		s.jobID, ontologyJobKindClassRename, s.entry, string(from), s.to, jobStatePending, renameSteps[0].name, (&ontologyJob{}).cursor(), "{}", now, now)
}

// withRenameJob adds the rename job's state to update_chunk's answer as rename_job.
func withRenameJob(res tools.Result, st *connector.OntologyJobStatus) tools.Result {
	return withOntologyJob(res, "rename_job", st)
}

// withOntologyJob adds a job's state to an answer under field.
func withOntologyJob(res tools.Result, field string, st *connector.OntologyJobStatus) tools.Result {
	if st == nil || res.IsError {
		return res
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(res.Text))
	dec.UseNumber() // unix-nano timestamps exceed a float64's exact range
	if dec.Decode(&out) != nil {
		return res
	}
	out[field] = st
	b, err := json.Marshal(out)
	if err != nil {
		return res
	}
	res.Text = string(b)
	return res
}

// unfinishedOntologyJob is the tenant's unfinished job, nil when there is none.
func (d *Document) unfinishedOntologyJob(ctx context.Context, key sqlmem.ScopeKey) (*ontologyJob, error) {
	return d.readOntologyJob(ctx, key, `WHERE state IN (?, ?, ?) ORDER BY created_at`, jobStatePending, jobStateRunning, jobStateBlocked)
}

// readOntologyJob is the first job row clause (a WHERE and an ORDER BY) selects, nil
// when it selects none.
func (d *Document) readOntologyJob(ctx context.Context, key sqlmem.ScopeKey, clause string, args ...any) (*ontologyJob, error) {
	res, err := d.planQuery(ctx, key, `SELECT id, kind, entry, from_names, to_name, state, step, counts, coalesce(error, ''), scope_cursor
		FROM ontology_jobs `+clause+` LIMIT 1`, args...)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 10 {
		return nil, err
	}
	r := res.Rows[0]
	j := &ontologyJob{ID: asStr(r[0]), Kind: asStr(r[1]), Entry: asStr(r[2]), To: asStr(r[4]),
		State: asStr(r[5]), Step: asStr(r[6]), Error: asStr(r[8]), Counts: map[string]int{}}
	if err := json.Unmarshal([]byte(asStr(r[3])), &j.From); err != nil || len(j.From) == 0 {
		return nil, fmt.Errorf("job %s: unreadable from_names", j.ID)
	}
	if j.Kind == ontologyJobKindConceptRetag {
		if err := json.Unmarshal([]byte(j.To), &j.ToTags); err != nil || len(j.ToTags) != len(j.From) {
			return nil, fmt.Errorf("job %s: unreadable to_name", j.ID)
		}
		j.To = strings.Join(j.ToTags, ", ")
	}
	_ = json.Unmarshal([]byte(asStr(r[7])), &j.Counts)
	j.readCursor(asStr(r[9]))
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
	ensured := false
	alias := func(oldKey, newKey string) error {
		if !ensured {
			// A scope provisioned before the alias table existed, or one still on its agent
			// plane, may lack it; the job creates it rather than provisioning the scope.
			for _, ddl := range []string{subjectKeyAliasesDDL, subjectKeyAliasesToDDL} {
				if err := d.exec(ctx, key, ddl); err != nil {
					return err
				}
			}
			ensured = true
		}
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
