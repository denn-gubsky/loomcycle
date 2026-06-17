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
func (s *Server) RegisterHook(ctx context.Context, req connector.RegisterHookRequest) (connector.RegisterHookResponse, error) {
	if s.hookRegistry == nil {
		return connector.RegisterHookResponse{}, connector.ErrHookNotConfigured
	}
	// RFC AF: stamp the AUTHORITATIVE owning-tenant from the resolved principal,
	// never a wire/body field. A non-admin tenant operator's hooks are confined
	// to its own tenant (the dispatcher's Match fires them only on that tenant's
	// runs); admin / legacy / open-mode (allTenants) register an operator-global
	// hook (Tenant="") that fires on every run — preserving pre-RFC-AF behaviour.
	tenantID, allTenants := tenantScopeFromCtx(ctx)
	hookTenant := tenantID
	if allTenants {
		hookTenant = ""
	}
	h := &hooks.Hook{
		Tenant:      hookTenant,
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
func (s *Server) ListHooks(ctx context.Context) (connector.ListHooksResponse, error) {
	if s.hookRegistry == nil {
		return connector.ListHooksResponse{}, connector.ErrHookNotConfigured
	}
	all := s.hookRegistry.List()
	// RFC AF: admin / legacy / open-mode see every hook; a non-admin tenant
	// operator sees ONLY its own tenant's hooks. The operator/global hooks
	// (Tenant="") are deliberately hidden too — their callback URLs + owners are
	// infra a tenant must not introspect (cross-tenant config leak).
	tenantID, allTenants := tenantScopeFromCtx(ctx)
	if allTenants {
		return connector.ListHooksResponse{Hooks: all}, nil
	}
	filtered := make([]*hooks.Hook, 0, len(all))
	for _, h := range all {
		if h.Tenant == tenantID {
			filtered = append(filtered, h)
		}
	}
	return connector.ListHooksResponse{Hooks: filtered}, nil
}

// DeleteHook removes the hook with the given id. Idempotent only in the
// sense the HTTP layer was: a second delete on the same id always
// returns ErrHookNotFound.
func (s *Server) DeleteHook(ctx context.Context, id string) error {
	if s.hookRegistry == nil {
		return connector.ErrHookNotConfigured
	}
	// RFC AF: a non-admin tenant operator may delete ONLY its own tenant's
	// hooks. Resolve the target's tenant first and fold a cross-tenant (or
	// global-hook) id into the SAME opaque ErrHookNotFound a missing id returns,
	// so the delete path is not a cross-tenant existence oracle.
	if tenantID, allTenants := tenantScopeFromCtx(ctx); !allTenants {
		var target *hooks.Hook
		for _, h := range s.hookRegistry.List() {
			if h.ID == id {
				target = h
				break
			}
		}
		if target == nil || target.Tenant != tenantID {
			return fmt.Errorf("%w: %s", connector.ErrHookNotFound, id)
		}
	}
	if err := s.hookRegistry.Delete(id); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			return fmt.Errorf("%w: %s", connector.ErrHookNotFound, id)
		}
		return err
	}
	return nil
}
