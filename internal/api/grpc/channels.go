// channels.go — gRPC handlers for the v0.9.x Channel CRUD ops
// (publish / subscribe / peek / ack). Thin RPC wrappers around the
// Connector methods at internal/api/http/connector_impl_channels.go;
// every typed connector error maps to a specific gRPC status code so
// language-portable clients (Python, Java, the n8n adapter) can
// branch on `status.Code(err)` instead of parsing strings.
package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// channelErrCode maps the typed Connector errors to gRPC status codes.
// One place; every Channel CRUD RPC funnels through it so the wire
// mapping stays consistent. Mirrors writeChannelError() at the HTTP
// boundary (internal/api/http/channels_admin.go).
func channelErrCode(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, connector.ErrChannelNotDeclared):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrChannelScopeInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, connector.ErrChannelCursorRegression):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, connector.ErrSystemPublisherUnwired):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// PublishChannel — mirrors POST /v1/_channels/{name}/publish (and the
// /v1/users/{user_id}/channels/{name}/publish variant; the URL family
// is HTTP-only — gRPC callers carry the scope distinction in the
// request fields directly).
func (s *Server) PublishChannel(ctx context.Context, req *loomcyclepb.PublishChannelRequest) (*loomcyclepb.PublishChannelResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	resp, err := s.connector.PublishChannel(ctx, connector.ChannelPublishRequest{
		Channel:   req.GetChannel(),
		Scope:     req.GetScope(),
		ScopeID:   req.GetScopeId(),
		Payload:   req.GetPayload(),
		DeliverAt: req.GetDeliverAt(),
	})
	if err != nil {
		return nil, channelErrCode(err)
	}
	return &loomcyclepb.PublishChannelResponse{
		MsgId:     resp.MsgID,
		Channel:   resp.Channel,
		CreatedAt: resp.CreatedAt,
		VisibleAt: resp.VisibleAt,
	}, nil
}

// SubscribeChannel — mirrors POST /v1/_channels/{name}/subscribe. The
// HTTP route is a single-round-trip long-poll (not SSE); the gRPC RPC
// matches that semantic — unary, not server-streamed. Callers that
// want a continuous stream rebuild it via repeated SubscribeChannel
// calls (the n8n trigger node's pattern).
func (s *Server) SubscribeChannel(ctx context.Context, req *loomcyclepb.SubscribeChannelRequest) (*loomcyclepb.SubscribeChannelResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	resp, err := s.connector.SubscribeChannel(ctx, connector.ChannelSubscribeRequest{
		Channel:     req.GetChannel(),
		Scope:       req.GetScope(),
		ScopeID:     req.GetScopeId(),
		FromCursor:  req.GetFromCursor(),
		MaxMessages: int(req.GetMaxMessages()),
		WaitMS:      int(req.GetWaitMs()),
	})
	if err != nil {
		return nil, channelErrCode(err)
	}
	out := &loomcyclepb.SubscribeChannelResponse{
		Channel:    resp.Channel,
		NextCursor: resp.NextCursor,
		Messages:   make([]*loomcyclepb.ChannelMessage, 0, len(resp.Messages)),
	}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, &loomcyclepb.ChannelMessage{
			Id:          m.ID,
			Value:       m.Value,
			PublishedAt: m.PublishedAt,
		})
	}
	return out, nil
}

// PeekChannel — mirrors GET /v1/_channels/{name}/peek. Non-destructive.
func (s *Server) PeekChannel(ctx context.Context, req *loomcyclepb.PeekChannelRequest) (*loomcyclepb.PeekChannelResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	resp, err := s.connector.PeekChannel(ctx, connector.ChannelPeekRequest{
		Channel:     req.GetChannel(),
		Scope:       req.GetScope(),
		ScopeID:     req.GetScopeId(),
		FromCursor:  req.GetFromCursor(),
		MaxMessages: int(req.GetMaxMessages()),
	})
	if err != nil {
		return nil, channelErrCode(err)
	}
	out := &loomcyclepb.PeekChannelResponse{
		Channel:  resp.Channel,
		Messages: make([]*loomcyclepb.ChannelMessage, 0, len(resp.Messages)),
	}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, &loomcyclepb.ChannelMessage{
			Id:          m.ID,
			Value:       m.Value,
			PublishedAt: m.PublishedAt,
		})
	}
	return out, nil
}

// AckChannel — mirrors POST /v1/_channels/{name}/ack.
func (s *Server) AckChannel(ctx context.Context, req *loomcyclepb.AckChannelRequest) (*loomcyclepb.AckChannelResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	resp, err := s.connector.AckChannel(ctx, connector.ChannelAckRequest{
		Channel: req.GetChannel(),
		Scope:   req.GetScope(),
		ScopeID: req.GetScopeId(),
		Cursor:  req.GetCursor(),
	})
	if err != nil {
		return nil, channelErrCode(err)
	}
	return &loomcyclepb.AckChannelResponse{Ok: resp.OK}, nil
}
