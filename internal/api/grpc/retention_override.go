// retention_override.go — the gRPC twin of /v1/_retention/overrides: the timed
// override of an agent's no-trace mode. One op-based RPC (list|set|clear)
// over the connector, which holds the rules and the audit, so this transport
// cannot differ from HTTP or MCP in either.
package grpc

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// RetentionOverride lists, sets or clears a no-trace override.
func (s *Server) RetentionOverride(ctx context.Context, req *loomcyclepb.RetentionOverrideRequest) (*loomcyclepb.RetentionOverrideResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "RetentionOverride requires a connector; this Server was constructed without one")
	}
	in := connector.RetentionOverrideRequest{Op: req.GetOp(), Tenant: req.GetTenant(), Agent: req.GetAgent()}
	if u := req.GetUntil(); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "until must be an RFC 3339 time: %v", err)
		}
		in.Until = t
	}
	res, err := s.connector.RetentionOverride(ctx, in)
	if err != nil {
		return nil, retentionOverrideErrToStatus(err)
	}
	out := &loomcyclepb.RetentionOverrideResponse{Cleared: res.Cleared}
	for _, o := range res.Overrides {
		out.Overrides = append(out.Overrides, &loomcyclepb.RetentionOverrideEntry{
			Tenant: o.Tenant, Agent: o.Agent, Until: o.Until, SetBy: o.SetBy, CreatedAt: o.CreatedAt,
		})
	}
	return out, nil
}

func retentionOverrideErrToStatus(err error) error {
	switch {
	case errors.Is(err, connector.ErrRetentionOverrideInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, connector.ErrRetentionOverrideForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, connector.ErrRetentionOverrideAuditUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, connector.ErrRetentionOverrideStoreUnavailable):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "retention override: %v", err)
	}
}
