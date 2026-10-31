// merge_subjects.go — the gRPC twin of POST /v1/_ontology/subjects/merge
// (RFC FR P2). The connector holds the rules — who may merge, in which tenant,
// and the merge itself — so this transport only maps the shapes and the
// refusals.
package grpc

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// MergeSubjects folds one ontology subject into another.
func (s *Server) MergeSubjects(ctx context.Context, req *loomcyclepb.MergeSubjectsRequest) (*loomcyclepb.MergeSubjectsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "MergeSubjects requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.MergeSubjects(ctx, connector.MergeSubjectsRequest{
		Tenant: req.GetTenant(), Duplicate: req.GetDuplicate(), Survivor: req.GetSurvivor(),
	})
	if err != nil {
		return nil, mergeSubjectsErrToStatus(err)
	}
	ref := func(r connector.SubjectRef) *loomcyclepb.SubjectRef {
		return &loomcyclepb.SubjectRef{DocumentId: r.DocumentID, RootChunkId: r.RootChunkID, NaturalKey: r.NaturalKey}
	}
	return &loomcyclepb.MergeSubjectsResponse{
		Tenant:              res.Tenant,
		Duplicate:           ref(res.Duplicate),
		Survivor:            ref(res.Survivor),
		FactsMoved:          int32(res.FactsMoved),
		StatementsRepointed: int32(res.StatementsRepointed),
		StatementsRetired:   int32(res.StatementsRetired),
		StatementsLeft:      int32(res.StatementsLeft),
		NotAnnounced:        res.NotAnnounced,
	}, nil
}

// mergeSubjectsErrToStatus maps a merge refusal onto the gRPC code matching
// HTTP's status, with the shared code as the ErrorInfo reason so a client
// branches on the reason rather than on the sentence.
func mergeSubjectsErrToStatus(err error) error {
	reason := connector.SubjectMergeCode(err)
	code := codes.Internal
	switch {
	case errors.Is(err, connector.ErrSubjectMergeInvalid), errors.Is(err, connector.ErrSubjectMergeSame),
		errors.Is(err, connector.ErrSubjectMergeNotSubject):
		code = codes.InvalidArgument
	case errors.Is(err, connector.ErrSubjectMergeForbidden):
		code = codes.PermissionDenied
	case errors.Is(err, connector.ErrSubjectMergeNotInScope):
		code = codes.NotFound
	case errors.Is(err, connector.ErrSubjectMergeAlreadyMerged), errors.Is(err, connector.ErrSubjectMergeConflict),
		errors.Is(err, connector.ErrSubjectMergePrincipalConflict):
		code = codes.FailedPrecondition
	case errors.Is(err, connector.ErrSubjectMergeUnavailable):
		code = codes.Unavailable
	default:
		reason = "merge_failed"
	}
	st := status.New(code, err.Error())
	if withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain}); derr == nil {
		return withDetails.Err()
	}
	return st.Err()
}
