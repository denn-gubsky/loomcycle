package a2a

import (
	"context"
	"net/http"
	"strings"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
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
	if !ok {
		// Auth is configured but the request carried no valid credential.
		// REJECT at the frontier: returning a non-nil error from a
		// CallInterceptor.Before short-circuits the call before the
		// executor ever runs (a2asrv intercepted_handler), so this single
		// gate covers message/send, streaming, resume, and cancel across
		// all three bindings uniformly. Without it a wrong/absent bearer
		// would still spawn a run with an anonymous principal, i.e. the
		// A2A surface would ignore LOOMCYCLE_AUTH_TOKEN entirely (the
		// binding endpoints are deliberately not wrapped by the HTTP
		// bearer authMiddleware — see Server.Mount).
		callCtx.User = &a2asrv.User{Authenticated: false}
		return ctx, nil, a2asdk.ErrUnauthenticated
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(name, nil)
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
	// DNS is case-insensitive, so normalise the label before extracting
	// the tenant id — otherwise "tenant-Acme.host" and "tenant-acme.host"
	// would resolve to two distinct tenant partitions for the same
	// authority.
	label, _, ok := strings.Cut(strings.ToLower(host), ".")
	if !ok {
		return ""
	}
	const prefix = "tenant-"
	if !strings.HasPrefix(label, prefix) {
		return ""
	}
	return strings.TrimPrefix(label, prefix)
}

// hostTenantWrap makes the tenancy routing decision authoritative on
// every A2A route it wraps, so the bridge's principalFromContext never
// falls back to the peer-supplied body tenant in a multi-tenant
// deployment:
//
//   - host mode: stamp the host-derived tenant (possibly "" for a
//     non-tenant host) — empty is stamped on purpose so a bare host is
//     authoritatively single-tenant rather than body-controlled.
//   - path mode: PathTenantWrapper has already stamped the tenant when
//     the request carried a "/{tenant}" prefix. A binding route reached
//     WITHOUT a prefix has no stamp yet; mark it authoritative-empty here
//     so a direct un-prefixed hit cannot smuggle a body tenant.
//   - none/single-tenant mode: leave the context unstamped so the body
//     tenant is permitted (it is attribution-only, never an allowlist).
func (s *Server) hostTenantWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch s.tenancy {
		case "host":
			next.ServeHTTP(w, r.WithContext(bridge.WithRoutedTenant(r.Context(), tenantFromHost(r.Host))))
		case "path":
			if _, ok := bridge.RoutedTenantFrom(r.Context()); ok {
				next.ServeHTTP(w, r) // PathTenantWrapper already stamped it
				return
			}
			next.ServeHTTP(w, r.WithContext(bridge.WithRoutedTenant(r.Context(), "")))
		default:
			next.ServeHTTP(w, r)
		}
	})
}
