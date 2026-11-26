// ontology_rollback.go — the gRPC twin of POST /v1/_ontology/rollback (RFC GQ
// P5.7, its transports in P5.6). The connector holds who may roll back, in which
// tenant, and the rollback itself, so this transport only maps the shapes and the
// refusals.
package grpc

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// ApplyOntologyRollback applies the plan of rolling the tenant ontology back to a
// release.
func (s *Server) ApplyOntologyRollback(ctx context.Context, req *loomcyclepb.ApplyOntologyRollbackRequest) (*loomcyclepb.ApplyOntologyRollbackResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ApplyOntologyRollback requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.ApplyOntologyRollback(ctx, connector.ApplyOntologyRollbackRequest{
		Tenant: req.GetTenant(), Seq: req.GetSeq(), PlanFingerprint: req.GetPlanFingerprint(),
	})
	if err != nil {
		return nil, ontologyRollbackErrToStatus(err)
	}
	return &loomcyclepb.ApplyOntologyRollbackResponse{Tenant: res.Tenant, Rollback: rollbackReportToProto(res.Rollback)}, nil
}

func rollbackReportToProto(r connector.OntologyRollbackReport) *loomcyclepb.OntologyRollbackReport {
	out := &loomcyclepb.OntologyRollbackReport{
		Release: r.Release, ReleaseContentVersion: r.ReleaseContentVersion, ContentVersion: r.ContentVersion,
		Matches: r.Matches, Risk: r.Risk, RollbackRelease: r.RollbackRelease, Notes: r.Notes, Warnings: r.Warnings,
	}
	for _, a := range r.Applied {
		out.Applied = append(out.Applied, rollbackStepToProto(a))
	}
	for i := range r.RenameJobs {
		out.RenameJobs = append(out.RenameJobs, ontologyJobToProto(&r.RenameJobs[i]))
	}
	return out
}

func rollbackStepToProto(a connector.OntologyRollbackStep) *loomcyclepb.OntologyRollbackStep {
	return &loomcyclepb.OntologyRollbackStep{Op: a.Op, Entry: a.Entry, Name: a.Name, NewId: a.NewID}
}

// ontologyRollbackErrToStatus maps a rollback refusal onto the gRPC code matching
// HTTP's status, with HTTP's code as the ErrorInfo reason. What HTTP's 409 body
// carries beside the code goes in the ErrorInfo's metadata: `fingerprint` (the
// ontology's current one) on plan_required and plan_stale, and `applied` (the writes
// made, as HTTP's JSON array) on partially_applied. The plan's own refusals map as
// PlanOntology's do; an internal failure's text is not returned.
func ontologyRollbackErrToStatus(err error) error {
	var ref *connector.OntologyRollbackRefusal
	if !errors.As(err, &ref) {
		if connector.OntologyPlanCode(err) != "" {
			return planOntologyErrToStatus(err)
		}
		return statusWithReason(codes.Internal, "ontology_write_failed", "the rollback could not be applied; see the server log")
	}
	code := codes.FailedPrecondition
	meta := map[string]string{}
	switch ref.Code {
	case connector.RollbackPartiallyApplied:
		code = codes.Aborted
		b, _ := json.Marshal(ref.Applied)
		meta["applied"] = string(b)
	case connector.RollbackInvalid:
		code = codes.InvalidArgument
	}
	if ref.Fingerprint != "" {
		meta["fingerprint"] = ref.Fingerprint
	}
	st := status.New(code, ref.Message)
	info := &errdetails.ErrorInfo{Reason: ref.Code, Domain: errorDomain}
	if len(meta) > 0 {
		info.Metadata = meta
	}
	if withDetails, derr := st.WithDetails(info); derr == nil {
		return withDetails.Err()
	}
	return st.Err()
}
