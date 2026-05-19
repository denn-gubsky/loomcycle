// Package grpc — hook management RPCs.
//
// Three unary RPCs (RegisterHook / ListHooks / DeleteHook) that dispatch
// through connector.Connector. The hook management surface is in-memory
// only — the same operation set the HTTP /v1/hooks routes expose, just
// translated to proto. Callback delivery itself stays HTTP-only (the
// consumer's webhook receiver runs whatever framework it likes).

package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// RegisterHook adds a webhook registration. Returns the assigned id.
// Re-registering the same (owner, name) replaces the prior entry with
// a fresh id (idempotent app-restart contract — see hooks.Registry).
//
// Typed-error mapping (mirrors HTTP /v1/hooks):
//
//	connector.ErrHookInvalidRegistration → codes.InvalidArgument
//	connector.ErrHookNotConfigured       → codes.Unavailable
//	(anything else)                      → codes.Internal
func (s *Server) RegisterHook(ctx context.Context, req *loomcyclepb.RegisterHookRequest) (*loomcyclepb.RegisterHookResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	res, err := s.connector.RegisterHook(ctx, connector.RegisterHookRequest{
		Owner:       req.GetOwner(),
		Name:        req.GetName(),
		Phase:       hooks.Phase(req.GetPhase()),
		Agents:      req.GetAgents(),
		Tools:       req.GetTools(),
		CallbackURL: req.GetCallbackUrl(),
		FailMode:    hooks.FailMode(req.GetFailMode()),
		TimeoutMs:   int(req.GetTimeoutMs()),
	})
	if err != nil {
		return nil, translateHookError(err, "register_hook")
	}
	return &loomcyclepb.RegisterHookResponse{Id: res.ID}, nil
}

// ListHooks returns every registered hook in registration order.
func (s *Server) ListHooks(ctx context.Context, _ *loomcyclepb.ListHooksRequest) (*loomcyclepb.ListHooksResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	res, err := s.connector.ListHooks(ctx)
	if err != nil {
		return nil, translateHookError(err, "list_hooks")
	}
	out := make([]*loomcyclepb.Hook, 0, len(res.Hooks))
	for _, h := range res.Hooks {
		out = append(out, hookToProto(h))
	}
	return &loomcyclepb.ListHooksResponse{Hooks: out}, nil
}

// DeleteHook removes the hook with the given id. Returns NotFound when
// no such hook exists. Idempotent only insofar as the HTTP layer was:
// a second DELETE on the same id always returns NotFound.
func (s *Server) DeleteHook(ctx context.Context, req *loomcyclepb.DeleteHookRequest) (*loomcyclepb.DeleteHookResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	id := req.GetId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	if err := s.connector.DeleteHook(ctx, id); err != nil {
		return nil, translateHookError(err, "delete_hook")
	}
	return &loomcyclepb.DeleteHookResponse{Deleted: id}, nil
}

// translateHookError maps connector typed errors to gRPC status codes.
// Centralised so all three handlers stay short — and so the mapping is
// the one place to look when adding a new typed error.
func translateHookError(err error, op string) error {
	switch {
	case errors.Is(err, connector.ErrHookInvalidRegistration):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, connector.ErrHookNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrHookNotConfigured):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// hookToProto converts an in-memory hooks.Hook to its wire shape.
// The internal Timeout field is not surfaced (callers track timeout_ms,
// which is what they passed in).
func hookToProto(h *hooks.Hook) *loomcyclepb.Hook {
	return &loomcyclepb.Hook{
		Id:           h.ID,
		Owner:        h.Owner,
		Name:         h.Name,
		Phase:        string(h.Phase),
		Agents:       h.Agents,
		Tools:        h.Tools,
		CallbackUrl:  h.CallbackURL,
		FailMode:     string(h.FailMode),
		TimeoutMs:    int32(h.TimeoutMs),
		RegisteredAt: timestamppb.New(h.RegisteredAt),
	}
}
