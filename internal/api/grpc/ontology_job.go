// ontology_job.go — the gRPC twins of GET /v1/_ontology/job and POST
// /v1/_ontology/job/resume (RFC GQ P5.6), and the job shapes PlanOntology's
// rename block carries. The connector holds who may act and in which tenant,
// and the job itself, so this transport only maps the shapes and the refusals.
package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// GetOntologyJob reads the tenant's ontology job.
func (s *Server) GetOntologyJob(ctx context.Context, req *loomcyclepb.GetOntologyJobRequest) (*loomcyclepb.OntologyJobResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "GetOntologyJob requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.GetOntologyJob(ctx, connector.GetOntologyJobRequest{Tenant: req.GetTenant()})
	if err != nil {
		return nil, ontologyJobErrToStatus(err)
	}
	return &loomcyclepb.OntologyJobResponse{Tenant: res.Tenant, Job: ontologyJobToProto(res.Job)}, nil
}

// ResumeOntologyJob gives the tenant's unfinished ontology job one slice.
func (s *Server) ResumeOntologyJob(ctx context.Context, req *loomcyclepb.ResumeOntologyJobRequest) (*loomcyclepb.OntologyJobResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ResumeOntologyJob requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.ResumeOntologyJob(ctx, connector.ResumeOntologyJobRequest{Tenant: req.GetTenant(), ID: req.GetId()})
	if err != nil {
		return nil, ontologyJobErrToStatus(err)
	}
	return &loomcyclepb.OntologyJobResponse{Tenant: res.Tenant, Job: ontologyJobToProto(res.Job)}, nil
}

// ontologyJobToProto keeps an absent job absent: a tenant that never had one.
func ontologyJobToProto(j *connector.OntologyJobStatus) *loomcyclepb.OntologyJobStatus {
	if j == nil {
		return nil
	}
	out := &loomcyclepb.OntologyJobStatus{
		Id: j.ID, Kind: j.Kind, Entry: j.Entry, From: j.From, To: j.To, State: j.State, Step: j.Step, Error: j.Error,
		Remaining: int32(j.Remain), Scope: j.Scope, ScopesDone: int32(j.ScopesDone), ToTags: j.ToTags,
		ScopesTotal: int32(j.ScopesTotal), CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
	}
	if len(j.Done) > 0 {
		out.Done = make(map[string]int32, len(j.Done))
		for k, v := range j.Done {
			out.Done[k] = int32(v)
		}
	}
	return out
}

// renameJobPlanToProto is a planned change's class-rename block; nil for a change
// with none.
func renameJobPlanToProto(p *connector.OntologyRenameJobPlan) *loomcyclepb.OntologyRenameJobPlan {
	if p == nil {
		return nil
	}
	out := &loomcyclepb.OntologyRenameJobPlan{
		Runnable: p.Runnable, Blocked: p.Blocked, CollisionsTotal: int32(p.CollisionsTotal),
		CollisionsOtherScopes: int32(p.CollisionsOtherScopes), ScopesNotCounted: int32(p.ScopesNotCounted),
		Running: ontologyJobToProto(p.Running), Notes: p.Notes,
	}
	for _, s := range p.Steps {
		out.Steps = append(out.Steps, &loomcyclepb.OntologyRenameJobStep{Name: s.Name, TenantScope: int32(s.TenantScope), OtherScopes: int32(s.OtherScopes)})
	}
	for _, c := range p.Collisions {
		out.Collisions = append(out.Collisions, &loomcyclepb.OntologyRenameCollision{Column: c.Column, Key: c.Key, Into: c.Into, Holder: c.Holder})
	}
	return out
}

// ontologyJobErrToStatus maps a job refusal onto the gRPC code matching HTTP's
// status (400/403/404/409/503), with the shared code as the ErrorInfo reason. HTTP's
// 409 job_busy is FAILED_PRECONDITION, as this API's other 409s are: the state the
// call needs (a free rename lease) is not there yet. A resume without an id is
// INVALID_ARGUMENT invalid_request. An internal failure's text is not returned, as on
// HTTP.
func ontologyJobErrToStatus(err error) error {
	reason := connector.OntologyJobCode(err)
	switch {
	case errors.Is(err, connector.ErrOntologyJobForbidden):
		return statusWithReason(codes.PermissionDenied, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyJobNotFound):
		return statusWithReason(codes.NotFound, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyJobBusy):
		return statusWithReason(codes.FailedPrecondition, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyJobIDRequired):
		return statusWithReason(codes.InvalidArgument, reason, err.Error())
	case errors.Is(err, connector.ErrOntologyJobUnavailable):
		return statusWithReason(codes.Unavailable, reason, err.Error())
	}
	return statusWithReason(codes.Internal, "job_failed", "the ontology job could not be read or resumed; see the server log")
}
