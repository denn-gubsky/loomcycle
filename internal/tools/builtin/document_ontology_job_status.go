package builtin

// document_ontology_job_status.go — the tenant's ontology job, read and resumed (RFC GQ
// plan P5.6).
//
// A class rename (document_ontology_rename_job.go) or a concept re-tag
// (document_ontology_retag_job.go) carries the change through stored rows in slices. Until
// now the only thing that gave an unfinished job its next slice was the next ontology
// write that moved a name or a tag, and nothing could say how far a job had got. These two
// methods are what every transport's status and resume call: the status reads, the resume
// gives the job one slice under the same lease and with the same continueOntologyJob the
// gate uses, so a resumed job and one carried by the next rename cannot behave differently.
//
// Who may call them is the connector's question (operators only).

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// OntologyJob is ctx's tenant's ontology job: the unfinished one when there is one, else
// the one updated last; nil when the tenant has never had one. It writes nothing.
func (d *Document) OntologyJob(ctx context.Context) (*connector.OntologyJobStatus, error) {
	key, mscope, err := d.ontologyJobKey(ctx)
	if err != nil {
		return nil, err
	}
	ctx = noWrites(ctx)
	j, err := d.unfinishedOntologyJob(ctx, key)
	if err == nil && j == nil {
		j, err = d.readOntologyJob(ctx, key, `ORDER BY updated_at DESC, created_at DESC`)
	}
	if err != nil || j == nil {
		return nil, err
	}
	d.ontologyJobProgress(ctx, key, mscope, j)
	return j.status(), nil
}

// ResumeOntologyJob gives ctx's tenant's unfinished ontology job one slice, as the gate of
// the next rename would (classRenameGate): under the ontology-rename lease, through
// continueOntologyJob, so a pending job whose write never landed is abandoned and any
// other is carried on from its scope and step. id, when not "", must be that job's.
func (d *Document) ResumeOntologyJob(ctx context.Context, id string) (*connector.OntologyJobStatus, error) {
	key, mscope, err := d.ontologyJobKey(ctx)
	if err != nil {
		return nil, err
	}
	owner := "runtime:" + ontologyRenameLeaseName + ":" + newDocID()
	lease, err := d.acquireDocumentLease(ctx, key, ontologyRenameLeaseName, owner, time.Now(), renameLeaseTTL)
	if err != nil {
		return nil, err
	}
	if !lease.Acquired {
		return nil, connector.WithMessage(connector.ErrOntologyJobBusy,
			"a class rename or concept move is being checked or carried out right now; try again in a minute")
	}
	defer func() { _, _ = d.releaseDocumentLease(ctx, key, ontologyRenameLeaseName, owner) }()
	j, err := d.unfinishedOntologyJob(ctx, key)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, connector.WithMessage(connector.ErrOntologyJobNotFound, "this tenant has no unfinished ontology job")
	}
	if id != "" && id != j.ID {
		return nil, connector.WithMessage(connector.ErrOntologyJobNotFound,
			"job "+id+" is not this tenant's unfinished ontology job (that is "+j.ID+"); read the job again")
	}
	before := j.State
	j = d.continueOntologyJob(ctx, key, mscope, j)
	slog.InfoContext(ctx, "ontology job resumed", "tenant", key.Tenant, "job", j.ID, "kind", j.Kind,
		"from_state", before, "state", j.State, "step", j.Step, "scopes_done", j.ScopesDone)
	d.ontologyJobProgress(noWrites(ctx), key, mscope, j)
	return j.status(), nil
}

// ontologyJobKey is the tenant scope a job lives in.
func (d *Document) ontologyJobKey(ctx context.Context) (sqlmem.ScopeKey, store.MemoryScope, error) {
	if d.Store == nil || d.SqlMem == nil {
		return sqlmem.ScopeKey{}, "", connector.WithMessage(connector.ErrOntologyJobUnavailable,
			"ontology jobs need SQL Memory; none is configured")
	}
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return sqlmem.ScopeKey{}, "", connector.WithMessage(connector.ErrOntologyJobUnavailable, err.Error())
	}
	return key, mscope, nil
}

// ontologyJobProgress fills in what the job row does not store: how many scopes the walk
// has, the row's times, and — for an unfinished job a slice did not just leave — the kind
// of scope it is at and the rows still under an old name there. Best effort and read
// only (ctx is noWrites): a figure it cannot read is left out, never guessed.
func (d *Document) ontologyJobProgress(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, j *ontologyJob) {
	if res, err := d.planQuery(ctx, key, `SELECT created_at, updated_at FROM ontology_jobs WHERE id = ?`, j.ID); err == nil &&
		len(res.Rows) == 1 && len(res.Rows[0]) == 2 {
		j.CreatedAt, j.UpdatedAt = asInt64Val(res.Rows[0][0]), asInt64Val(res.Rows[0][1])
	}
	names, byName, err := d.renameScopeNames(ctx, key)
	if err != nil {
		return
	}
	j.ScopesTotal = len(names)
	if !j.unfinished() || j.Scope != "" {
		return
	}
	at := renameScopeAfter(names, j.Done)
	if at >= len(names) {
		return
	}
	sc, ok, err := d.renameScopeAt(ctx, key, mscope, names[at], byName)
	if err != nil || !ok {
		return
	}
	j.Scope = strings.SplitN(sc.name, ":", 2)[0]
	if n, err := d.renameRowsLeft(renameJobCtx(ctx, sc), sc, j); err == nil {
		j.Remain = n
	}
}
