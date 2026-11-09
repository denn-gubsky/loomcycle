package grpc

// app_signins.go — the gRPC twins of GET/DELETE /v1/_users/{subject}/app-sign-ins
// (the tenant operator's) and GET/DELETE /v1/_me/app-sign-ins (the caller's
// own). Both pairs call the connector the HTTP routes call, which asks the
// operator route's access-control question itself, so the answer is the same
// on every transport.

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ListUserAppSignIns is the twin of GET /v1/_users/{subject}/app-sign-ins.
func (s *Server) ListUserAppSignIns(ctx context.Context, req *loomcyclepb.ListUserAppSignInsRequest) (*loomcyclepb.AppSignInsResponse, error) {
	tenant, err := s.appSignInUser(ctx, req.GetSubject())
	if err != nil {
		return nil, err
	}
	return s.listAppSignIns(ctx, tenant, req.GetSubject())
}

// EndUserAppSignIn is the twin of DELETE /v1/_users/{subject}/app-sign-ins/{family_id}.
func (s *Server) EndUserAppSignIn(ctx context.Context, req *loomcyclepb.EndUserAppSignInRequest) (*loomcyclepb.EndAppSignInResponse, error) {
	tenant, err := s.appSignInUser(ctx, req.GetSubject())
	if err != nil {
		return nil, err
	}
	return s.endAppSignIn(ctx, tenant, req.GetSubject(), req.GetFamilyId())
}

// ListMyAppSignIns is the twin of GET /v1/_me/app-sign-ins.
func (s *Server) ListMyAppSignIns(ctx context.Context, _ *loomcyclepb.ListMyAppSignInsRequest) (*loomcyclepb.AppSignInsResponse, error) {
	p, err := s.appSignInSelf(ctx)
	if err != nil {
		return nil, err
	}
	return s.listAppSignIns(ctx, p.TenantID, p.Subject)
}

// EndMyAppSignIn is the twin of DELETE /v1/_me/app-sign-ins/{family_id}.
func (s *Server) EndMyAppSignIn(ctx context.Context, req *loomcyclepb.EndMyAppSignInRequest) (*loomcyclepb.EndAppSignInResponse, error) {
	p, err := s.appSignInSelf(ctx)
	if err != nil {
		return nil, err
	}
	return s.endAppSignIn(ctx, p.TenantID, p.Subject, req.GetFamilyId())
}

// appSignInUser resolves the operator twins' tenant — the caller's own, as
// DirectoryInspect — and refuses a user that does not exist in it, as the
// HTTP route does.
func (s *Server) appSignInUser(ctx context.Context, subject string) (string, error) {
	if s.connector == nil || s.store == nil {
		return "", status.Error(codes.Unimplemented, "app sign-ins require a connector and a store")
	}
	if subject == "" {
		return "", status.Error(codes.InvalidArgument, "subject is required")
	}
	tenant := erasureTenant(ctx)
	if _, err := s.store.UserGet(ctx, tenant, subject); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return "", status.Error(codes.NotFound, "no such user in this tenant")
		}
		return "", status.Error(codes.Internal, err.Error())
	}
	return tenant, nil
}

// appSignInSelf returns the caller's own principal: a credential that names
// no subject (open mode, the legacy token) has no sign-ins of its own.
func (s *Server) appSignInSelf(ctx context.Context) (auth.Principal, error) {
	if s.connector == nil {
		return auth.Principal{}, status.Error(codes.Unimplemented, "app sign-ins require a connector")
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Subject == "" {
		return auth.Principal{}, status.Error(codes.PermissionDenied, "app sign-ins belong to a signed-in person; this credential names none")
	}
	return p, nil
}

func (s *Server) listAppSignIns(ctx context.Context, tenant, subject string) (*loomcyclepb.AppSignInsResponse, error) {
	list, err := s.connector.AppSignIns(ctx, tenant, subject)
	if err != nil {
		return nil, appSignInErrToStatus(err)
	}
	out := &loomcyclepb.AppSignInsResponse{Subject: subject}
	for _, a := range list {
		out.AppSignIns = append(out.AppSignIns, appSignInToProto(a))
	}
	return out, nil
}

func (s *Server) endAppSignIn(ctx context.Context, tenant, subject, familyID string) (*loomcyclepb.EndAppSignInResponse, error) {
	if familyID == "" {
		return nil, status.Error(codes.InvalidArgument, "family_id is required")
	}
	if err := s.connector.EndAppSignIn(ctx, tenant, subject, familyID); err != nil {
		return nil, appSignInErrToStatus(err)
	}
	return &loomcyclepb.EndAppSignInResponse{}, nil
}

func appSignInToProto(a connector.AppSignIn) *loomcyclepb.AppSignIn {
	return &loomcyclepb.AppSignIn{FamilyId: a.FamilyID, ClientId: a.ClientID, ClientName: a.ClientName, Gate: a.Gate,
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339), LastUsedAt: a.LastUsedAt.UTC().Format(time.RFC3339),
		ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339)}
}

// appSignInErrToStatus maps the connector's answers: an access-control
// refusal (which reports itself as a not-allowed run, the shape every
// ACL refusal of the shared operations takes) is PermissionDenied, a family
// that is not the user's live sign-in NotFound.
func appSignInErrToStatus(err error) error {
	var nf *store.ErrNotFound
	switch {
	case errors.Is(err, runner.ErrAgentNotAllowed):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.As(err, &nf):
		return status.Error(codes.NotFound, "no such app sign-in for this user")
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
