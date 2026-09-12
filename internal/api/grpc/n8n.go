// n8n.go — gRPC handlers for the v0.9.x n8n RFC Phase 0 RPCs.
// ListChannels is sync request/response; StreamUserRunStates is a
// server-streamed RPC backed by the Connector's visitor-pattern.
package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ListChannels — mirrors GET /v1/_channels.
func (s *Server) ListChannels(ctx context.Context, _ *loomcyclepb.ListChannelsRequest) (*loomcyclepb.ListChannelsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	resp, err := s.connector.ListChannels(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &loomcyclepb.ListChannelsResponse{
		Channels: make([]*loomcyclepb.ChannelDescriptor, 0, len(resp.Channels)),
	}
	for _, c := range resp.Channels {
		out.Channels = append(out.Channels, &loomcyclepb.ChannelDescriptor{
			Name:            c.Name,
			Scope:           c.Scope,
			Semantic:        c.Semantic,
			Publisher:       c.Publisher,
			Period:          c.Period,
			DefaultTtl:      int32(c.DefaultTTL),
			MaxMessages:     int32(c.MaxMessages),
			MessageCount:    c.MessageCount,
			OldestVisibleAt: c.OldestVisibleAt,
			NewestVisibleAt: c.NewestVisibleAt,
		})
	}
	return out, nil
}

// StreamUserRunStates — server-streamed RPC that mirrors
// GET /v1/users/{user_id}/agents/stream. Yields one RunStateEvent
// per matching state transition until ctx fires.
func (s *Server) StreamUserRunStates(req *loomcyclepb.StreamUserRunStatesRequest, stream loomcyclepb.Loomcycle_StreamUserRunStatesServer) error {
	if s.connector == nil {
		return status.Error(codes.Unavailable, "connector not wired")
	}
	if req.GetUserId() == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}

	// Tenant isolation (RFC L/N): confine the stream to the caller's tenant,
	// mirroring the HTTP handleStreamUserAgents which sets TenantID/TenantScoped
	// from principalTenantScope. Without this a scoped principal streamed every
	// tenant's live run-state transitions (the connector filter is gated on
	// TenantScoped, which the gRPC path previously left false).
	tenantID, allTenants := grpcTenantScope(stream.Context())
	cReq := connector.StreamUserRunStatesRequest{
		UserID:   req.GetUserId(),
		Statuses: req.GetStatuses(),
		Agent:    req.GetAgent(),
		// Narrow to one team walk's runs. Applied by the connector against
		// each event's parent_context.walk_id, exactly as the HTTP handler's
		// ?walk_id= does — the two transports must not disagree about what a
		// walk filter means.
		WalkID:       req.GetWalkId(),
		TenantID:     tenantID,
		TenantScoped: !allTenants,
	}

	visit := func(evt connector.RunStateEvent) error {
		// Translate Connector event into the proto event. Send error
		// propagates as a non-sentinel error from the visitor; the
		// connector loop will treat it as a real error and unwind.
		return stream.Send(&loomcyclepb.RunStateEvent{
			RunId:         evt.RunID,
			AgentId:       evt.AgentID,
			Agent:         evt.Agent,
			UserId:        evt.UserID,
			ParentAgentId: evt.ParentAgentID,
			Status:        evt.Status,
			StopReason:    evt.StopReason,
			Error:         evt.Error,
			Ts:            evt.TS,
			ParentContext: parentContextToProto(evt.ParentContext),
		})
	}

	err := s.connector.StreamUserRunStates(stream.Context(), cReq, visit)
	if err != nil {
		if errors.Is(err, connector.ErrRunStateStreamUnavailable) {
			return status.Error(codes.Unavailable, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

// parentContextToProto maps the run's lineage onto the wire message, or nil
// when the run carried none.
//
// It exists because the gRPC transport used to drop this entirely: the HTTP
// SSE frame has carried `parent_context` since v0.12.x, and a gRPC subscriber
// could neither attribute a finishing sub-agent to the request that started it
// nor tell which wave of a walk a run belonged to. The walk_id FILTER is
// server-side, so a client can now narrow the stream — but narrowing is only
// half of it; placing a run inside the walk needs the wave fields.
//
// Every field is copied explicitly rather than by reflection so a field added
// to store.ParentContext fails to compile here instead of silently going
// missing on one transport, which is the failure this whole change is fixing.
func parentContextToProto(pc *store.ParentContext) *loomcyclepb.ParentContext {
	if pc == nil {
		return nil
	}
	return &loomcyclepb.ParentContext{
		RootAgentRunId:  pc.RootAgentRunID,
		FunctionKey:     pc.FunctionKey,
		TierAtRun:       pc.TierAtRun,
		BoardScope:      pc.BoardScope,
		BoardChunkId:    pc.BoardChunkID,
		BoardDocumentId: pc.BoardDocumentID,
		WalkId:          pc.WalkID,
		WaveId:          pc.WaveID,
		WaveIndex:       int32(pc.WaveIndex),
	}
}
