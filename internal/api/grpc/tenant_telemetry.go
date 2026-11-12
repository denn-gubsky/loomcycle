// tenant_telemetry.go — the gRPC twin of /v1/_telemetry: a tenant's own trace
// export. One op-based RPC (get|set|delete|test) over the connector, which
// holds who may, the policy rules and the audit, so this transport cannot
// differ from HTTP or MCP in any of them.
package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// TenantTelemetry reads, sets, deletes or tests a tenant's trace export.
func (s *Server) TenantTelemetry(ctx context.Context, req *loomcyclepb.TenantTelemetryRequest) (*loomcyclepb.TenantTelemetryResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "TenantTelemetry requires a connector; this Server was constructed without one")
	}
	in := connector.TenantTelemetryRequest{Op: req.GetOp(), Tenant: req.GetTenant()}
	if p := req.GetPolicy(); p != nil {
		in.Policy = &connector.TenantTelemetryPolicyInput{
			Endpoint: p.GetEndpoint(), Protocol: p.GetProtocol(), HeadersCredential: p.GetHeadersCredential(),
			Signals: p.GetSignals(), SamplerRatio: p.SamplerRatio, ContentCapture: p.GetContentCapture(),
		}
		if p.ContentMaxChars != nil {
			n := int(p.GetContentMaxChars())
			in.Policy.ContentMaxChars = &n
		}
	}
	res, err := s.connector.TenantTelemetry(ctx, in)
	if err != nil {
		return nil, tenantTelemetryErrToStatus(err)
	}
	out := &loomcyclepb.TenantTelemetryResponse{Tenant: res.Tenant, Deleted: res.Deleted}
	if p := res.Policy; p != nil {
		out.Policy = &loomcyclepb.TenantTelemetryPolicy{
			Endpoint: p.Endpoint, Protocol: p.Protocol, HeadersCredential: p.HeadersCredential,
			Signals: p.Signals, SamplerRatio: p.SamplerRatio, UpdatedAt: p.UpdatedAt, UpdatedBy: p.UpdatedBy,
			ContentCapture: p.ContentCapture, ContentMaxChars: int32(p.ContentMaxChars),
		}
	}
	if st := res.Status; st != nil {
		out.Status = &loomcyclepb.TenantTelemetryStatus{
			Replica: st.Replica, Exporting: st.Exporting, ExportedSpans: st.ExportedSpans, DroppedSpans: st.DroppedSpans,
			ExportFailures: st.ExportFailures, LastSuccessAt: st.LastSuccessAt, LastError: st.LastError, LastErrorAt: st.LastErrorAt,
		}
	}
	if tr := res.Test; tr != nil {
		out.Test = &loomcyclepb.TenantTelemetryTest{Ok: tr.OK, Error: tr.Error, DurationMs: tr.DurationMS}
	}
	return out, nil
}

func tenantTelemetryErrToStatus(err error) error {
	if st, ok := aclRefusalStatus(err); ok {
		return st
	}
	switch {
	case errors.Is(err, connector.ErrTenantTelemetryDisabled):
		return status.Error(codes.Unimplemented, "feature_disabled: "+err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryAuditUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryRateLimited):
		return status.Error(codes.ResourceExhausted, err.Error())
	default:
		return status.Error(codes.Internal, "tenant telemetry: the request failed")
	}
}
