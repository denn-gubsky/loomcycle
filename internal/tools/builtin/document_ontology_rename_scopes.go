package builtin

// document_ontology_rename_scopes.go — the class-rename job's walk over the tenant's
// scopes (RFC GQ plan P5.4; the job itself is document_ontology_rename_job.go).
//
// ORDER. The tenant scope first (the ontology's own mentions are rewritten there, before
// any row), then every user and agent scope sqlmem lists for the tenant, sorted by kind
// and id. The order is fixed so the job row can say where it is with one value: the last
// scope finished. A scope created behind that point while the job runs is not visited;
// its rows were written under a name that reads as the class (@renamed_from), as any row
// a writer still sends under the former name does.
//
// WHERE A SCOPE'S ROWS ARE. Its document plane, or — for a scope from before the planes
// split that nothing has used since — its agent plane, exactly as the plan counts it
// (planScopePlane). The job rewrites such a scope there and never provisions it: a
// missing table must not re-run provisioning, which would move the tables to the
// document plane under the job's feet (renameJobCtx).
//
// BUDGET. A slice works until its deadline. A scope it does not finish keeps the job
// unfinished at that scope; every row is moved by its own statement, so a scope is never
// half-applied per row, and the next slice redoes the scope's current step.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// renameScope is one scope the job works in.
type renameScope struct {
	key    sqlmem.ScopeKey   // on the plane holding its documents
	mscope store.MemoryScope // its bodies' Memory scope
	tenant bool
	name   string // its place in the walk: renameTenantScope, or "<kind>:<id>"
}

const renameTenantScope = "tenant"

// renameCursor is ontology_jobs.scope_cursor. A value that is not this JSON (P5.3 wrote
// "tenant" before the walk existed) reads as nothing finished, which redoes the tenant
// scope: every step is idempotent, so that costs only time.
type renameCursor struct {
	Done       string `json:"done,omitempty"`
	ScopesDone int    `json:"scopes_done,omitempty"`
	Doc        string `json:"doc,omitempty"`
}

func (j *ontologyJob) cursor() string {
	b, _ := json.Marshal(renameCursor{Done: j.Done, ScopesDone: j.ScopesDone, Doc: j.Doc})
	return string(b)
}

func (j *ontologyJob) readCursor(raw string) {
	var c renameCursor
	if json.Unmarshal([]byte(raw), &c) == nil {
		j.Done, j.ScopesDone, j.Doc = c.Done, c.ScopesDone, c.Doc
	}
}

// renameScopeName is a user or agent scope's place in the walk.
func renameScopeName(sk sqlmem.ScopeKey) string { return sk.Scope + ":" + sk.ScopeID }

// renameJobCtx is the context the job writes a scope under. On a pre-split scope's agent
// plane a statement that meets a missing table must fail, not provision: provisioning
// moves the scope's tables to the document plane mid-job. reensureSchema declines inside
// provisioning, so that marker serves.
func renameJobCtx(ctx context.Context, sc renameScope) context.Context {
	if sc.tenant {
		return ctx
	}
	return provisioning(ctx)
}

// renameScopeNames is the walk: the tenant scope, then the tenant's user and agent
// scopes in order.
func (d *Document) renameScopeNames(ctx context.Context, key sqlmem.ScopeKey) ([]string, map[string]sqlmem.ScopeKey, error) {
	scopes, err := d.SqlMem.ListTenantScopes(ctx, key.Tenant)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]sqlmem.ScopeKey{}
	var names []string
	for _, sk := range scopes {
		// The listing is the tenant's; the comparison is belt-and-braces.
		if sk.Tenant != key.Tenant || (sk.Scope != "user" && sk.Scope != "agent") {
			continue
		}
		n := renameScopeName(sk)
		if _, dup := byName[n]; !dup {
			byName[n] = sk
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return append([]string{renameTenantScope}, names...), byName, nil
}

// renameScopeAfter is the index in names (the walk) of the first scope after done; 0
// when done is "". A finished scope that has since gone is passed over by its name's
// place in the order.
func renameScopeAfter(names []string, done string) int {
	if done == "" {
		return 0
	}
	if done == renameTenantScope {
		return 1
	}
	for i := 1; i < len(names); i++ {
		if names[i] == done {
			return i + 1
		}
		if names[i] > done {
			return i
		}
	}
	return len(names)
}

// renameScopeAt resolves a name of the walk to the scope to work in; ok is false for a
// scope that holds no documents on either plane.
func (d *Document) renameScopeAt(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, name string,
	byName map[string]sqlmem.ScopeKey) (renameScope, bool, error) {
	if name == renameTenantScope {
		return renameScope{key: key, mscope: mscope, tenant: true, name: name}, true, nil
	}
	sk := byName[name]
	at, ok, err := d.planScopePlane(ctx, sk)
	if err != nil || !ok {
		return renameScope{}, false, err
	}
	return renameScope{key: at, mscope: store.MemoryScope(sk.Scope), name: name}, true, nil
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
	return d.runOntologyJob(ctx, key, mscope, j).status()
}

// continueOntologyJob gives an unfinished job one slice. A pending job whose rename
// never reached the ontology is abandoned; one whose rename did is resumed.
func (d *Document) continueOntologyJob(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, j *ontologyJob) *ontologyJob {
	if j.State == jobStatePending {
		read, err := d.ontologyForKey(noWrites(ctx), key, mscope, memrank.OntologyPath, nil)
		if err != nil {
			return j
		}
		if !pendingJobLanded(read, j) {
			j.State = jobStateAbandoned
			_ = d.saveOntologyJob(ctx, key, j)
			return j
		}
		j.State = jobStateRunning
	}
	return d.runOntologyJob(ctx, key, mscope, j)
}

// pendingJobLanded reports whether the ontology write a pending job was recorded for
// reached the ontology: the class carries its new name, or every new tag of a re-tag is
// a concept's (or a scheme's) tag.
func pendingJobLanded(read OntologyRead, j *ontologyJob) bool {
	if j.Kind == ontologyJobKindConceptRetag {
		held := map[string]bool{}
		for _, tag := range ontologyConceptTags(read) {
			held[tag] = true
		}
		for _, tag := range j.ToTags {
			if !held[tag] {
				return false
			}
		}
		return len(j.ToTags) > 0
	}
	e, ok := planEntries(read)[j.Entry]
	return ok && strings.EqualFold(e.Name, j.To)
}

// runOntologyJob works on j for one slice, from the scope and step it is at. It returns
// j as it left it: done, still running (out of time, or stopped by a test), or blocked.
// key and mscope are the tenant scope's.
func (d *Document) runOntologyJob(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, j *ontologyJob) *ontologyJob {
	budget := d.renameJobBudget
	if budget <= 0 {
		budget = renameJobSlice
	}
	deadline := time.Now().Add(budget)
	if j.Counts == nil {
		j.Counts = map[string]int{}
	}
	j.State, j.Error = jobStateRunning, ""
	block := func(why string) *ontologyJob {
		j.State, j.Error = jobStateBlocked, why
		_ = d.saveOntologyJob(ctx, key, j)
		return j
	}
	names, byName, err := d.renameScopeNames(ctx, key)
	if err != nil {
		return block("could not list the tenant's scopes: " + err.Error())
	}
	for at := renameScopeAfter(names, j.Done); at < len(names); at++ {
		sc, ok, err := d.renameScopeAt(ctx, key, mscope, names[at], byName)
		if err != nil {
			return block("could not check a " + strings.SplitN(names[at], ":", 2)[0] + " scope for documents")
		}
		if ok {
			j.Scope = sc.key.Scope
			finished, err := d.runRenameScope(renameJobCtx(ctx, sc), sc, j, deadline)
			if err != nil {
				return block(renameBlockedWhy(sc, j, err))
			}
			if !finished {
				if n, err := d.renameRowsLeft(renameJobCtx(ctx, sc), sc, j); err == nil {
					j.Remain = n
				}
				_ = d.saveOntologyJob(ctx, key, j)
				return j
			}
		}
		j.Done, j.ScopesDone, j.Step, j.Doc = names[at], j.ScopesDone+1, j.steps()[0].name, ""
		_ = d.saveOntologyJob(ctx, key, j)
	}
	j.State, j.Step, j.Scope, j.Remain = jobStateDone, "", "", 0
	_ = d.saveOntologyJob(ctx, key, j)
	if j.Kind == ontologyJobKindClassRename {
		// A re-tag changes no ontology content, so it has nothing to release.
		d.renameJobRelease(ctx, key, j)
	}
	return j
}

// renameBlockedWhy is a blocked job's error, which the tenant's operator reads. In the
// tenant scope it is the step's own (a colliding key is named, as the plan names it). In
// a user or agent scope it names the kind of scope and the step only: the step's error
// can carry another user's stored key or the values a statement was bound with, and the
// plan never lists another user's rows either.
func renameBlockedWhy(sc renameScope, j *ontologyJob, err error) string {
	if sc.tenant {
		return err.Error()
	}
	return "a " + sc.key.Scope + " scope: " + j.Step + ": a row could not be rewritten (a stored key there may already be held by another row; the plan counts such keys under job.collisions_other_scopes)"
}

// runRenameScope runs the steps in one scope from the step j is at, then checks that
// nothing in the scope still holds an old name; rows written under it meanwhile send it
// round again. finished is false when the deadline passed (or a test stopped it).
func (d *Document) runRenameScope(ctx context.Context, sc renameScope, j *ontologyJob, deadline time.Time) (bool, error) {
	steps := j.steps()
	for pass := 0; pass < renameFinishPasses; pass++ {
		for i := stepIndex(steps, j.Step); i < len(steps); i++ {
			st := steps[i]
			j.Step = st.name
			if !st.tenantOnly || sc.tenant {
				n, finished, err := st.run(d, ctx, sc, j, deadline)
				j.Counts[st.name] += n
				if err != nil {
					return false, fmt.Errorf("%s: %w", st.name, err)
				}
				if !finished {
					return false, nil
				}
			}
			if i+1 < len(steps) {
				j.Step = steps[i+1].name
			}
			if d.renameJobStop != nil && d.renameJobStop(sc.key.Scope, st.name) {
				return false, nil
			}
		}
		// The finish check (RFC GQ check P5), per scope.
		left, err := d.renameRowsLeft(ctx, sc, j)
		if err != nil {
			return false, fmt.Errorf("finish check: %w", err)
		}
		if left == 0 {
			return true, nil
		}
		if pastDeadline(ctx, deadline) {
			break
		}
		j.Step, j.Doc = steps[0].name, ""
	}
	j.Step, j.Doc = steps[0].name, ""
	return false, nil
}

func stepIndex(steps []renameStep, name string) int {
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	return 0
}

// renameRowsLeft counts a scope's rows still under an old name, measure points a
// collision kept aside excepted (they are never deleted, RFC GQ 9b), and classification
// records, which only their step reads.
func (d *Document) renameRowsLeft(ctx context.Context, sc renameScope, j *ontologyJob) (int, error) {
	total := 0
	for _, st := range j.steps() {
		if st.left == nil || st.name == "measure_points" || (st.tenantOnly && !sc.tenant) {
			continue
		}
		n, err := st.left(d, ctx, sc, j.From, j.To)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", st.name, err)
		}
		total += n
	}
	return total, nil
}

// saveOntologyJob writes j's state back to its row, in the tenant scope.
func (d *Document) saveOntologyJob(ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob) error {
	counts, _ := json.Marshal(j.Counts)
	var errText any
	if j.Error != "" {
		errText = j.Error
	}
	err := d.exec(ctx, key, `UPDATE ontology_jobs SET state = ?, step = ?, scope_cursor = ?, counts = ?, error = ?, updated_at = ? WHERE id = ?`,
		j.State, j.Step, j.cursor(), string(counts), errText, time.Now().UnixNano(), j.ID)
	if err != nil {
		// The rows already moved stay moved and every step is idempotent, so a lost
		// save only makes the next slice redo a step. The store error is returned, not
		// logged: its text can carry whatever the failing statement was bound with.
		slog.WarnContext(ctx, "ontology rename job: could not save its state", "tenant", key.Tenant, "job", j.ID, "state", j.State, "step", j.Step)
	}
	return err
}
