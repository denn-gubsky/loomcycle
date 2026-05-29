package a2a

import (
	"context"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	bridge "github.com/denn-gubsky/loomcycle/internal/a2a"
)

// principalInterceptor authenticates a binding request at the A2A
// frontier and stamps the SDK CallContext.User. It runs inside the SDK
// handler (a CallInterceptor), which is the only place the per-request
// CallContext is available — loomcycle's bearer authMiddleware does NOT
// wrap the binding endpoints (see Server.Mount).
//
// A nil authenticator means open mode (dev): every request is treated
// as authenticated anonymous, mirroring authMiddleware's behaviour when
// LOOMCYCLE_AUTH_TOKEN is unset. The principal flows into run
// attribution only, never into an authz allowlist (CLAUDE.md §8).
type principalInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	auth Authenticator
}

// Before authenticates from the request headers carried on the
// CallContext's ServiceParams and sets callCtx.User accordingly.
func (p *principalInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if callCtx == nil {
		return ctx, nil, nil
	}
	if p.auth == nil {
		callCtx.User = a2asrv.NewAuthenticatedUser("anonymous", nil)
		return ctx, nil, nil
	}
	name, ok := p.auth(serviceParamsToHeader(callCtx.ServiceParams()))
	if ok {
		callCtx.User = a2asrv.NewAuthenticatedUser(name, nil)
	} else {
		callCtx.User = &a2asrv.User{Authenticated: false}
	}
	return ctx, nil, nil
}

// serviceParamsToHeader projects the SDK ServiceParams (request headers,
// lower-cased) back into an http.Header so the Authenticator — written
// against the standard header type — can read them uniformly across
// REST / JSON-RPC / gRPC transports.
func serviceParamsToHeader(sp *a2asrv.ServiceParams) http.Header {
	h := http.Header{}
	if sp == nil {
		return h
	}
	for k, vals := range sp.List() {
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	return h
}

// tenantFromRequest resolves the request tenant per the configured
// routing mode. It is the single trust-boundary read: the tenant comes
// only from the host (host mode) or the routed-tenant context the
// path-mode PathTenantWrapper attached, never from a request body.
// Returns "" for single-tenant / none mode.
func (s *Server) tenantFromRequest(r *http.Request) string {
	switch s.tenancy {
	case "host":
		return tenantFromHost(r.Host)
	case "path":
		// PathTenantWrapper stripped the segment and stamped the context.
		if t, ok := bridge.RoutedTenantFrom(r.Context()); ok {
			return t
		}
		return ""
	default:
		return ""
	}
}

// tenantFromHost extracts the tenant from a "tenant-{id}.<host>"
// subdomain. Returns "" when the leftmost label is not a tenant-prefixed
// label, so a bare host root serves the single-tenant card.
func tenantFromHost(host string) string {
	if host == "" {
		return ""
	}
	// Strip any port.
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	label, _, ok := strings.Cut(host, ".")
	if !ok {
		return ""
	}
	const prefix = "tenant-"
	if !strings.HasPrefix(label, prefix) {
		return ""
	}
	return strings.TrimPrefix(label, prefix)
}

// hostTenantWrap attaches the host-derived routed tenant (host/none
// mode) to the request context before delegating to the SDK handler, so
// the bridge's principalFromContext treats it as authoritative. In path
// mode the routed tenant is already on the context (PathTenantWrapper
// stamped it), so this only adds the host tenant when in host mode.
func (s *Server) hostTenantWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.tenancy != "host" {
			next.ServeHTTP(w, r)
			return
		}
		tenant := tenantFromHost(r.Host)
		next.ServeHTTP(w, r.WithContext(bridge.WithRoutedTenant(r.Context(), tenant)))
	})
}
