package http

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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

// tokenInvalidateTopic is the backplane channel for cross-replica auth
// cache invalidation (RFC L Decision 11).
const tokenInvalidateTopic = "loomcycle.operator_token_changed"

// EnableTokenCache wires the per-replica auth-token resolution cache
// with the given TTL (RFC L Decision 11). ttl <= 0 leaves the cache
// disabled (direct lookup per request — immediate revocation).
func (s *Server) EnableTokenCache(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	s.tokenCache = newTokenCache(ttl)
}

// invalidateTokenCache flushes the local cache and, in cluster mode,
// broadcasts a flush to peer replicas. Called after a successful token
// mutation (create/rotate/retire). The local flush is essential: the
// backplane self-filters the publisher's own message, so the mutating
// replica would not otherwise see its own invalidation.
func (s *Server) invalidateTokenCache(ctx context.Context) {
	s.tokenCache.flush()
	if s.backplane != nil {
		// Payload is a sentinel — subscribers flush their whole cache
		// (a mutation can change any resolution, incl. the legacy gate).
		if err := s.backplane.Publish(ctx, tokenInvalidateTopic, []byte("flush")); err != nil {
			log.Printf("auth: token-cache invalidation publish failed: %v", err)
		}
	}
}

// SubscribeTokenInvalidations starts the goroutine that flushes the
// local auth cache when a peer replica reports a token mutation. Wired
// from main.go in cluster mode (mirrors the runstate/channel bus
// SubscribeBackplane pattern). Returns once subscribed; the goroutine
// exits on ctx.Done.
func (s *Server) SubscribeTokenInvalidations(ctx context.Context, bp coord.Backplane) error {
	ch, err := bp.Subscribe(ctx, tokenInvalidateTopic)
	if err != nil {
		return err
	}
	go func() {
		for range ch {
			s.tokenCache.flush()
		}
	}()
	return nil
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

// resolvePrincipal maps a raw bearer to a principal, with a short-TTL
// per-replica cache in front of the DB lookup (RFC L Decision 11).
// The cache key is the token's SHA-256 hash (never a secret); a
// mutation flushes it locally + cross-replica. ttl<=0 (or no cache
// wired) → direct lookup every time.
func (s *Server) resolvePrincipal(ctx context.Context, bearer string) (auth.Principal, bool) {
	if bearer == "" {
		return auth.Principal{}, false
	}
	hash := auth.HashToken(s.cfg.Env.OperatorTokenPepper, bearer)
	if p, found, ok := s.tokenCache.get(hash); ok {
		return p, found
	}
	p, found, cacheable := s.resolvePrincipalUncached(ctx, bearer, hash)
	// Only memoise DEFINITIVE outcomes (token hit, legacy fallback, genuine
	// not-found/expired). A transient store OUTAGE resolves to (_, false) but
	// is NOT cacheable: caching it would lock a VALID token out for the whole
	// TTL after the DB recovered — a blip amplified into a ≤30s sticky
	// lockout. On an outage we fail closed for THIS request only and re-probe.
	if cacheable {
		s.tokenCache.put(hash, p, found)
	}
	return p, found
}

// resolvePrincipalUncached is the resolution itself: token substrate
// first (indexed peppered-hash lookup, honoring the rotation grace
// window), then the legacy LOOMCYCLE_AUTH_TOKEN fallback (disabled once
// an admin-scoped token exists — the no-lockout migration gate). Returns
// (_, false) for unknown/expired/invalid — the caller maps that to an
// opaque 401. NEVER fails open to the legacy token on a substrate error.
//
// The third return is `cacheable`: true for a DEFINITIVE outcome (hit, legacy,
// genuine not-found/expired) that resolvePrincipal may memoise; FALSE for a
// transient store outage so a blip is not cached into a sticky lockout.
func (s *Server) resolvePrincipalUncached(ctx context.Context, bearer, hash string) (auth.Principal, bool, bool) {
	if s.store != nil {
		row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, hash)
		if err == nil {
			// Valid iff never retired, or still inside the grace window.
			if row.RetiredAt.IsZero() || time.Now().Before(row.RetiredAt) {
				return auth.Principal{
					TenantID:   row.TenantID,
					Subject:    row.Subject,
					Scopes:     row.AllowedScopes,
					TokenDefID: row.DefID,
				}, true, true
			}
			return auth.Principal{}, false, true // expired → opaque 401, cacheable
		}
		// Not found (ErrNotFound) → fall through to legacy. Any OTHER
		// store error is a genuine outage: fail closed AND not cacheable (we
		// do not reach the legacy branch with a usable substrate state).
		if !isNotFound(err) {
			if s.authVerbose() {
				log.Printf("auth: token-substrate lookup error (failing closed): %v", err)
			}
			return auth.Principal{}, false, false
		}
	}
	// Legacy shared-secret fallback.
	if s.cfg.Env.AuthToken != "" && auth.CompareBearer(bearer, s.cfg.Env.AuthToken) {
		if s.legacyFallbackDisabled(ctx) {
			return auth.Principal{}, false, true
		}
		return auth.Principal{
			TenantID: "default",
			Subject:  "default",
			Scopes:   []string{auth.ScopeAdmin},
			Legacy:   true,
		}, true, true
	}
	return auth.Principal{}, false, true // genuine unknown → cacheable negative
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
//
// Legacy exception (F18): the LOOMCYCLE_AUTH_TOKEN shared-secret fallback is
// the single-operator, NO-BOUNDARY mode — its principal carries a FIXED
// placeholder identity ("default"/"default"), not an authoritative per-actor
// subject. Overriding the caller's wire user_id with that placeholder gave
// zero security benefit (one fully-trusted operator, no isolation boundary)
// while silently scoping every spawn_run / POST /v1/runs to user_id="default"
// — breaking per-user fairness, memory/channel scope, and attribution, and
// the documented "zero-disruption upgrade" (pre-RFC-L user_id was caller-set).
// So for a legacy principal we HONOR the wire user_id (falling back to the
// placeholder only when the caller omits it). A REAL OperatorTokenDef
// principal keeps the strict override — its subject IS an authoritative actor
// and a caller must not be able to spoof another subject.
func (s *Server) applyPrincipal(ctx context.Context, wireTenant, wireUser string) (tenant, subject string) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return wireTenant, wireUser
	}
	if p.Legacy {
		subject = p.Subject
		if wireUser != "" {
			subject = wireUser
		}
		// Tenant stays the legacy default ("default"): the shared token is
		// single-tenant by construction, and tenant routing is a real
		// isolation axis we don't let the wire steer here.
		return p.TenantID, subject
	}
	if wireTenant != "" && wireTenant != p.TenantID {
		log.Printf("auth: identity_overridden kind=tenant wire=%q principal=%q token_def=%q", wireTenant, p.TenantID, p.TokenDefID)
	}
	if wireUser != "" && wireUser != p.Subject {
		log.Printf("auth: identity_overridden kind=subject wire=%q principal=%q token_def=%q", wireUser, p.Subject, p.TokenDefID)
	}
	return p.TenantID, p.Subject
}

// sessionOwnershipOK reports whether the ctx principal may continue or read
// sess. A continuation runs under / a transcript read exposes the SESSION'S
// history, so the gate keeps a caller from acting on a session OUTSIDE ITS
// TENANT — session ids are NOT secrets (returned to callers, logged, shown in
// the UI, embedded in transcripts), so without this a token from tenant-B
// could POST to a tenant-A session id and get cross-tenant memory read/write,
// transcript replay, fairness-cap evasion, and attribution in A.
//
// WHOLE-TENANT model (the chosen Web-UI authz granularity): the boundary is
// the TENANT, not the subject — subjects within one tenant share the tenant's
// workspace (they collaborate), so any acme subject may read/continue any acme
// session. The cross-TENANT boundary (the actual security property) stays
// hard. A super-admin (substrate:admin) crosses tenants by design.
//
// Exempt (return true): no principal (open dev mode); the single-operator
// legacy principal (Legacy=true) — one principal, no boundary, and pre-RFC-L
// sessions carry default/empty identity; and any substrate:admin principal.
func sessionOwnershipOK(ctx context.Context, sess store.Session) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	return sess.TenantID == p.TenantID
}

// handleWhoami serves GET /v1/_me — the Web UI's role source (multi-tenant
// UI authz). Returns the resolved principal so the SPA renders the
// super-admin (all-tenants) vs tenant (own-workspace) experience. Any
// authenticated principal may call it (required scope ""). In open mode
// (no auth configured) there's no principal → return a synthetic
// admin-equivalent so the dev UI stays fully functional.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		writeJSONOK(w, map[string]any{
			"tenant_id": "default", "subject": "default",
			"scopes": []string{auth.ScopeAdmin}, "is_admin": true,
			"legacy": false, "open_mode": true,
		})
		return
	}
	writeJSONOK(w, map[string]any{
		"tenant_id": p.TenantID,
		"subject":   p.Subject,
		"scopes":    p.Scopes,
		"is_admin":  auth.HasScope(p.Scopes, auth.ScopeAdmin),
		"legacy":    p.Legacy,
	})
}

// principalTenantScope resolves the tenant a list read should be scoped
// to, mirroring applyPrincipal's posture (multi-tenant UI authz):
//   - super-admin (substrate:admin) → (wireTenant, all = wireTenant=="") so the
//     UI's tenant switcher can focus one tenant via ?tenant=, or see all;
//   - non-admin (tenant) → (principal.TenantID, all=false); wire/?tenant IGNORED
//     (a tenant can't widen its scope); a disagreement is logged;
//   - open mode (no principal) → (wireTenant, all=wireTenant=="").
func (s *Server) principalTenantScope(ctx context.Context, wireTenant string) (tenantID string, all bool) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return wireTenant, wireTenant == ""
	}
	if wireTenant != "" && wireTenant != p.TenantID {
		log.Printf("auth: tenant_scope_overridden wire=%q principal=%q token_def=%q", wireTenant, p.TenantID, p.TokenDefID)
	}
	return p.TenantID, false
}

// tenantFromCtx resolves the authoritative tenant for definition-plane
// resolution + write-stamping (RFC N), mirroring applyPrincipal's
// authority model:
//
//  1. auth.PrincipalFromContext(ctx).TenantID — the bearer-derived
//     principal stamped by the auth middleware (the floor on authed
//     routes).
//  2. tools.RunIdentity(ctx).TenantID — the run's effective tenant,
//     carried in ctx for in-loop callers (sub-agent spawn, tool
//     dispatch) where no HTTP principal is present but the parent run's
//     tenant flows via RunIdentity. Sub-agents inherit it unchanged.
//  3. "" — the shared/default/legacy tenant (open mode / un-authed
//     internal paths).
//
// NEVER derived from a wire/request body field or model-generated text —
// the tenant boundary is caller/config-authoritative (the same posture
// RFC L's applyPrincipal enforces for run stamping).
func tenantFromCtx(ctx context.Context) string {
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.TenantID != "" {
		return p.TenantID
	}
	return tools.RunIdentity(ctx).TenantID
}

// tenantVisible reports whether the caller may read a row belonging to
// rowTenant. Super-admin (or open mode) sees all; a tenant principal sees
// only its own tenant. Single-row read handlers use this to 404 a
// cross-tenant probe (opaque — no existence oracle).
func (s *Server) tenantVisible(ctx context.Context, rowTenant string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	return rowTenant == p.TenantID
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
	// Whoami — any authenticated principal must be able to learn its own
	// identity (the Web UI's role source); a tenant token needs it too.
	case path == "/v1/_me":
		return ""
	// User listing — any authenticated principal; the handler tenant-scopes
	// the result (a tenant sees only its tenant's users; admin sees all /
	// can focus via ?tenant=). The UI's per-tenant workspace picker needs it.
	case path == "/v1/_users":
		return ""
	// Everything else under /v1/_* is operator-admin: the substrate Def
	// endpoints, runtime admin (pause/resume/state/snapshots/metrics),
	// resolver, users, memory admin, channels admin.
	case strings.HasPrefix(path, "/v1/_"):
		return auth.ScopeAdmin
	// Operator hook management.
	case strings.HasPrefix(path, "/v1/hooks"):
		return auth.ScopeAdmin
	// Prometheus scrape — operator surface, same posture as /v1/_metrics/*.
	case path == "/metrics":
		return auth.ScopeAdmin
	// Per-user channel surface (/v1/users/{id}/channels/...) — graduate the
	// channel scopes. MUST precede the generic "/v1/users/" reads case below
	// so peek resolves to channel:read, not runs:read, and the writes
	// (publish/subscribe/ack) are not left at the any-authenticated default.
	case strings.HasPrefix(path, "/v1/users/") && strings.Contains(path, "/channels/"):
		if method == http.MethodGet {
			return auth.ScopeChannelRead
		}
		return auth.ScopeChannelPublish
	// Run creation (fresh run + session continuation message).
	case method == http.MethodPost && path == "/v1/runs":
		return auth.ScopeRunsCreate
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/sessions/") && strings.HasSuffix(path, "/messages"):
		return auth.ScopeRunsCreate
	// Cancel a run — a write on run state. (The real route is POST
	// /v1/agents/{id}/cancel; the prior DELETE /v1/agents/ case matched no
	// registered route, so cancel fell through to any-authenticated.)
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/agents/") && strings.HasSuffix(path, "/cancel"):
		return auth.ScopeRunsCreate
	// Human-in-the-loop interrupt: resolve is a run-state write, list a read.
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/resolve"):
		return auth.ScopeRunsCreate
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/runs/"):
		return auth.ScopeRunsRead
	// Run / agent / session / user reads.
	case method == http.MethodGet && (strings.HasPrefix(path, "/v1/agents/") ||
		strings.HasPrefix(path, "/v1/users/") ||
		strings.HasPrefix(path, "/v1/sessions/")):
		return auth.ScopeRunsRead
	default:
		return ""
	}
}
