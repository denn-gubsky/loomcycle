package http

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/webui"
)

// isNotFound reports whether err is a store ErrNotFound (a missing
// token row vs. a genuine store outage — the two are handled
// differently in resolvePrincipal: miss falls through to legacy, outage
// fails closed).
func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

// RFC L OSS multi-tenant authorization — the authenticated-principal
// middleware. Replaces the v0.7.x single-shared-secret authMiddleware:
// resolve the bearer to an auth.Principal {tenant, subject, scopes} FROM
// THE TOKEN, stamp it into ctx, enforce the route's required scope, and
// let the run-creation sites make the principal authoritative over the
// wire tenant_id/user_id. The legacy LOOMCYCLE_AUTH_TOKEN keeps working
// (synthetic default principal) until an admin-scoped token exists.

// authMiddleware authenticates the request, stamps the resolved
// principal into ctx, and enforces the route's required scope.
//
//   - open mode (no auth configured at all) → pass through (dev only)
//   - unknown/expired/invalid bearer → 401 opaque (no oracle)
//   - valid bearer but insufficient scope → 403 + RFC 6750 hint
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authConfigured(r.Context()) {
			// No shared secret AND no tokens → open mode (dev). Startup
			// logged a warning. No principal is stamped; run-creation
			// sites fall back to the wire user_id/tenant_id unchanged.
			next.ServeHTTP(w, r)
			return
		}
		bearer, ok := extractBearer(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p, ok := s.resolvePrincipal(r.Context(), bearer)
		if !ok {
			// Opaque — never distinguish "unknown" from "expired" from
			// "wrong" (no oracle). Detail only under LOOMCYCLE_AUTH_VERBOSE.
			if s.authVerbose() {
				log.Printf("auth: rejected bearer (no matching token / expired / wrong secret)")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if required := requiredScopeFor(r.Method, r.URL.Path); required != "" && !auth.HasScope(p.Scopes, required) {
			// Scope names are public; token state is not.
			w.Header().Set("WWW-Authenticate", `Bearer scope="`+required+`"`)
			http.Error(w, "insufficient scope", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// ResolvePrincipal is the exported wrapper main.go wires into the gRPC
// adapter's Config.PrincipalResolver so both transports share identical
// token resolution (RFC L). Returns (_, false) for unknown/expired/invalid.
func (s *Server) ResolvePrincipal(ctx context.Context, bearer string) (auth.Principal, bool) {
	return s.resolvePrincipal(ctx, bearer)
}

// AuthConfigured is the exported wrapper for the gRPC adapter's
// open-mode decision (RFC L) — true when a legacy secret is set or an
// admin token exists.
func (s *Server) AuthConfigured(ctx context.Context) bool {
	return s.authConfigured(ctx)
}

// extractBearer returns the raw token (sans "Bearer ") from the
// Authorization header, or the Web-UI session cookie value as a
// fallback. A present-but-malformed Authorization header is a miss.
func extractBearer(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		const pfx = "Bearer "
		if len(h) > len(pfx) && strings.EqualFold(h[:len(pfx)], pfx) {
			return h[len(pfx):], true
		}
		return "", false
	}
	if c, err := r.Cookie(webui.SessionCookie); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

// authConfigured reports whether authentication is active. True when a
// legacy shared secret is set, or any admin-scoped OperatorTokenDef
// exists. When neither holds, the server is in dev open-mode.
//
// (A deployment with ONLY narrow tokens and no legacy secret + no admin
// token can't be bootstrapped — the admin endpoints that create tokens
// require auth — so the admin-count check is sufficient in practice.)
func (s *Server) authConfigured(ctx context.Context) bool {
	if s.cfg.Env.AuthToken != "" {
		return true
	}
	if s.store == nil {
		return false
	}
	n, err := s.store.OperatorTokenDefCountActiveAdmin(ctx)
	return err == nil && n > 0
}

func (s *Server) authVerbose() bool {
	return s.cfg.Env.AuthVerbose
}

// resolvePrincipal maps a raw bearer to a principal. Token substrate
// first (indexed peppered-hash lookup, honoring the rotation grace
// window), then the legacy LOOMCYCLE_AUTH_TOKEN fallback (disabled once
// an admin-scoped token exists — the no-lockout migration gate). Returns
// (_, false) for unknown/expired/invalid — the caller maps that to an
// opaque 401. NEVER fails open to the legacy token on a substrate error.
func (s *Server) resolvePrincipal(ctx context.Context, bearer string) (auth.Principal, bool) {
	if bearer == "" {
		return auth.Principal{}, false
	}
	if s.store != nil {
		hash := auth.HashToken(s.cfg.Env.OperatorTokenPepper, bearer)
		row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, hash)
		if err == nil {
			// Valid iff never retired, or still inside the grace window.
			if row.RetiredAt.IsZero() || time.Now().Before(row.RetiredAt) {
				return auth.Principal{
					TenantID:   row.TenantID,
					Subject:    row.Subject,
					Scopes:     row.AllowedScopes,
					TokenDefID: row.DefID,
				}, true
			}
			return auth.Principal{}, false // expired → opaque 401, no legacy fall-open
		}
		// Not found (ErrNotFound) → fall through to legacy. Any OTHER
		// store error is a genuine outage: fail closed below (we do not
		// reach the legacy branch with a usable substrate state).
		if !isNotFound(err) {
			if s.authVerbose() {
				log.Printf("auth: token-substrate lookup error (failing closed): %v", err)
			}
			return auth.Principal{}, false
		}
	}
	// Legacy shared-secret fallback.
	if s.cfg.Env.AuthToken != "" && auth.CompareBearer(bearer, s.cfg.Env.AuthToken) {
		if s.legacyFallbackDisabled(ctx) {
			return auth.Principal{}, false
		}
		return auth.Principal{
			TenantID: "default",
			Subject:  "default",
			Scopes:   []string{auth.ScopeAdmin},
			Legacy:   true,
		}, true
	}
	return auth.Principal{}, false
}

// legacyFallbackDisabled reports whether the LOOMCYCLE_AUTH_TOKEN path is
// retired — true once at least one admin-scoped OperatorTokenDef exists
// (Decision 10/12). On a store error we keep the legacy path ENABLED
// (return false): the alternative would strand the operator on a
// transient DB blip.
func (s *Server) legacyFallbackDisabled(ctx context.Context) bool {
	if s.store == nil {
		return false
	}
	n, err := s.store.OperatorTokenDefCountActiveAdmin(ctx)
	return err == nil && n > 0
}

// applyPrincipal makes the authenticated principal authoritative over
// the caller-asserted wire tenant_id/user_id (Decision 5). Returns the
// authoritative (tenant, subject); on open/un-authed paths returns the
// wire values unchanged. A disagreement is honored server-side and
// logged kind=identity_overridden for triage.
func (s *Server) applyPrincipal(ctx context.Context, wireTenant, wireUser string) (tenant, subject string) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return wireTenant, wireUser
	}
	if wireTenant != "" && wireTenant != p.TenantID {
		log.Printf("auth: identity_overridden kind=tenant wire=%q principal=%q token_def=%q", wireTenant, p.TenantID, p.TokenDefID)
	}
	if wireUser != "" && wireUser != p.Subject {
		log.Printf("auth: identity_overridden kind=subject wire=%q principal=%q token_def=%q", wireUser, p.Subject, p.TokenDefID)
	}
	return p.TenantID, p.Subject
}

// requiredScopeFor maps an HTTP (method, path) to the scope a caller
// must hold. Empty string = any authenticated principal (no specific
// scope). substrate:admin satisfies everything (see auth.HasScope), so
// the default admin/legacy token works on every route; narrow tokens
// get the specific gate. Conservative by design: ambiguous routes fall
// through to "" rather than risk locking a legitimate narrow token out.
func requiredScopeFor(method, path string) string {
	switch {
	// Consumer LLM gateway / OpenAI-compat shims — NOT an admin surface;
	// any authenticated principal may drive inference.
	case path == "/v1/_llm/chat" || path == "/v1/chat/completions" || path == "/v1/embeddings":
		return ""
	// Everything else under /v1/_* is operator-admin: the substrate Def
	// endpoints, runtime admin (pause/resume/state/snapshots/metrics),
	// resolver, users, memory admin, channels admin.
	case strings.HasPrefix(path, "/v1/_"):
		return auth.ScopeAdmin
	// Operator hook management.
	case strings.HasPrefix(path, "/v1/hooks"):
		return auth.ScopeAdmin
	// Run creation (fresh run + session continuation message).
	case method == http.MethodPost && path == "/v1/runs":
		return auth.ScopeRunsCreate
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/sessions/") && strings.HasSuffix(path, "/messages"):
		return auth.ScopeRunsCreate
	// Cancel a run — a write on run state.
	case method == http.MethodDelete && strings.HasPrefix(path, "/v1/agents/"):
		return auth.ScopeRunsCreate
	// Run / agent / session reads.
	case method == http.MethodGet && (strings.HasPrefix(path, "/v1/agents/") ||
		strings.HasPrefix(path, "/v1/users/") ||
		strings.HasPrefix(path, "/v1/sessions/")):
		return auth.ScopeRunsRead
	default:
		return ""
	}
}
