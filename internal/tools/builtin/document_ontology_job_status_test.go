package builtin

// document_ontology_job_status_test.go — RFC GQ P5.6: reading the tenant's ontology job
// and giving an unfinished one a slice, the methods every transport's status and resume
// call. Each starts from a job a rename left unfinished (stopped after a step by the
// test hook), as the rename job's own tests do.

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// stoppedRename renames Bulletin with the job stopped after documents_type in the tenant
// scope, so it is left running at chunks_type with rows still under the old name.
func stoppedRename(t *testing.T, f *renameFixture) {
	t.Helper()
	f.d.renameJobStop = func(scope, step string) bool { return scope == "tenant" && step == "documents_type" }
	if out, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateRunning {
		t.Fatalf("stopped rename: %s", r.Text)
	}
	f.d.renameJobStop = nil
}

// TestOntologyJob_StatusReadsTheUnfinishedJobElseTheLastOne: no job reads as none; a
// stopped rename reads as running at its next step in the tenant scope, with the rows
// left there counted and every scope of the walk, and reading it writes nothing; once it
// has finished, the status is that finished job.
func TestOntologyJob_StatusReadsTheUnfinishedJobElseTheLastOne(t *testing.T) {
	f := newRenameFixture(t)
	if st, err := f.d.OntologyJob(f.ctx); err != nil || st != nil {
		t.Fatalf("status with no job = %+v, %v; want none", st, err)
	}
	stoppedRename(t, f)
	before := f.state()
	st, err := f.d.OntologyJob(f.ctx)
	if err != nil || st == nil {
		t.Fatalf("status of the stopped job = %+v, %v", st, err)
	}
	if st.Kind != ontologyJobKindClassRename || st.State != jobStateRunning || st.Step != "chunks_type" ||
		!reflect.DeepEqual(st.From, []string{"Bulletin"}) || st.To != "Circular" || st.Entry != f.bulletin {
		t.Errorf("status = %+v, want the class rename Bulletin → Circular running at chunks_type", st)
	}
	if st.Scope != "tenant" || st.ScopesDone != 0 || st.ScopesTotal != 3 || st.Remain == 0 {
		t.Errorf("progress = scope %q, %d of %d scopes done, %d rows left; want the tenant scope, 0 of 3, rows left",
			st.Scope, st.ScopesDone, st.ScopesTotal, st.Remain)
	}
	if st.Done["documents_type"] == 0 || st.CreatedAt == 0 || st.UpdatedAt < st.CreatedAt {
		t.Errorf("status = done %v, created %d, updated %d; want documents_type counted and the row's times", st.Done, st.CreatedAt, st.UpdatedAt)
	}
	if got := f.state(); !reflect.DeepEqual(got, before) {
		t.Errorf("reading the status moved rows:\n got %v\nwant %v", got, before)
	}

	if _, err := f.d.ResumeOntologyJob(f.ctx, st.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, err = f.d.OntologyJob(f.ctx)
	if err != nil || st == nil || st.State != jobStateDone || st.Remain != 0 || st.ScopesDone != 3 || st.Scope != "" {
		t.Errorf("status after the job finished = %+v, %v; want it done after 3 scopes", st, err)
	}
}

// TestOntologyJob_ResumeCarriesTheJobToWhereAnUninterruptedRunEnds: resuming a stopped
// rename leaves exactly the rows an uninterrupted rename leaves; a pending job whose
// rename never reached the ontology is abandoned and moves nothing.
func TestOntologyJob_ResumeCarriesTheJobToWhereAnUninterruptedRunEnds(t *testing.T) {
	whole := newRenameFixture(t)
	if out, r := whole.rename(whole.ctx, whole.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("uninterrupted rename: %s", r.Text)
	}
	want := whole.state()

	f := newRenameFixture(t)
	stoppedRename(t, f)
	job, _ := f.d.unfinishedOntologyJob(f.ctx, f.key)
	st, err := f.d.ResumeOntologyJob(f.ctx, job.ID)
	if err != nil || st.State != jobStateDone || st.ID != job.ID {
		t.Fatalf("resume = %+v, %v; want job %s done", st, err, job.ID)
	}
	if got := f.state(); !reflect.DeepEqual(got, want) {
		t.Errorf("resumed:\n got %v\nwant %v", got, want)
	}

	p := newRenameFixture(t)
	before := p.state()
	s := &renameJobStart{entry: p.bulletin, from: []string{"Bulletin"}, to: "Circular"}
	if err := p.d.createRenameJob(p.ctx, p.key, s); err != nil {
		t.Fatal(err)
	}
	pending, _ := p.d.unfinishedOntologyJob(p.ctx, p.key)
	if pending == nil {
		t.Fatal("no pending job recorded")
	}
	if st, err := p.d.ResumeOntologyJob(p.ctx, pending.ID); err != nil || st.State != jobStateAbandoned {
		t.Errorf("resume of a pending job that never landed = %+v, %v; want abandoned", st, err)
	}
	if got := p.state(); !reflect.DeepEqual(got, before) {
		t.Errorf("an abandoned job moved rows:\n got %v\nwant %v", got, before)
	}
}

// TestOntologyJob_ResumeIsRefusedWithoutItsJobOrWhileTheLeaseIsHeld: no unfinished job
// is job_not_found; an id that is not the unfinished job's is job_not_found and leaves
// the job where it was; while another call holds the rename lease the resume is
// job_busy and moves nothing; once the lease is free the same resume goes through.
func TestOntologyJob_ResumeIsRefusedWithoutItsJobOrWhileTheLeaseIsHeld(t *testing.T) {
	f := newRenameFixture(t)
	if _, err := f.d.ResumeOntologyJob(f.ctx, "some-job"); !errors.Is(err, connector.ErrOntologyJobNotFound) {
		t.Errorf("resume with no job = %v, want job_not_found", err)
	}
	stoppedRename(t, f)
	job, _ := f.d.unfinishedOntologyJob(f.ctx, f.key)
	before := f.state()
	if _, err := f.d.ResumeOntologyJob(f.ctx, "another-job"); !errors.Is(err, connector.ErrOntologyJobNotFound) {
		t.Errorf("resume of another id = %v, want job_not_found", err)
	}
	lease, err := f.d.acquireDocumentLease(f.ctx, f.key, ontologyRenameLeaseName, "runtime:other", time.Now(), time.Minute)
	if err != nil || !lease.Acquired {
		t.Fatalf("take the lease: %+v, %v", lease, err)
	}
	if _, err := f.d.ResumeOntologyJob(f.ctx, job.ID); !errors.Is(err, connector.ErrOntologyJobBusy) {
		t.Errorf("resume while the lease is held = %v, want job_busy", err)
	}
	if got := f.state(); !reflect.DeepEqual(got, before) {
		t.Errorf("a refused resume moved rows:\n got %v\nwant %v", got, before)
	}
	if _, err := f.d.releaseDocumentLease(f.ctx, f.key, ontologyRenameLeaseName, "runtime:other"); err != nil {
		t.Fatal(err)
	}
	if st, err := f.d.ResumeOntologyJob(f.ctx, job.ID); err != nil || st.State != jobStateDone {
		t.Errorf("resume once the lease is free = %+v, %v; want done", st, err)
	}
}

// TestOntologyJob_ResumeIsRefusedWithoutAnID: a resume names the job it carries on, so
// one aimed at a job that has since finished cannot carry on a newer one. Without an id
// it is invalid_request, before the lease is asked for: it is refused while another call
// holds the lease too, and it moves nothing.
func TestOntologyJob_ResumeIsRefusedWithoutAnID(t *testing.T) {
	f := newRenameFixture(t)
	stoppedRename(t, f)
	before := f.state()
	if st, err := f.d.ResumeOntologyJob(f.ctx, ""); connector.OntologyJobCode(err) != "invalid_request" {
		t.Errorf("resume without an id = %+v, %v (code %q); want invalid_request", st, err, connector.OntologyJobCode(err))
	}
	lease, err := f.d.acquireDocumentLease(f.ctx, f.key, ontologyRenameLeaseName, "runtime:other", time.Now(), time.Minute)
	if err != nil || !lease.Acquired {
		t.Fatalf("take the lease: %+v, %v", lease, err)
	}
	if _, err := f.d.ResumeOntologyJob(f.ctx, ""); connector.OntologyJobCode(err) != "invalid_request" {
		t.Errorf("resume without an id while the lease is held = %v, want invalid_request", err)
	}
	if got := f.state(); !reflect.DeepEqual(got, before) {
		t.Errorf("a resume without an id moved rows:\n got %v\nwant %v", got, before)
	}
	if j, _ := f.d.unfinishedOntologyJob(f.ctx, f.key); j == nil || j.State != jobStateRunning {
		t.Errorf("after a resume without an id the job = %+v, want still running", j)
	}
}
