package grpc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// RFC GQ P5.6 — the gRPC twins of GET /v1/_ontology/job and POST
// /v1/_ontology/job/resume.

// Who may call them: an admin and a tenant operator, as on HTTP. The job rewrites
// stored rows in every scope of the tenant, so a member, an isolated user and a
// token held to the run plane are refused at the scope gate.
func TestOntologyJobRPCs_AreOperatorOnly(t *testing.T) {
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	for _, rpc := range []string{"GetOntologyJob", "ResumeOntologyJob"} {
		method := grpcMethodPrefix + rpc
		if got := requiredScopeForRPC(method); got != auth.ScopeTenant {
			t.Errorf("%s: required scope = %q, want %q", rpc, got, auth.ScopeTenant)
		}
		for what, tc := range map[string]struct {
			ctx  context.Context
			want bool
		}{
			"an admin":           {as([]string{auth.ScopeAdmin}, auth.Restrictions{}), true},
			"a tenant operator":  {as([]string{auth.ScopeTenant}, auth.Restrictions{}), true},
			"a member":           {as([]string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, auth.Restrictions{}), false},
			"an isolated user":   {as([]string{auth.ScopeUser}, auth.Restrictions{}), false},
			"a restricted token": {as([]string{auth.ScopeTenant}, auth.Restrictions{Agents: []string{"judge"}}), false},
		} {
			if err := enforceScope(tc.ctx, method); (err == nil) != tc.want {
				t.Errorf("%s, %s: enforceScope = %v, want allowed=%v", rpc, what, err, tc.want)
			}
		}
	}
}

// jobFromProto reads a job back into the shared type, field by field, so a field the
// mapping dropped shows as a difference.
func jobFromProto(j *loomcyclepb.OntologyJobStatus) *connector.OntologyJobStatus {
	if j == nil {
		return nil
	}
	done := map[string]int{}
	for k, v := range j.GetDone() {
		done[k] = int(v)
	}
	return &connector.OntologyJobStatus{ID: j.GetId(), Kind: j.GetKind(), Entry: j.GetEntry(), From: j.GetFrom(), To: j.GetTo(),
		State: j.GetState(), Step: j.GetStep(), Done: done, Error: j.GetError(), Remain: int(j.GetRemaining()),
		Scope: j.GetScope(), ScopesDone: int(j.GetScopesDone()), ToTags: j.GetToTags(), ScopesTotal: int(j.GetScopesTotal()),
		CreatedAt: j.GetCreatedAt(), UpdatedAt: j.GetUpdatedAt()}
}

// The RPCs hand the tenant (and the resume's id) to the connector and render every
// field of the job; no job stays absent; a refusal carries HTTP's code as its
// ErrorInfo reason under the gRPC code matching HTTP's status, and an internal failure
// hides its text.
func TestOntologyJobRPCs_DelegateToTheConnector(t *testing.T) {
	job := &connector.OntologyJobStatus{ID: "j1", Kind: "concept_retag", Entry: "c1", From: []string{"topics/risk"},
		To: "topics/reporting/risk", ToTags: []string{"topics/reporting/risk"}, State: "blocked", Step: "document_tags",
		Done: map[string]int{"chunk_tags": 4}, Error: "a user scope: document_tags: a row could not be rewritten", Remain: 7,
		Scope: "user", ScopesDone: 1, ScopesTotal: 3, CreatedAt: 1764000000123456789, UpdatedAt: 1764000000123456790}
	mc := &mockConnector{jobRes: connector.OntologyJobResult{Tenant: "acme", Job: job}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	got, err := client.GetOntologyJob(context.Background(), &loomcyclepb.GetOntologyJobRequest{Tenant: "globex"})
	if err != nil {
		t.Fatalf("GetOntologyJob: %v", err)
	}
	if mc.jobGetReq != (connector.GetOntologyJobRequest{Tenant: "globex"}) || got.GetTenant() != "acme" ||
		!reflect.DeepEqual(jobFromProto(got.GetJob()), job) {
		t.Errorf("GetOntologyJob = %v, connector asked %+v; want every field of %+v", got, mc.jobGetReq, job)
	}
	got, err = client.ResumeOntologyJob(context.Background(), &loomcyclepb.ResumeOntologyJobRequest{Tenant: "globex", Id: "j1"})
	if err != nil {
		t.Fatalf("ResumeOntologyJob: %v", err)
	}
	if mc.jobResumeReq != (connector.ResumeOntologyJobRequest{Tenant: "globex", ID: "j1"}) || !reflect.DeepEqual(jobFromProto(got.GetJob()), job) {
		t.Errorf("ResumeOntologyJob = %v, connector asked %+v", got, mc.jobResumeReq)
	}
	mc.jobRes = connector.OntologyJobResult{Tenant: "acme"}
	if got, err := client.GetOntologyJob(context.Background(), &loomcyclepb.GetOntologyJobRequest{}); err != nil || got.GetJob() != nil {
		t.Errorf("GetOntologyJob with no job = %v, %v; want job absent", got, err)
	}

	for _, tc := range []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{connector.WithMessage(connector.ErrOntologyJobForbidden, "for an operator"), codes.PermissionDenied, "forbidden"},
		{connector.WithMessage(connector.ErrOntologyJobNotFound, "no unfinished job"), codes.NotFound, "job_not_found"},
		{connector.WithMessage(connector.ErrOntologyJobBusy, "a rename is running"), codes.FailedPrecondition, "job_busy"},
		{connector.WithMessage(connector.ErrOntologyJobUnavailable, "no SQL Memory"), codes.Unavailable, "unavailable"},
		{errors.New("open /var/lib/secret.db: denied"), codes.Internal, "job_failed"},
	} {
		mc.jobErr = tc.err
		_, gerr := client.GetOntologyJob(context.Background(), &loomcyclepb.GetOntologyJobRequest{})
		_, rerr := client.ResumeOntologyJob(context.Background(), &loomcyclepb.ResumeOntologyJobRequest{Id: "j1"})
		for _, err := range []error{gerr, rerr} {
			code, reason, msg := reasonOf(err)
			if code != tc.code || reason != tc.reason {
				t.Errorf("%v: %s %q, want %s %q", tc.err, code, reason, tc.code, tc.reason)
			}
			if strings.Contains(msg, "/var/lib") {
				t.Errorf("an internal failure's text reached the caller: %s", msg)
			}
		}
	}
}

// PlanOntology renders the job block of a planned change — a class rename's steps,
// collisions and the unfinished job it waits for, a concept re-tag's kind and tag
// moves — and keeps it absent on a change with none.
func TestPlanOntology_RenameJobBlockRoundTrips(t *testing.T) {
	plan := &connector.OntologyRenameJobPlan{
		Steps:    []connector.OntologyRenameJobStep{{Name: "documents_type", TenantScope: 2, OtherScopes: 5}},
		Runnable: false, Blocked: []string{"the plan could not count every scope"},
		Collisions:      []connector.OntologyRenameCollision{{Column: "identity_key", Key: "bulletin:a", Into: "circular:a", Holder: "c9"}},
		CollisionsTotal: 3, CollisionsOtherScopes: 2, ScopesNotCounted: 1,
		Running: &connector.OntologyJobStatus{ID: "j0", Kind: "class_rename", From: []string{"Memo"}, To: "Note", State: "running",
			Step: "chunks_type", Done: map[string]int{"documents_type": 1}},
		Notes: []string{"definitions outside the ontology are not rewritten"}, Kind: "class_rename",
	}
	retag := &connector.OntologyRenameJobPlan{
		Kind:     "concept_retag",
		TagMoves: []connector.OntologyTagMove{{From: "topics/risk", To: "topics/reporting/risk"}, {From: "topics/a", To: "topics/b"}},
		Steps:    []connector.OntologyRenameJobStep{{Name: "chunk_tags", TenantScope: 1, OtherScopes: 2}, {Name: "classification_tags"}},
		Runnable: true, ScopesNotCounted: 1, Notes: []string{"the job rewrites the tenant scope, then every user and agent scope"},
	}
	mc := &mockConnector{planRes: connector.PlanOntologyResult{Tenant: "acme", Changes: []connector.OntologyPlannedChange{
		{Op: "rename", Entry: connector.OntologyPlanEntry{ChunkID: "c1"}, Job: plan},
		{Op: "retire", Entry: connector.OntologyPlanEntry{ChunkID: "c2"}},
		{Op: "move", Entry: connector.OntologyPlanEntry{ChunkID: "c3"}, Job: retag},
	}}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	res, err := client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{Changes: []*loomcyclepb.OntologyChange{{Op: "rename", Entry: "c1", Name: "x"}}})
	if err != nil {
		t.Fatalf("PlanOntology: %v", err)
	}
	read := func(j *loomcyclepb.OntologyRenameJobPlan) *connector.OntologyRenameJobPlan {
		got := &connector.OntologyRenameJobPlan{Runnable: j.GetRunnable(), Blocked: j.GetBlocked(), CollisionsTotal: int(j.GetCollisionsTotal()),
			CollisionsOtherScopes: int(j.GetCollisionsOtherScopes()), ScopesNotCounted: int(j.GetScopesNotCounted()),
			Running: jobFromProto(j.GetRunning()), Notes: j.GetNotes(), Kind: j.GetKind()}
		for _, s := range j.GetSteps() {
			got.Steps = append(got.Steps, connector.OntologyRenameJobStep{Name: s.GetName(), TenantScope: int(s.GetTenantScope()), OtherScopes: int(s.GetOtherScopes())})
		}
		for _, c := range j.GetCollisions() {
			got.Collisions = append(got.Collisions, connector.OntologyRenameCollision{Column: c.GetColumn(), Key: c.GetKey(), Into: c.GetInto(), Holder: c.GetHolder()})
		}
		for _, m := range j.GetTagMoves() {
			got.TagMoves = append(got.TagMoves, connector.OntologyTagMove{From: m.GetFrom(), To: m.GetTo()})
		}
		return got
	}
	// The running job's unset fields read back as their zero values.
	plan.Running.ToTags = nil
	if got := read(res.GetChanges()[0].GetJob()); !reflect.DeepEqual(got, plan) {
		t.Errorf("job block = %+v\nwant %+v", got, plan)
	}
	if res.GetChanges()[1].GetJob() != nil {
		t.Errorf("a change with no job carries %v", res.GetChanges()[1].GetJob())
	}
	if got := read(res.GetChanges()[2].GetJob()); !reflect.DeepEqual(got, retag) {
		t.Errorf("re-tag job block = %+v\nwant %+v", got, retag)
	}
}
