// limits.go — the gRPC twin of the HTTP /v1/_limits surface (RFC AW per-scope
// token budgets). TokenLimit is one op-based RPC (list|set|delete). Tenant
// scoping mirrors handleLimits*: admin/legacy/open see every row + an optional
// tenant focus; a substrate:tenant operator sees + writes ONLY its own tenant's
// rows. The tenant-confinement authz is shared with the HTTP transport via
// limits.ResolveWrite so the two enforce the identical boundary.
package grpc

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// tokenLimitTracker is the RFC AW limits-tracker surface the TokenLimit RPC
// needs: live month-to-date usage per scope + a cache reload after a CRUD
// change. *limits.Tracker satisfies it. Kept as an interface so the gRPC package
// doesn't hard-depend on the tracker's construction and tests can stub it.
type tokenLimitTracker interface {
	UsedFor(scope, tenantID, scopeID string) int64
	ReloadLimits(ctx context.Context) error
}

// TokenLimit manages per-scope token budgets (RFC AW), the gRPC twin of
// GET/PUT/DELETE /v1/_limits. op ∈ {list,set,delete}.
func (s *Server) TokenLimit(ctx context.Context, req *loomcyclepb.TokenLimitRequest) (*loomcyclepb.TokenLimitResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "token budgets require persistence (Store not configured)")
	}
	switch req.GetOp() {
	case "list":
		return s.tokenLimitList(ctx, req)
	case "set":
		return s.tokenLimitSet(ctx, req)
	case "delete":
		return s.tokenLimitDelete(ctx, req)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "op must be one of: list, set, delete (got %q)", req.GetOp())
	}
}

// tokenLimitList returns the caller-visible budgets, each with live usage.
// Tenant scope mirrors handleLimitsList / principalTenantScope: admin/legacy/
// open see all rows + an optional wire tenant focus; a scoped principal is
// confined to its own tenant (req.tenant ignored).
func (s *Server) tokenLimitList(ctx context.Context, req *loomcyclepb.TokenLimitRequest) (*loomcyclepb.TokenLimitResponse, error) {
	rows, err := s.store.TokenLimitsAll(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list token limits: %v", err)
	}
	// Reproduce principalTenantScope(ctx, req.tenant): a full-authority caller
	// may focus one tenant via the wire field; a scoped caller is pinned.
	tenantID, all := grpcTenantScope(ctx)
	if all && req.GetTenant() != "" {
		tenantID, all = req.GetTenant(), false
	}
	out := &loomcyclepb.TokenLimitResponse{}
	for _, row := range rows {
		if !all && row.TenantID != tenantID {
			continue // a tenant operator sees only its own tenant's rows
		}
		out.Limits = append(out.Limits, s.tokenLimitEntry(row))
	}
	return out, nil
}

// tokenLimitSet upserts one budget row (op=set). Full-row upsert semantics
// match the HTTP PUT: a present soft/hard tier is set, an absent one is cleared
// (unlimited on that axis). Tenant-confined via the shared limits.ResolveWrite.
func (s *Server) tokenLimitSet(ctx context.Context, req *loomcyclepb.TokenLimitRequest) (*loomcyclepb.TokenLimitResponse, error) {
	if (req.SoftLimit != nil && *req.SoftLimit < 0) || (req.HardLimit != nil && *req.HardLimit < 0) {
		return nil, status.Error(codes.InvalidArgument, "soft_limit/hard_limit must be >= 0")
	}
	tenantID, scopeID, aerr := s.resolveLimitWrite(ctx, req.GetScope(), req.GetTenant(), req.GetScopeId())
	if aerr != nil {
		return nil, aerr
	}
	row := store.TokenLimitRow{
		TenantID:  tenantID,
		Scope:     req.GetScope(),
		ScopeID:   scopeID,
		SoftLimit: req.SoftLimit, // *int64 (proto3 optional) → *int64, present/absent preserved
		HardLimit: req.HardLimit,
		UpdatedAt: time.Now().UTC(),
		UpdatedBy: grpcPrincipalSubject(ctx),
	}
	if err := s.store.TokenLimitPut(ctx, row); err != nil {
		return nil, status.Errorf(codes.Internal, "put token limit: %v", err)
	}
	if err := s.reloadLimits(ctx); err != nil {
		// The row is persisted; a reload fault just lags the cached ceiling.
		return nil, status.Errorf(codes.Internal, "limit stored but reload failed: %v", err)
	}
	return &loomcyclepb.TokenLimitResponse{Limits: []*loomcyclepb.TokenLimitEntry{s.tokenLimitEntry(row)}}, nil
}

// tokenLimitDelete removes a budget (op=delete) → the scope is unlimited again.
// Same tenant-confinement gate as set.
func (s *Server) tokenLimitDelete(ctx context.Context, req *loomcyclepb.TokenLimitRequest) (*loomcyclepb.TokenLimitResponse, error) {
	tenantID, scopeID, aerr := s.resolveLimitWrite(ctx, req.GetScope(), req.GetTenant(), req.GetScopeId())
	if aerr != nil {
		return nil, aerr
	}
	if err := s.store.TokenLimitDelete(ctx, tenantID, req.GetScope(), scopeID); err != nil {
		return nil, status.Errorf(codes.Internal, "delete token limit: %v", err)
	}
	if err := s.reloadLimits(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "limit deleted but reload failed: %v", err)
	}
	return &loomcyclepb.TokenLimitResponse{}, nil
}

// resolveLimitWrite validates + resolves the authoritative (tenant, scope_id)
// for a set/delete under RFC AW confinement, delegating to the shared
// limits.ResolveWrite (same logic the HTTP resolveLimitWrite uses) and mapping
// its typed verdict to a gRPC status: Forbidden → PermissionDenied, else
// InvalidArgument. Returns a nil error when the write is allowed.
func (s *Server) resolveLimitWrite(ctx context.Context, scope, wireTenant, scopeID string) (tenantID, resolvedScopeID string, err error) {
	callerTenant, all := grpcTenantScope(ctx)
	rTenant, rScopeID, aerr := limits.ResolveWrite(scope, wireTenant, scopeID, callerTenant, all)
	if aerr != nil {
		if aerr.Forbidden {
			return "", "", status.Error(codes.PermissionDenied, aerr.Msg)
		}
		return "", "", status.Error(codes.InvalidArgument, aerr.Msg)
	}
	return rTenant, rScopeID, nil
}

// tokenLimitEntry maps a store row + its live usage onto the proto entry.
func (s *Server) tokenLimitEntry(row store.TokenLimitRow) *loomcyclepb.TokenLimitEntry {
	e := &loomcyclepb.TokenLimitEntry{
		TenantId:  row.TenantID,
		Scope:     row.Scope,
		ScopeId:   row.ScopeID,
		SoftLimit: row.SoftLimit, // *int64 → proto3 optional int64
		HardLimit: row.HardLimit,
		Used:      s.usedFor(row.Scope, row.TenantID, row.ScopeID),
		UpdatedBy: row.UpdatedBy,
	}
	if !row.UpdatedAt.IsZero() {
		e.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return e
}

// usedFor reads live month-to-date usage from the shared tracker; 0 when no
// tracker is wired (a store-only deployment).
func (s *Server) usedFor(scope, tenantID, scopeID string) int64 {
	if s.limits == nil {
		return 0
	}
	return s.limits.UsedFor(scope, tenantID, scopeID)
}

// reloadLimits refreshes the shared tracker's cached ceilings after a write;
// a no-op when no tracker is wired.
func (s *Server) reloadLimits(ctx context.Context) error {
	if s.limits == nil {
		return nil
	}
	return s.limits.ReloadLimits(ctx)
}
