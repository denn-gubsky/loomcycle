// Package http — Connector hook-management implementation. Lives in its
// own file (mirroring how grpc/pause_snapshot.go isolates the
// pause/snapshot concern) so the file growing here doesn't bloat the
// main connector_impl.go.

package http

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// RegisterHook adapts the existing hookRegistry.Register flow to the
// Connector interface. The transport-shape connector.RegisterHookRequest
// is mapped to a hooks.Hook struct; the registry validates and assigns
// an ID. hooks.ErrInvalidRegistration is wrapped as
// connector.ErrHookInvalidRegistration so gRPC/MCP can map cleanly.
func (s *Server) RegisterHook(_ context.Context, req connector.RegisterHookRequest) (connector.RegisterHookResponse, error) {
	if s.hookRegistry == nil {
		return connector.RegisterHookResponse{}, connector.ErrHookNotConfigured
	}
	h := &hooks.Hook{
		Owner:       req.Owner,
		Name:        req.Name,
		Phase:       req.Phase,
		Agents:      req.Agents,
		Tools:       req.Tools,
		CallbackURL: req.CallbackURL,
		FailMode:    req.FailMode,
		TimeoutMs:   req.TimeoutMs,
	}
	id, err := s.hookRegistry.Register(h)
	if err != nil {
		if errors.Is(err, hooks.ErrInvalidRegistration) {
			return connector.RegisterHookResponse{}, fmt.Errorf("%w: %s", connector.ErrHookInvalidRegistration, err.Error())
		}
		return connector.RegisterHookResponse{}, err
	}
	return connector.RegisterHookResponse{ID: id}, nil
}

// ListHooks returns the full registry snapshot. The slice is owned by
// the registry (List clones internally — see registry.go) so callers
// may not mutate it but reading concurrently is safe.
func (s *Server) ListHooks(_ context.Context) (connector.ListHooksResponse, error) {
	if s.hookRegistry == nil {
		return connector.ListHooksResponse{}, connector.ErrHookNotConfigured
	}
	return connector.ListHooksResponse{Hooks: s.hookRegistry.List()}, nil
}

// DeleteHook removes the hook with the given id. Idempotent only in the
// sense the HTTP layer was: a second delete on the same id always
// returns ErrHookNotFound.
func (s *Server) DeleteHook(_ context.Context, id string) error {
	if s.hookRegistry == nil {
		return connector.ErrHookNotConfigured
	}
	if err := s.hookRegistry.Delete(id); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			return fmt.Errorf("%w: %s", connector.ErrHookNotFound, id)
		}
		return err
	}
	return nil
}
