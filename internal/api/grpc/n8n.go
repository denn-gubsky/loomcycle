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

	cReq := connector.StreamUserRunStatesRequest{
		UserID:   req.GetUserId(),
		Statuses: req.GetStatuses(),
		Agent:    req.GetAgent(),
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
