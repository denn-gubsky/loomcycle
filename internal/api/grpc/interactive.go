package grpc

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// interactive.go — RFC AI. The gRPC twins of the interactive-session HTTP
// endpoints. Both dispatch through the Connector (SteerRun / StreamRunEvents),
// which the HTTP *Server implements over the SAME in-process steer registry +
// store the HTTP handlers use — so a gRPC steer reaches an HTTP-started run and
// vice versa, and the tenant-ownership gate is enforced once, server-side.

// RunInput pushes an operator steering message into a live interactive run.
// Mirrors POST /v1/runs/{run_id}/input. The `source` is server-stamped (gRPC
// has no cookie → the API class), never trusted from the wire. Error mapping
// mirrors the HTTP codes: NotFound (no in-flight run / cross-tenant),
// ResourceExhausted (queue full — HTTP 429), Unavailable (steering off).
func (s *Server) RunInput(ctx context.Context, req *loomcyclepb.RunInputRequest) (*loomcyclepb.RunInputResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if runID == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	delivered, err := s.connector.SteerRun(ctx, runID, req.GetText(), store.InterruptResolvedByAPI)
	switch {
	case errors.Is(err, connector.ErrRunNotInFlight):
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSteerQueueFull):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, connector.ErrSteeringUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &loomcyclepb.RunInputResponse{RunId: runID, Delivered: delivered}, nil
}

// StreamRun re-attaches to a run's event stream by run_id, replaying from
// from_seq then live-tailing. Mirrors GET /v1/runs/{run_id}/stream. The
// operator's own turns are replayed too (connector.StreamRunEvents / RFC AI S1),
// so a cold gRPC client reconstructs the whole conversation. Tenant-ownership is
// gated inside the connector (cross-tenant → NotFound, opaque). As-built note:
// unlike the HTTP endpoint, this does NOT emit a leading side-channel `agent`
// metadata frame — the caller already holds run_id; if it needs the run's
// agent_id (e.g. for CancelAgent) it calls GetAgent. (Deviation from RFC AI
// fork D, taken to keep the run metadata fetch tenant-gated through the single
// connector entry rather than a raw store read in the handler.)
func (s *Server) StreamRun(req *loomcyclepb.StreamRunRequest, stream loomcyclepb.Loomcycle_StreamRunServer) error {
	if s.connector == nil {
		return status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if runID == "" {
		return status.Error(codes.InvalidArgument, "run_id is required")
	}
	visit := func(ev providers.Event) error {
		return stream.Send(eventToProto(ev))
	}
	err := s.connector.StreamRunEvents(stream.Context(), runID, req.GetFromSeq(), visit)
	switch {
	case errors.Is(err, connector.ErrRunNotInFlight):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSteeringUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}
