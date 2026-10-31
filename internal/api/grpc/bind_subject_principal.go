// bind_subject_principal.go — the gRPC twin of POST
// /v1/_ontology/subjects/principal (RFC FR P4). The connector holds the rules —
// who may bind, in which tenant, which identities are users, and the binding
// itself — so this transport only maps the shapes and the refusals.
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

// BindSubjectPrincipal binds an ontology subject to a user, or clears it.
func (s *Server) BindSubjectPrincipal(ctx context.Context, req *loomcyclepb.BindSubjectPrincipalRequest) (*loomcyclepb.BindSubjectPrincipalResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "BindSubjectPrincipal requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.BindSubjectPrincipal(ctx, connector.BindSubjectPrincipalRequest{
		Tenant: req.GetTenant(), Subject: req.GetSubject(), Principal: req.GetPrincipal(),
	})
	if err != nil {
		return nil, bindSubjectPrincipalErrToStatus(err)
	}
	return &loomcyclepb.BindSubjectPrincipalResponse{
		Tenant: res.Tenant, DocumentId: res.DocumentID, RootChunkId: res.RootChunkID, NaturalKey: res.NaturalKey,
		Path: res.Path, Principal: res.Principal, Previous: res.Previous, Changed: res.Changed,
	}, nil
}

// bindSubjectPrincipalErrToStatus maps a binding refusal onto the gRPC code
// matching HTTP's status, with the shared code as the ErrorInfo reason.
func bindSubjectPrincipalErrToStatus(err error) error {
	reason := connector.SubjectBindCode(err)
	code := codes.Internal
	switch {
	case errors.Is(err, connector.ErrSubjectBindInvalid), errors.Is(err, connector.ErrSubjectBindNotSubject):
		code = codes.InvalidArgument
	case errors.Is(err, connector.ErrSubjectBindForbidden):
		code = codes.PermissionDenied
	case errors.Is(err, connector.ErrSubjectBindNotFound), errors.Is(err, connector.ErrSubjectBindPrincipalNotFound):
		code = codes.NotFound
	case errors.Is(err, connector.ErrSubjectBindConflict):
		code = codes.FailedPrecondition
	case errors.Is(err, connector.ErrSubjectBindUnavailable):
		code = codes.Unavailable
	default:
		reason = "bind_failed"
	}
	st := status.New(code, err.Error())
	if withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain}); derr == nil {
		return withDetails.Err()
	}
	return st.Err()
}
