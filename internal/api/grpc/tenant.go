// tenant.go — gRPC-side tenant/subject isolation helpers.
//
// RFC L/N tenant isolation on the HTTP transport lives entirely in the http
// package's handlers (tenantStore / principalTenantScope /
// requirePrincipalOwnsPathUser). The gRPC interceptors authenticate + stamp the
// principal and enforce SCOPE, but historically performed no tenant/subject ROW
// filtering — so a scoped principal could read/stream/act across tenants over
// gRPC that HTTP folds away. These helpers are the gRPC twins of the HTTP
// choke-points; every tenant-bearing gRPC read must run through them so the two
// transports enforce the identical boundary. Keep in lockstep with
// internal/api/http/tenant_store.go (tenantScopeFromCtx) and auth_principal.go
// (requirePrincipalOwnsPathUser).
package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// grpcTenantScope resolves (tenantID, allTenants) from the ctx principal the
// auth interceptor stamped — the gRPC twin of the HTTP tenantScopeFromCtx.
// No principal (open dev mode), the single-operator legacy principal, and any
// substrate:admin principal all see every tenant; a scoped principal is
// confined to its own tenant.
func grpcTenantScope(ctx context.Context) (tenantID string, allTenants bool) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return "", true
	}
	return p.TenantID, false
}

// grpcPrincipalSubject returns the ctx principal's subject for an audit column
// (empty for open/legacy mode). Mirrors the HTTP principalSubject.
func grpcPrincipalSubject(ctx context.Context) string {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return p.Subject
	}
	return ""
}

// grpcTenantVisible reports whether a row owned by rowTenant is in the caller's
// tenant scope. Callers fold a false result into an opaque NotFound so the gate
// is not a cross-tenant existence oracle (ids are not secret).
func grpcTenantVisible(ctx context.Context, rowTenant string) bool {
	tenantID, all := grpcTenantScope(ctx)
	return all || rowTenant == tenantID
}

// principalMayUseChannelScope enforces the same channel-scope authz the HTTP
// channel routes apply: the operator "global" scope is admin-only (HTTP serves
// it solely under the substrate:admin /v1/_channels/* routes) and a "user"
// scope is confined to the caller's own subject (HTTP userChannelScopeID →
// requirePrincipalOwnsPathUser). Admin / legacy / open (no principal) bypass,
// matching the HTTP exemption set. Returns a gRPC status error to surface as-is,
// or nil when the (scope, scopeID) pair is allowed.
func principalMayUseChannelScope(ctx context.Context, scope, scopeID string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return nil
	}
	switch scope {
	case "user":
		// A non-admin may touch only its OWN user-scoped channel. Opaque
		// NotFound (not PermissionDenied) mirrors the HTTP 404 "unknown_channel"
		// so the gate isn't a cross-subject existence oracle.
		if scopeID != p.Subject {
			return status.Error(codes.NotFound, "no such channel")
		}
		return nil
	default:
		// "global" / "" (and any other non-user scope) is the operator plane;
		// HTTP admits it only on the substrate:admin /v1/_channels/* routes.
		return status.Error(codes.PermissionDenied, "channel scope requires admin")
	}
}
