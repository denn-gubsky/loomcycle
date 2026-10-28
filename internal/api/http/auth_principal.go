package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/runner"
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
//   - open mode (no legacy secret, no config principal, no admin token) →
//     pass through (dev only)
//   - unknown/expired/invalid bearer → 401 opaque (no oracle)
//   - valid bearer but insufficient scope → 403 insufficient_scope JSON naming
//     the scope + the RFC 6750 hint
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authConfigured(r.Context()) {
			// No shared secret, principal or admin token → open mode (dev). Startup
			// logged a warning. No principal is stamped; run-creation
			// sites fall back to the wire user_id/tenant_id unchanged.
			next.ServeHTTP(w, r)
			return
		}
		p, ui, ok := s.authenticateRequest(r)
		if !ok {
			// Opaque — never distinguish "unknown" from "expired" from
			// "wrong" (no oracle). Detail only under LOOMCYCLE_AUTH_VERBOSE.
			if s.authVerbose() {
				slog.Info("auth: rejected credential (no matching token or session / expired / wrong secret)")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if ui != nil {
			// A cookie authenticated the request, so a browser may have sent
			// it on another site's behalf.
			if code := uiCSRFRefusal(r); code != "" {
				writeCSRFRefusal(w, code)
				return
			}
			r = r.WithContext(withUIAuth(r.Context(), *ui))
			// A user awaiting approval may learn that it waits, nothing else.
			if ui.pending && !(r.Method == http.MethodGet && r.URL.Path == "/v1/_me") {
				writeJSONError(w, http.StatusForbidden, "awaiting_approval",
					"your account is awaiting an operator's approval")
				return
			}
		}
		if required, admitted := scopeGate(p, r.Method, r.URL.Path); !admitted && !ownIdentityRequest(p, r) {
			writeScopeError(w, required, "insufficient scope")
			return
		}
		// After the scope check, and independent of it: a credential limited
		// to certain agents is held to the run plane however wide its scopes.
		if p.Restrictions.RunPlaneOnly() && !runPlaneRoute(r.Method, r.URL.Path) {
			writeJSONError(w, http.StatusForbidden, "restricted_token",
				"this token is limited to starting and following runs of its allowed agents")
			return
		}
		// RFC EY: the route's type-level ACL question, after every legacy
		// gate admitted the request. The mode and transport ride the ctx so
		// the instance checks downstream (the run intakes, the MCP per-tool
		// gate behind /v1/_mcp) evaluate under the same rules.
		ctx := authz.WithTransport(authz.WithMode(r.Context(), s.aclMode()), "http")
		r = r.WithContext(ctx)
		ctx, ok = s.aclGateHTTP(w, r, p)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(ctx, p)))
	})
}

// scopeGate is the route's scope gate for an authenticated principal p: the
// scope the route requires, and whether p is admitted — by holding it, or on
// one of the paths below that admit a principal without it. It returns the
// scope so the refusal can name it.
func scopeGate(p auth.Principal, method, path string) (required string, admitted bool) {
	// RFC FA D16: a user-management credential reaches its route list and
	// nothing else — not even the routes any authenticated principal may
	// call. Checked first so no member, self-service or gateway path below
	// can admit it elsewhere.
	if auth.IsAuthManager(p) {
		return auth.ScopeAuthManage, authManageRoute(method, path)
	}
	required = requiredScopeFor(method, path)
	if required == "" || auth.HasScope(p.Scopes, required) {
		return required, true
	}

	// RFC CB: the scope check failed, but a NON-ISOLATED user token (a
	// tenant member — runs:*/channel:*, no substrate:tenant) may still
	// reach a member-accessible tenant surface. Such a token already has
	// whole-tenant DATA access inside a run (it is not isolated — see
	// auth/grants.go), so this admits the matching HTTP surfaces for
	// read+write. Isolated tokens (substrate:user) fail !IsIsolated and
	// stay 403; the operator-control carve-outs (minting / roster /
	// budget-write / erasure / hooks) are excluded by
	// tenantMemberAccessible; and every such handler still confines a
	// member to its own tenant via principalTenantScope / tenantFromCtx.
	memberOK := tenantMemberAccessible(method, path) && !auth.IsIsolated(p, true)
	// RFC CN: the user self-service surfaces (the credential store + the MCP
	// transport) admit ANY authenticated principal that carries a subject —
	// INCLUDING an isolated substrate:user user, who is otherwise held at the
	// tenant-member isolation floor — so a user can store its OWN scope=user
	// API tokens (a personal Slack/Telegram bot token, a per-user webhook
	// secret). The handler / MCP per-tool gate then confine a non-tenant caller
	// to scope=user on its own subject; tenant/agent scope still requires
	// substrate:tenant.
	selfOK := userSelfServiceRoute(method, path) && p.Subject != ""
	// The raw model endpoints stay open to every principal that is
	// not an isolated user: a member token holds no llm:gateway scope
	// of its own, and requiring one would have broken every member
	// token minted before the scope existed.
	gatewayMemberOK := required == auth.ScopeLLMGateway && !auth.IsIsolated(p, true)
	return required, memberOK || selfOK || gatewayMemberOK
}

// RouteAdmits reports whether an authenticated principal p gets past the
// legacy gates of the route — its scope (with the member, self-service and
// gateway paths) and the run-plane restriction — before any ACL question. It
// is what the cross-transport parity test (internal/api/acltest) asks of every
// gRPC RPC's HTTP twin; authMiddleware runs the same two functions.
func RouteAdmits(p auth.Principal, method, path string) bool {
	if _, admitted := scopeGate(p, method, path); !admitted {
		return false
	}
	return !p.Restrictions.RunPlaneOnly() || runPlaneRoute(method, path)
}

// admitPrincipalAgent refuses a top-level run of an agent the caller's token
// is not limited to. EVERY place that creates a top-level run row calls it
// (TestRunRowCreators_AllAdmitThePrincipal holds that), before it looks the
// agent up, so the answer does not say whether the agent exists.
//
// It reads the live principal, so a run started under a definition's own
// identity — a schedule, a webhook, a channel hook — is not gated: there is no
// credential there to restrict. Neither is an agent the allowed one spawns,
// which never comes this way.
func (s *Server) admitPrincipalAgent(ctx context.Context, agent string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if !auth.MayStartAgent(p, ok, agent) {
		slog.Warn("auth: agent_not_allowed", "agent", agent, "subject", p.Subject, "token_def", p.TokenDefID)
		return fmt.Errorf("%w: %q", runner.ErrAgentNotAllowed, agent)
	}
	// RFC EY: the ACL's instance question, `run agent:<name>`, after the
	// token's own allowlist. A queued run started after a restart has no
	// principal but its recorded caller's set, compiled again: it is asked
	// for that caller, so a deny bound while it waited refuses it.
	if rc, has := recordedCallerFrom(ctx); !ok && has {
		p = rc.origin.principal()
		p.Effective, ok = rc.eff, true
	}
	return s.aclAdmitAgent(ctx, p, ok, agent)
}

// refuseAgentlessCall refuses a call that reaches a model or starts agents
// with no single agent name to check — a team walk, a decision — when it is
// made DIRECTLY by a credential limited to certain agents. The run-plane gate
// on each transport already keeps such a credential off those surfaces; this
// is the same rule held at the call, for a transport that forgets.
//
// A call made from inside a run is not refused: that is the allowed agent
// using a tool its definition gives it, which the allowlist does not govern.
func (s *Server) refuseAgentlessCall(ctx context.Context, what string) error {
	if tools.RunID(ctx) != "" {
		return nil
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || !p.Restrictions.RunPlaneOnly() {
		return nil
	}
	return fmt.Errorf("%w: %s", runner.ErrAgentNotAllowed, what)
}

// writeAgentNotAllowed is the HTTP form of that refusal.
func writeAgentNotAllowed(w http.ResponseWriter, err error) {
	var denied *errACLDenied
	if errors.As(err, &denied) {
		writeACLDenied(w, denied.surface, denied.decision)
		return
	}
	writeJSONError(w, http.StatusForbidden, "agent_not_allowed", err.Error())
}

// isGatewayRoute reports whether path is one of the raw model endpoints.
func isGatewayRoute(path string) bool {
	return path == "/v1/_llm/chat" || path == "/v1/chat/completions" || path == "/v1/embeddings"
}

// runPlaneRoute reports whether a route belongs to the run plane: starting,
// continuing, reading and controlling runs. It is the WHOLE of what a
// credential with an agent allowlist may reach (auth.Restrictions.RunPlaneOnly
// says why the rule is an allowlist and not a list of refusals), so a route
// added later is refused to such a credential until someone decides it
// belongs here.
//
// What is deliberately absent: every definition route, the team and schedule
// triggers, the model gateways and the decision call (each reaches a model or
// an agent with no allowlist in the way), channels, memory, documents, and
// the credential store.
func runPlaneRoute(method, path string) bool {
	// Deleting a run tree or a session is not part of starting and following
	// runs, and is off the run plane on gRPC and MCP too: a token limited to
	// certain agents does not erase records, whatever scope it was given.
	if method == http.MethodDelete && (strings.HasPrefix(path, "/v1/sessions/") ||
		strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/tree")) {
		return false
	}
	switch {
	case path == "/v1/runs", path == "/v1/runs:batch", strings.HasPrefix(path, "/v1/runs/"):
		return true
	case strings.HasPrefix(path, "/v1/sessions/"):
		return true
	case path == "/v1/_runnable-agents", path == "/v1/_me", path == "/v1/config":
		return method == http.MethodGet
	// Which overrides a run of an agent may set. It is part of starting a run:
	// a client-held token for one agent needs it to build the form it starts
	// that agent with. It reveals nothing a run attempt would not — the
	// override lock refuses before a run exists — and the handler applies the
	// agent allowlist first, so the token learns nothing about other agents.
	case isAgentOverridablePath(path):
		return method == http.MethodGet
	case path == "/v1/_mcp":
		// The MCP transport itself; the per-tool gate inside it applies the
		// same rule to each tool (the MCP package's runPlaneTools).
		return true
	}
	// /v1/agents/{agent_id} and its cancel are a run by its agent id;
	// /v1/agents/{name}/channels is not.
	if rest, ok := strings.CutPrefix(path, "/v1/agents/"); ok {
		return !strings.Contains(rest, "/") || strings.HasSuffix(rest, "/cancel") && strings.Count(rest, "/") == 1
	}
	// A user's own runs and pending interrupts; not their channels.
	if rest, ok := strings.CutPrefix(path, "/v1/users/"); ok && method == http.MethodGet {
		_, tail, found := strings.Cut(rest, "/")
		return found && (tail == "agents" || tail == "agents/stream" || tail == "interrupts")
	}
	return false
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
			slog.Warn("auth: token-cache invalidation publish failed", "err", err)
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

// AuthConfigured is the exported wrapper for the gRPC adapter's and the A2A
// frontier's open-mode decision (RFC L) — true when a legacy secret is set,
// config principals are declared or an admin token exists, and true when the
// store cannot say (fails closed).
func (s *Server) AuthConfigured(ctx context.Context) bool {
	return s.authConfigured(ctx)
}

// AuthConfiguredErr is the same decision with the store error returned rather
// than read as "configured", for the boot check, which must not guess.
func (s *Server) AuthConfiguredErr(ctx context.Context) (bool, error) {
	return s.authConfiguredErr(ctx)
}

// authenticateRequest resolves the request's credential, in this order: an
// Authorization header; the client-tools WebSocket subprotocol bearer (not in
// strict mode); a Web UI session cookie; and, in token mode only, the legacy
// bearer cookie of earlier releases (RFC FA decision 2). ui is set when a
// cookie authenticated the request, so the caller applies the CSRF check. A
// present credential that fails is a refusal — the next kind is not tried.
//
// The subprotocol bearer comes before the cookies on purpose: a browser
// extension holding its own bearer may also carry the user's session cookie,
// and its chrome-extension:// Origin would fail the cookie path's origin check.
func (s *Server) authenticateRequest(r *http.Request) (auth.Principal, *uiAuth, bool) {
	strict := s.cfg().ResolvedAuth.Strict()
	ctx := r.Context()
	if bearer, ok := extractBearer(r, strict); ok {
		p, ok := s.resolvePrincipal(ctx, bearer)
		return p, nil, ok
	}
	if r.Header.Get("Authorization") != "" {
		return auth.Principal{}, nil, false
	}
	if id, ok := s.sessionIDFromCookie(r); ok {
		p, ua, ok := s.sessionPrincipal(r, id)
		if !ok {
			return auth.Principal{}, nil, false
		}
		return p, &ua, true
	}
	if bearer, ok := legacyBearerCookie(r, strict); ok {
		warnEvery(&s.legacyCookieWarnedAt, "auth: a browser signed in with the "+webui.LegacyBearerCookie+
			" bearer cookie of an earlier release; it keeps working in token mode for this release only. "+
			"Signing in again moves the browser to a session")
		p, ok := s.resolvePrincipal(ctx, bearer)
		if !ok {
			return auth.Principal{}, nil, false
		}
		return p, &uiAuth{}, true
	}
	return auth.Principal{}, nil, false
}

// credentialPresented reports whether the request carries any credential
// authenticateRequest would try, valid or not.
func (s *Server) credentialPresented(r *http.Request) bool {
	strict := s.cfg().ResolvedAuth.Strict()
	if _, ok := extractBearer(r, strict); ok || r.Header.Get("Authorization") != "" {
		return true
	}
	if c, err := r.Cookie(s.sessionCookieName(r)); err == nil && c.Value != "" {
		return true
	}
	_, ok := legacyBearerCookie(r, strict)
	return ok
}

// legacyBearerCookie returns the bearer the pre-session Web UI kept in a
// cookie. Read in token mode only, for one release; strict mode never reads a
// bearer from a cookie.
func legacyBearerCookie(r *http.Request, strict bool) (string, bool) {
	if strict {
		return "", false
	}
	c, err := r.Cookie(webui.LegacyBearerCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// extractBearer returns the raw token (sans "Bearer ") from the Authorization
// header or, outside strict mode, the client-tools WebSocket subprotocol. A
// present-but-malformed Authorization header is a miss. No bearer is ever read
// from a cookie here.
func extractBearer(r *http.Request, strict bool) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		const pfx = "Bearer "
		if len(h) > len(pfx) && strings.EqualFold(h[:len(pfx)], pfx) {
			return h[len(pfx):], true
		}
		return "", false
	}
	// WebSocket clients (browsers can't set an Authorization header on a
	// WebSocket) carry the bearer as a Sec-WebSocket-Protocol entry of the form
	// "bearer.<token>" (RFC BC). The app subprotocol is negotiated separately in
	// the /v1/client-tools handler; this entry is never echoed back. Read only on
	// a WebSocket upgrade to that route (RFC ES #5): elsewhere the header would be
	// a second credential channel that proxies and logs do not treat as one.
	// Strict mode does not read it (RFC FA §4): a browser there authenticates
	// by its session.
	if strict || !isClientToolsUpgrade(r) {
		return "", false
	}
	if protos := r.Header.Get("Sec-WebSocket-Protocol"); protos != "" {
		for _, p := range strings.Split(protos, ",") {
			if tok, ok := strings.CutPrefix(strings.TrimSpace(p), "bearer."); ok && tok != "" {
				return tok, true
			}
		}
	}
	return "", false
}

// authConfiguredErr reports whether authentication is active: true when a
// legacy shared secret is set, the config declares any principal (RFC ES F2),
// or any admin-scoped OperatorTokenDef exists. When none holds, the server is
// in dev open-mode. The error is set when the store could not be asked, and
// the answer is then unknown.
//
// (A deployment with ONLY narrow tokens and no legacy secret + no admin
// token can't be bootstrapped — the admin endpoints that create tokens
// require auth — so the admin-count check is sufficient in practice.)
func (s *Server) authConfiguredErr(ctx context.Context) (bool, error) {
	return s.authConfiguredUnder(ctx, s.cfg())
}

// authConfigured is the request-path form of authConfiguredErr, and it fails
// CLOSED: when the store cannot answer, auth is treated as on, so the request
// needs a valid bearer. Reading an error as "no admin token" would be open
// mode — every route served with no principal — on a database blip, or on a
// request whose context a client cancelled (a half-closed socket does it).
func (s *Server) authConfigured(ctx context.Context) bool {
	on, err := s.authConfiguredErr(ctx)
	if err != nil {
		s.logAdminCountErr(ctx, err)
		return true
	}
	return on
}

// logAdminCountErr logs a failed admin-token count that the caller is failing
// closed on. A store fault is logged; a cancelled request is not (outside
// LOOMCYCLE_AUTH_VERBOSE), because a client can cancel its own request at will.
func (s *Server) logAdminCountErr(ctx context.Context, err error) {
	if ctx.Err() == nil || s.authVerbose() {
		slog.Error("auth: admin-token count failed; failing closed", "err", err)
	}
}

func (s *Server) authVerbose() bool {
	return s.cfg().Env.AuthVerbose
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
	h, err := s.hasher()
	if err != nil {
		// Unreachable past boot (a malformed key is fatal there); never fall
		// back to an unpeppered lookup.
		slog.Error("auth: token hashing unavailable (failing closed)", "err", err)
		return auth.Principal{}, false
	}
	hash := h.Hash(bearer)
	if p, found, ok := s.tokenCache.get(hash); ok {
		if !found {
			return p, false
		}
		if p, ok = s.liveTokenPrincipal(ctx, p); !ok {
			return auth.Principal{}, false
		}
		return s.enrichPrincipal(ctx, p), true
	}
	p, found, cacheable, validUntil := s.resolvePrincipalUncached(ctx, h, bearer, hash)
	// Only memoise DEFINITIVE outcomes (token hit, legacy fallback, genuine
	// not-found/expired). A transient store OUTAGE resolves to (_, false) but
	// is NOT cacheable: caching it would lock a VALID token out for the whole
	// TTL after the DB recovered — a blip amplified into a ≤30s sticky
	// lockout. On an outage we fail closed for THIS request only and re-probe.
	if cacheable {
		s.tokenCache.put(hash, p, found, validUntil)
	}
	if !found {
		return p, false
	}
	if p, found = s.liveTokenPrincipal(ctx, p); !found {
		return auth.Principal{}, false
	}
	// The RFC EY set is resolved OUTSIDE the token cache: it has its own
	// cache keyed on the tenant's acl_version, so a binding change reaches
	// the next request without a token-cache flush.
	return s.enrichPrincipal(ctx, p), true
}

// liveTokenPrincipal applies, after the token cache, what a token row cannot
// fix at mint because it depends on the user as the user is NOW. It reads
// the user row on every request it has work for, so neither a cached
// resolution nor a removal whose token retire failed part-way can carry a
// stale user past it, on any replica:
//
//   - a gate-issued token (the on-use half of the RFC FA §3.7c removal
//     policy) stands only while its user is active and the gate that issued
//     it is configured: an operator who takes a gate out of the config
//     withdraws its trust, and what it vouched for must not outlive that
//     until its TTL (a gate's browser sessions follow the same rule in
//     identitySessionPrincipal);
//   - an inherit token (RFC EY §8) stands only while its user exists and is
//     active, and its scopes BECOME the implied set of the user's current
//     access_mode, so a promotion widens it and a demotion narrows it on the
//     next request with no re-mint. Its restrictions are untouched, so they
//     still only narrow.
//
// A frozen token from neither path is returned as it is, with no read. Fails
// closed: a user that cannot be read, or an access_mode that grants nothing,
// refuses the request.
func (s *Server) liveTokenPrincipal(ctx context.Context, p auth.Principal) (auth.Principal, bool) {
	if p.IssuedByGate == "" && !p.InheritsAuthority {
		return p, true
	}
	if p.IssuedByGate != "" {
		// issuingGate, not configuredGate: a token-mode deployment's implicit
		// token gate issues app tokens too (RFC FL), and stands while offered.
		if _, ok := s.issuingGate(p.IssuedByGate); !ok {
			return auth.Principal{}, false
		}
		// An app's token (RFC FL) also stands only while its client does.
		if !s.appTokenStands(ctx, p) {
			return auth.Principal{}, false
		}
	}
	if s.store == nil {
		return auth.Principal{}, false
	}
	u, err := s.store.UserGet(ctx, p.TenantID, p.Subject)
	if err != nil {
		if !isNotFound(err) && ctx.Err() == nil {
			slog.Error("auth: reading a token's user failed (refusing)", "token_def", p.TokenDefID, "err", err)
		}
		return auth.Principal{}, false
	}
	if u.Status != "active" {
		return auth.Principal{}, false
	}
	if p.InheritsAuthority {
		scopes, err := auth.GrantableUserScopes(u.AccessMode)
		if err != nil {
			slog.Error("auth: an inherit token's user grants no scopes (refusing)", "token_def", p.TokenDefID, "err", err)
			return auth.Principal{}, false
		}
		p.Scopes = scopes
	}
	return p, true
}

// resolvePrincipalUncached is the resolution itself: token substrate
// first (indexed peppered-hash lookup, honoring the rotation grace
// window; on a miss, the older forms of lookupOlderTokenForm), then the
// legacy LOOMCYCLE_AUTH_TOKEN fallback (disabled once an admin-scoped
// token exists — the no-lockout migration gate). Returns
// (_, false) for unknown/expired/invalid — the caller maps that to an
// opaque 401. NEVER fails open to the legacy token on a substrate error.
//
// The third return is `cacheable`: true for a DEFINITIVE outcome (hit, legacy,
// genuine not-found/expired) that resolvePrincipal may memoise; FALSE for a
// transient store outage so a blip is not cached into a sticky lockout.
//
// The fourth is when a minted token stops authenticating (zero = never, and
// for every other kind of principal), so the cache can stop answering for it
// at that instant rather than at the end of its own TTL.
func (s *Server) resolvePrincipalUncached(ctx context.Context, h auth.TokenHasher, bearer, hash string) (auth.Principal, bool, bool, time.Time) {
	var never time.Time
	// A refusal that went through the older-form lookups is not cached
	// (RFC ES F3; see lookupOlderTokenForm).
	negativeCacheable := true
	if s.store != nil {
		row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, hash)
		if isNotFound(err) && (s.wrappedTokens.Load() || h.HasPrevious()) {
			row, err = s.lookupOlderTokenForm(ctx, h, bearer, hash, err)
			negativeCacheable = false
		}
		if err == nil {
			p, ok, validUntil := tokenRowPrincipal(row, time.Now())
			return p, ok, true, validUntil // a refusal (expired, unreadable) is cacheable
		}
		// Not found (ErrNotFound) → fall through to legacy. Any OTHER
		// store error is a genuine outage: fail closed AND not cacheable (we
		// do not reach the legacy branch with a usable substrate state).
		if !isNotFound(err) {
			if s.authVerbose() {
				slog.Error("auth: token-substrate lookup error (failing closed)", "err", err)
			}
			return auth.Principal{}, false, false, never
		}
	}
	// Config-declared principals (RFC AO) — static service identities from the
	// `principals:` block, matched constant-time. Tried AFTER the minted
	// substrate (a minted def wins a value clash) and BEFORE the legacy
	// fallback. A definitive, cacheable outcome.
	if p, ok := auth.MatchDeclared(bearer, s.cfg().ResolvedPrincipals); ok {
		return p, true, true, never
	}
	// Legacy shared-secret fallback.
	if s.cfg().Env.AuthToken != "" && auth.CompareBearer(bearer, s.cfg().Env.AuthToken) {
		disabled, err := s.legacyFallbackDisabled(ctx)
		if err != nil {
			// Unknown whether the secret is retired: refuse, and do not cache
			// the refusal, so the next request asks the store again.
			s.logAdminCountErr(ctx, err)
			return auth.Principal{}, false, false, never
		}
		if disabled {
			return auth.Principal{}, false, negativeCacheable, never
		}
		return auth.Principal{
			TenantID: "default",
			Subject:  "default",
			Scopes:   []string{auth.ScopeAdmin},
			Legacy:   true,
		}, true, true, never
	}
	return auth.Principal{}, false, negativeCacheable, never // genuine unknown → cacheable negative, unless the older-form lookups ran
}

// tokenRowPrincipal is the principal a minted token row authenticates as at
// now, whether it authenticates at all, and when it stops (zero = never). Every
// path that turns a token row into a principal uses it, so a Web UI session
// built on a token can never read the row more generously than the bearer
// lookup does.
func tokenRowPrincipal(row store.OperatorTokenDefRow, now time.Time) (auth.Principal, bool, time.Time) {
	var never time.Time
	// Valid iff not retired (or still inside the grace window) and not past its
	// fixed lifetime. Both refusals look the same.
	if !row.ValidAt(now) {
		return auth.Principal{}, false, never
	}
	// A restrictions record this build cannot read refuses the token. Reading
	// it as "unrestricted" would give a narrowed credential the whole of its
	// scopes.
	restrictions, err := auth.ParseRestrictions(row.RestrictionsJSON)
	if err != nil {
		slog.Warn("auth: token refused", "token_def", row.DefID, "err", err)
		return auth.Principal{}, false, never
	}
	// Likewise an authority mode this build does not know: reading it as
	// frozen would hand the token its stored scopes, as inherit its user's.
	inherit, err := auth.TokenInheritsAuthority(row.Authority)
	if err != nil {
		slog.Warn("auth: token refused", "token_def", row.DefID, "err", err)
		return auth.Principal{}, false, never
	}
	validUntil := never
	if !row.RetiredAt.IsZero() || !row.ExpiresAt.IsZero() {
		validUntil = row.ValidUntil()
	}
	return auth.Principal{
		TenantID:     row.TenantID,
		Subject:      row.Subject,
		Scopes:       row.AllowedScopes,
		TokenDefID:   row.DefID,
		Restrictions: restrictions,
		IssuedByGate: row.IssuedByGate,
		OAuthClient:  row.OAuthClient,
		OAuthFamily:  row.OAuthFamily,
		// Scopes above are the set the user held at mint; liveTokenPrincipal
		// replaces them with the current one on every request.
		InheritsAuthority: inherit,
	}, true, validUntil
}

// legacyFallbackDisabled reports whether the LOOMCYCLE_AUTH_TOKEN path is
// retired — true once at least one admin-scoped OperatorTokenDef that can
// authenticate exists (Decision 10/12). The error is set when the count could not be read; the
// caller then refuses the legacy secret (fails closed). Keeping it enabled on
// an error would revive a retired admin credential, and does not spare the
// operator a DB blip: a real outage already fails closed at the token lookup
// before this runs, so the error that reaches here is a cancelled request.
func (s *Server) legacyFallbackDisabled(ctx context.Context) (bool, error) {
	if s.store == nil {
		return false, nil
	}
	h, err := s.hasher()
	if err != nil {
		return false, err
	}
	// Only an admin token that can still authenticate counts (RFC ES F3): an
	// unpeppered row not yet rewrapped, or one under a key no longer
	// configured, would otherwise switch the shared secret off with no admin
	// token left.
	include, exclude := h.UsablePrefixes()
	n, err := s.store.OperatorTokenDefCountActiveAdminMatching(ctx, store.TokenHashMatch{Prefixes: include, ExcludePrefixes: exclude})
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
	tenant, subject = auth.ResolveWireIdentity(p, ok, wireTenant, wireUser)
	// Log a disagreement only for a REAL principal (legacy honors the wire
	// user_id by design; no-principal is the open-mode passthrough). Triage
	// only — the override itself is the security property and lives in
	// auth.ResolveWireIdentity, the rule shared with the MCP transport.
	if ok && !p.Legacy {
		if wireTenant != "" && wireTenant != p.TenantID {
			slog.Warn("auth: identity_overridden", "kind", "tenant", "wire", wireTenant, "principal", p.TenantID, "token_def", p.TokenDefID)
		}
		if wireUser != "" && wireUser != p.Subject {
			slog.Warn("auth: identity_overridden", "kind", "subject", "wire", wireUser, "principal", p.Subject, "token_def", p.TokenDefID)
		}
	}
	return tenant, subject
}

// operatorKeyRestrictedForCtx computes the RFC AX restriction bit from the LIVE
// principal on ctx and the deployment gate — the run-start path for surfaces
// that carry an auth.Principal (HTTP, and gRPC/MCP/connector via applyPrincipal).
// Fail-open: no principal (open mode) / legacy / gate-off / scope-present all
// yield false (operator key allowed). See auth.OperatorKeyRestricted.
func (s *Server) operatorKeyRestrictedForCtx(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return auth.OperatorKeyRestricted(p, ok, s.cfg().Env.OperatorKeyRestriction)
}

// isolatedForCtx computes the RFC BX P2b isolation bit from the LIVE principal on
// ctx — the run-start path for surfaces that carry an auth.Principal (HTTP, and
// gRPC/MCP/connector via applyPrincipal). Server-derived only (never a wire/body/
// model field). Fail-open: no principal (open mode) / a principal whose authority
// is tenant/admin all yield false (unconfined). See auth.IsIsolated. Sibling of
// operatorKeyRestrictedForCtx; unlike that one there is NO deployment gate —
// substrate:user is always enforced when present.
func (s *Server) isolatedForCtx(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return auth.IsIsolated(p, ok)
}

// isolatedCrossUser reports whether the ctx principal is an isolated member
// reaching for a DIFFERENT user's per-user surface (a `/v1/users/{user_id}/…`
// route) — i.e. it should be confined to itself. RFC BX P2b: an isolated
// member sees only its own runs/sessions/metadata; a non-isolated principal
// keeps the whole-tenant view (returns false). Handlers use this to return an
// empty/own-only result rather than another user's data.
func (s *Server) isolatedCrossUser(ctx context.Context, pathUserID string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return auth.IsIsolated(p, ok) && pathUserID != p.Subject
}

// isolatedOrCaptured is the RunOnce variant (sibling of
// operatorKeyRestrictedOrCaptured): use the live principal when one is on ctx
// (gRPC/MCP/connector), else fall back to the bit CAPTURED on a trigger def by a
// non-principal path (scheduler/webhook/A2A supply it via RunInput.Isolated).
// RFC BX P2b anti-bypass: an isolated member's captured confinement must ride the
// trigger def since no token is present at fire time.
func (s *Server) isolatedOrCaptured(ctx context.Context, captured bool) bool {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return auth.IsIsolated(p, ok)
	}
	return captured
}

// operatorKeyRestrictedOrCaptured is the RunOnce variant: use the live principal
// when one is on ctx (gRPC/MCP/connector), else fall back to the bit CAPTURED on
// a trigger def by a non-principal path (scheduler/webhook/A2A supply it via
// RunInput.OperatorKeyRestricted). RFC AX §2 anti-bypass: a restricted tenant's
// captured grant must ride the trigger def since no token is present at fire time.
func (s *Server) operatorKeyRestrictedOrCaptured(ctx context.Context, captured bool) bool {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return auth.OperatorKeyRestricted(p, ok, s.cfg().Env.OperatorKeyRestriction)
	}
	return captured
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
	// RFC BX P2b: an ISOLATED member is confined to its OWN sessions — the
	// tenant match alone would let it read/continue another user's session in
	// the same tenant (and, via a continuation, that user's user-scoped data,
	// since the run inherits sess.UserID). Every non-isolated principal keeps
	// the whole-tenant collaboration model (session_ids are not secret, so the
	// tenant is the boundary there).
	if auth.IsIsolated(p, ok) {
		return sess.TenantID == p.TenantID && sess.UserID == p.Subject
	}
	return sess.TenantID == p.TenantID
}

// runOwnershipOK is sessionOwnershipOK for a run: whether the ctx principal
// may read the run's content — its prompt snapshot, result, spec or draft. The
// tenant accessor alone admits an isolated member to another user's run in the
// same tenant; every read that returns run content applies this on top of it
// and answers a false with its own opaque not-found.
func runOwnershipOK(ctx context.Context, run store.Run) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return auth.OwnedRowVisible(p, ok, run.TenantID, run.UserID)
}

// allowlistReachesRun reports whether the caller's agent allowlist reaches the
// run (auth.RunTree says how). A token limited to certain agents may only
// START those, so steering, reviewing, retuning, compacting, cancelling or
// answering a run of any other agent in its tenant would hand it the control
// of runs it could never have started. Every run-control gate applies this on
// top of its ownership rule and folds a false into the same opaque not-found.
func (s *Server) allowlistReachesRun(ctx context.Context, runID string) bool {
	return s.runTree(ctx).Reaches(runID)
}

// allowlistReachesRow is allowlistReachesRun for a run row the caller has
// already read: the walk starts from the row instead of reading it again.
// Every read that returns a run's content or metadata applies it on top of
// its ownership rule and answers a false with its own opaque not-found.
func (s *Server) allowlistReachesRow(ctx context.Context, run store.Run) bool {
	return s.runTree(ctx).ReachesKnown(runLink(run))
}

// allowlistReachesSession reports whether the caller's agent allowlist
// reaches a session: whether it reaches every run in it (auth.RunTree's
// ReachesSession). A session read hands over the history of those runs, so a
// token limited to certain agents must not read a session of another agent,
// nor one a sub-run opened inside another agent's tree. Same posture as
// allowlistReachesRun: true with no read for an unrestricted caller, fails
// closed, and every gate folds a false into its opaque not-found.
func (s *Server) allowlistReachesSession(ctx context.Context, sess store.Session) bool {
	tree := s.runTree(ctx)
	if !tree.Restricted() {
		return true
	}
	if s.store == nil {
		return false
	}
	return tree.ReachesSession(sess.Agent, func() ([]auth.RunLink, error) {
		runs, err := s.store.RunsForSession(ctx, sess.ID)
		if err != nil {
			return nil, err
		}
		links := make([]auth.RunLink, 0, len(runs))
		for _, r := range runs {
			links = append(links, runLink(r))
		}
		return links, nil
	})
}

// runTree is the allowlist walker for the principal on ctx, reading runs from
// the store. Without a store it reads nothing, so a restricted caller is
// refused (fail closed) and every other caller is unaffected.
func (s *Server) runTree(ctx context.Context) *auth.RunTree {
	p, ok := auth.PrincipalFromContext(ctx)
	if s.store == nil {
		return auth.NewRunTree(p, ok, nil)
	}
	return auth.NewRunTree(p, ok, func(runID string) (auth.RunLink, error) {
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return auth.RunLink{}, err
		}
		return runLink(run), nil
	})
}

// runLink is what an allowlist walk reads of a run row.
func runLink(run store.Run) auth.RunLink {
	return auth.RunLink{ID: run.ID, Agent: run.Agent, ParentRunID: run.ParentRunID}
}

// continuationReachesSession reports whether the caller's agent allowlist
// reaches session sess, for a continuation: its agent is on the list, and so
// is every run of it — a top-level run's own agent, and for a run another run
// spawned, the root of its tree (allowlistReachesRun). A sub-agent's session
// under a root outside the list is not reached, though the sub-agent itself
// is on it: continuing replays the session's whole history to the model, so
// the continuation would read what the token may not. A caller with no
// allowlist reaches every session, with no read. Fails closed: a run whose
// tree cannot be read does not reach. The session read routes apply the same
// rule through their own, more general check.
func (s *Server) continuationReachesSession(ctx context.Context, sess store.Session) (bool, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || !p.Restrictions.RunPlaneOnly() {
		return true, nil
	}
	if !p.Restrictions.AgentAllowed(sess.Agent) {
		return false, nil
	}
	if s.store == nil {
		return false, nil
	}
	runs, err := s.store.RunsForSession(ctx, sess.ID)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.ParentRunID == "" && run.ParentAgentID == "" {
			if !p.Restrictions.AgentAllowed(run.Agent) {
				return false, nil
			}
			continue
		}
		if !s.allowlistReachesRun(ctx, run.ID) {
			return false, nil
		}
	}
	return true, nil
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
			"capabilities": s.serverCapabilities(),
			// The rollout mode is the deployment's, not the principal's: an
			// open-mode console shows it like any other.
			"acl_mode":  string(s.aclMode()),
			"effective": openModeEffective(),
			"auth":      s.whoamiAuth(r.Context(), true),
		})
		return
	}
	out := map[string]any{
		"tenant_id":    p.TenantID,
		"subject":      p.Subject,
		"scopes":       p.Scopes,
		"is_admin":     auth.HasScope(p.Scopes, auth.ScopeAdmin),
		"legacy":       p.Legacy,
		"capabilities": s.serverCapabilities(),
	}
	if !p.Restrictions.IsZero() {
		out["restrictions"] = p.Restrictions
	}
	// RFC EY §8: whether scopes above are the token's own (frozen) or its
	// user's current access_mode (inherit), so a UI can say a promotion
	// needs no new token.
	out["authority"] = p.AuthorityMode()
	// RFC EY: the principal's groups and the rollout mode, so a UI can show
	// why a surface is missing; the compiled set itself is not serialised.
	out["acl_mode"] = string(s.aclMode())
	if p.Groups != nil {
		out["groups"] = p.Groups
	}
	if p.Effective != nil {
		out["acl_version"] = p.Effective.Version
	}
	// RFC EZ §4: which Web UI pages the principal may reach, asked of the
	// gates a request to each page's primary route would face.
	out["effective"] = navEffective(r.Context(), p)
	// RFC FA: how the caller signed in — the mode always, the gate, methods
	// and session times when a Web UI session authenticated the request.
	out["auth"] = s.whoamiAuth(r.Context(), false)
	writeJSONOK(w, out)
}

// serverCapabilities is a NON-SECRET, booleans-only advertisement of runtime
// posture the Web UI needs to gate its affordances (RFC AU) — notably whether a
// tenant may import a stdio MCP server (host RCE, off by default). It is exposed
// on the auth-only /v1/_me boot call to every principal, so it must never leak
// operator infra detail: report only booleans/presence, NEVER the allowlist
// CONTENTS (internal hostnames) or any secret.
func (s *Server) serverCapabilities() map[string]any {
	return map[string]any{
		// Whether the MCPServerDef substrate accepts a dynamically-registered
		// stdio server (LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO). The UI enables the
		// stdio import path only when true, with a loud RCE warning.
		"mcp_allow_dynamic_stdio": s.cfg().Env.MCPAllowDynamicStdio,
		// Whether ANY http host allowlist is configured — so the UI can warn
		// that an imported http MCP server will 422 unless its host is listed.
		// Presence only; never the entries themselves.
		"http_host_allowlist_configured": len(s.cfg().Env.HTTPHostAllowlist)+len(s.cfg().Env.HTTPPrivateHostAllowlist) > 0,
	}
}

// THREE HELPERS RESOLVE A TENANT, AND THEY DISAGREE ON PURPOSE.
//
// The disagreement is irreducible, so it is written down here once instead of
// being rediscovered from three call sites:
//
//   - principalTenantScope — a LIST read, which can mean "every tenant". An
//     admin naming none gets all=true and sees the whole deployment.
//   - adminFocusedTenant — a SINGLE-TUPLE read or write, which must name exactly
//     one tenant. "All" is not expressible, so an admin naming none keeps its
//     own tenant, and only an explicit ?tenant= widens.
//   - substrateBrowseCtxFn — the same single-tuple case for the Path/Document
//     data plane, which additionally carries a subject (?scope_id=).
//
// What they must NEVER disagree on, and do not: a non-admin's wire value is
// ignored rather than honoured-then-checked, so no tenant can widen its own
// scope on any of the three. TestTenantResolution_AdminDefaultsAreDeliberate
// pins every cell of that table, so a future change to one has to state whether
// it meant to change the family.
//
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
		slog.Warn("auth: tenant_scope_overridden", "wire", wireTenant, "principal", p.TenantID, "token_def", p.TokenDefID)
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

// adminFocusedTenant resolves the tenant for an operator console request that
// addresses ONE tenant's data, honouring a super-admin's explicit `?tenant=`
// focus and ignoring it for everyone else.
//
// It exists because admin was, on these surfaces, strictly LESS capable than a
// tenant operator. The memory browse handlers pinned the tenant with
// tenantFromCtx and took nothing from the URL, so a substrate:tenant token read
// its own tenant's memory while a substrate:admin token — which satisfies every
// scope gate — could only ever see its own, usually empty, tenant and had no way
// to name another. The sibling maintenance routes on the same store (embed
// stats, reembed, backfill, purge) had accepted `?tenant=` via
// principalTenantScope all along, so the inconsistency was inside one file.
//
// Only an EXPLICIT focus widens: with no `?tenant=` this returns exactly what
// tenantFromCtx returned, so every existing caller is byte-identical and the
// change adds a capability rather than moving a default. A non-admin's wire
// value is ignored, not honoured-then-checked, which is the same posture
// principalTenantScope takes — a tenant cannot widen its own scope.
func (s *Server) adminFocusedTenant(ctx context.Context, wireTenant string) string {
	wireTenant = strings.TrimSpace(wireTenant)
	if wireTenant == "" {
		return tenantFromCtx(ctx)
	}
	p, ok := auth.PrincipalFromContext(ctx)
	// Open mode and legacy are is_admin-equivalent, matching principalTenantScope.
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return wireTenant
	}
	if wireTenant != p.TenantID {
		slog.Info("auth: tenant_focus_ignored", "wire", wireTenant, "principal", p.TenantID, "token_def", p.TokenDefID)
	}
	return tenantFromCtx(ctx)
}

// tenantVisible reports whether the caller may read a row belonging to
// rowTenant. Super-admin (or open mode) sees all; a tenant principal sees
// only its own tenant. Single-row read handlers use this to 404 a
// cross-tenant probe (opaque — no existence oracle).
func (s *Server) tenantVisible(ctx context.Context, rowTenant string) bool {
	// Single source of truth for the visibility predicate: the same scope
	// resolution the tenant-scoped store accessor uses (tenant_store.go).
	tenantID, all := tenantScopeFromCtx(ctx)
	return all || rowTenant == tenantID
}

// requirePrincipalOwnsPathUser reports whether the ctx principal may act on the
// per-user surface for pathUserID. Used by the per-user channel routes, whose
// backing channel_messages carry NO tenant column — so the whole-tenant model
// (subjects within a tenant share its workspace) can't be enforced without the
// deferred tenant_id denormalisation. The safe no-schema mitigation is
// per-SUBJECT: a non-admin principal may act ONLY on its own subject. Admin /
// legacy / open mode are unrestricted (mirrors the exemptions of
// sessionOwnershipOK / tenantVisible).
func requirePrincipalOwnsPathUser(ctx context.Context, pathUserID string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return true
	}
	return pathUserID == p.Subject
}

// requiredScopeFor maps an HTTP (method, path) to the scope a caller
// must hold. Empty string = any authenticated principal (no specific
// scope). substrate:admin satisfies everything (see auth.HasScope), so
// the default admin/legacy token works on every route; narrow tokens
// get the specific gate. Conservative by design: ambiguous routes fall
// through to "" rather than risk locking a legitimate narrow token out.
func requiredScopeFor(method, path string) string {
	switch {
	// The raw model endpoints: the LLM gateway and the OpenAI-compatible
	// shims. Not an admin surface, but not open to every principal either:
	// see auth.ScopeLLMGateway. authMiddleware admits a non-isolated member
	// without the scope (gatewayMemberOK).
	case isGatewayRoute(path):
		return auth.ScopeLLMGateway
	// Whoami — any authenticated principal must be able to learn its own
	// identity (the Web UI's role source); a tenant token needs it too.
	case path == "/v1/_me":
		return ""
	// The /v1/_users collection, method-aware (RFC BX P2a):
	//   - GET (list) — any authenticated principal; the handler tenant-scopes
	//     the result (a tenant sees only its tenant's users; admin sees all /
	//     can focus via ?tenant=). The UI's per-tenant workspace picker needs it.
	//   - POST (create a first-class user) — a tenant-operator action; only a
	//     substrate:tenant operator (or admin, which also satisfies) may
	//     register a user. The handler stamps the authoritative principal tenant.
	// PATCH/DELETE /v1/_users/{subject} (update / delete) fall through to the
	// strings.HasPrefix(path, "/v1/_users/") case below, which already returns
	// ScopeTenant — so all three mutating ops are tenant-operator-gated.
	case path == "/v1/_users":
		if method == http.MethodPost {
			return auth.ScopeTenant
		}
		return ""
	// RFC BY: user-facing READ surfaces — a delegated user's OWN chat history
	// (/v1/_history) and the runnable-agent catalog (/v1/_runnable-agents).
	// Gated member-read (runs:read) so a delegated user token reaches them —
	// isolated users hold substrate:user (→ runs:read via userImplied) and
	// tenant-mode users hold runs:read directly; tenant operators + admin also
	// satisfy runs:read (see auth.HasScope). The HANDLERS do the real
	// confinement: history is capped to [self, user] for a member
	// (substrateAdminCtx), and discovery is tiered by the caller's
	// users.access_mode. Keying on substrate:tenant would lock users out; keying
	// on substrate:user would lock TENANT-MODE users out (they hold neither).
	case memberReadable(path):
		return auth.ScopeRunsRead
	// Evaluation outside a run. runs:read is the floor every principal that may
	// read a run holds — an isolated user included, which is why it is not
	// substrate:tenant. The gate admits; it grants nothing: which ops the caller
	// may use is tools.OffRunEvaluationPolicy (every op for a tenant operator or
	// admin, submit_self for anyone else), the same grant as the Evaluation RPC
	// and the MCP `evaluation` tool, which gate on runs:read too.
	case path == "/v1/_evaluation":
		return auth.ScopeRunsRead
	// RFC BC: the client-tool host WebSocket. A client registers tools it runs on
	// the user's own machine; the connection serves ONLY its own principal's runs
	// (filed under (tenant, subject) from the bearer). Requires runs:create — you
	// must be able to run agents to benefit, and it mirrors the steer /input gate.
	// ScopeAdmin also satisfies it.
	case path == "/v1/client-tools":
		return auth.ScopeRunsCreate
	// RFC AF: the tenant-confined substrate plane — def-authoring across the 8
	// xxxDef families, INCLUDING /v1/_mcpserverdef (the "dynamic MCP tools
	// ingestion" surface: a tenant registers an external MCP server and the
	// runtime mounts its tools for the tenant's agents). ScopeTenant (ScopeAdmin
	// also satisfies it). The handlers stamp the principal's authoritative tenant
	// (dispatchSubstrate → RunIdentity) and 404 cross-tenant reads, so a tenant
	// operator authors ONLY its own surface. DELIBERATELY EXCLUDES
	// /v1/_operatortokendef (token minting — no tenant dimension, stays ScopeAdmin
	// via the catch-all below). /v1/_mcp (the loomcycle-as-MCP-server transport)
	// now has its OWN ScopeTenant case below: RFC AG made it per-principal
	// (mcpPrincipalCtx stamps the tenant + a per-tool gate withholds the admin
	// meta-tools), so it no longer needs to be operator-only.
	case isTenantConfinedDefPath(path):
		return auth.ScopeTenant
	// RFC EO §5.5: which per-run overrides a run of one agent may set. The
	// def plane's posture — a tenant operator, and through the member path a
	// non-isolated member, who could read the whole definition through
	// POST /v1/_agentdef anyway. Listed explicitly because the def family
	// above is exact-match, and the /v1/_* catch-all would pin it to admin.
	// A token limited to certain agents reaches it too: see runPlaneRoute.
	case isAgentOverridablePath(path):
		return auth.ScopeTenant
	// RFC AG Phase 2: the loomcycle-as-MCP-server HTTP transport. With Phase 0
	// (mcpPrincipalCtx tenant/user stamp + the per-tool tools/call gate +
	// tools/list filter) and Phase 1 (applyPrincipal on spawn) in place, the
	// transport is per-principal and tenant-confined — a substrate:tenant token
	// may OPEN an MCP session, and the per-tool gate INSIDE still withholds the
	// admin-only meta-tools (mint / runtime-admin / snapshots). The route gate
	// only decides "may open a session at all". ScopeAdmin also satisfies
	// ScopeTenant (auth.HasScope), so admin sessions are unchanged.
	case path == "/v1/_mcp":
		return auth.ScopeTenant
	// RFC AH Phase 4: the two read-only volume views (GET /v1/_volumes +
	// /v1/_volumes/ephemeral). Same tenant-confined posture as the def plane —
	// the handlers filter dynamic + ephemeral rows to the principal's
	// authoritative tenant (statics are the shared bind floor, shown to all).
	// ScopeTenant (ScopeAdmin also satisfies). Distinct from the
	// isTenantConfinedDefPath set because these are GETs at /v1/_volumes(/...),
	// not the /v1/_volumedef def-authoring route.
	case path == "/v1/_volumes" || path == "/v1/_volumes/ephemeral":
		return auth.ScopeTenant
	// RFC AS Phase 1: the unified Library list views (agents / skills /
	// mcp-servers). #575 tenant-scoped the HANDLERS (a substrate:tenant
	// principal sees only its own tenant's substrate rows; operator-global
	// statics excluded), but the routes stayed at the /v1/_* ScopeAdmin
	// catch-all below — so a tenant token was 403'd at the gate before the
	// scoped handler could run, leaving #575's tenant branch unreachable
	// (and the Library invisible to a tenant operator). Grant ScopeTenant
	// here so the reads are actually reachable; the handler still confines
	// the result to the caller's tenant (ScopeAdmin also satisfies, so admin
	// + ?tenant= focus is unchanged). GET-only views — the def-AUTHORING
	// writes live at /v1/_agentdef etc. (isTenantConfinedDefPath, already
	// ScopeTenant), not at this path. Mirrors the /v1/_*def/names posture.
	case path == "/v1/_library/agents" || path == "/v1/_library/skills" || path == "/v1/_library/mcp-servers":
		return auth.ScopeTenant
	// The tenant ontology surface (GET state + POST the draft↔confirmed flip).
	// ScopeTenant on BOTH methods: the ontology is one document per tenant, the
	// handler derives that tenant from the principal (never the wire), and the same
	// token can already write the same root-chunk status through /v1/_document
	// update_chunk — so gating the write harder here would deny the intended actor
	// without closing anything. ScopeAdmin also satisfies it (+ ?tenant= focus).
	// RFC CA: /proposals (accept/reject) and /adopt sit with it. Same reasoning — one
	// document per tenant, the tenant derived from the principal and never the wire —
	// and both are strictly NARROWER than what this token can already do through
	// /v1/_document, which is the point of having them as dedicated ops.
	case path == "/v1/_ontology" ||
		path == "/v1/_ontology/proposals" || path == "/v1/_ontology/adopt":
		return auth.ScopeTenant
	// RFC AS: the Web UI schedules surface (list-all + per-def state / run-now /
	// pause / resume). list-all is tenant-scoped (the handler filters substrate
	// schedules to the caller's tenant; operator-global static crons stay
	// admin-only); the per-def ops confine a substrate:tenant principal to its
	// OWN tenant's schedule defs (opaque-404 cross-tenant + statics, which have
	// no ScheduleDef row). ScopeAdmin also satisfies. The def-AUTHORING writes
	// live at /v1/_scheduledef (isTenantConfinedDefPath, already ScopeTenant).
	case strings.HasPrefix(path, "/v1/_schedules/"):
		return auth.ScopeTenant
	// RFC AS: the audit/event log. events carry no tenant column, but each is
	// tied to a session (events.session_id NOT NULL) whose tenant_id is the
	// event's tenant — so handleListEvents tenant-scopes the result (a tenant
	// sees only its own tenant's events; admin sees all + the ?tenant= focus).
	// ScopeAdmin also satisfies. Read-only GET.
	case path == "/v1/_events":
		return auth.ScopeTenant
	// RFC AV: the usage/cost report. token_usage rows carry tenant_id, so
	// handleUsageReport tenant-scopes the aggregation (a tenant operator sees
	// only its own tenant's spend; admin sees all + the ?tenant= focus).
	// ScopeAdmin also satisfies. Read-only GET.
	case path == "/v1/_usage":
		return auth.ScopeTenant
	// The per-subject erasure report (GET). Tenant-readable, and deliberately so:
	// answering "what do you hold about this person" is the tenant OPERATOR's job
	// — they are the controller for their own tenant's subjects — and requiring
	// operator-admin would put a routine data-subject request behind a token that
	// can read every OTHER tenant too. principalTenantScope confines the walk to
	// the caller's tenant; admin additionally gets the ?tenant= focus.
	//
	// Not narrower than ScopeTenant despite reading across scopes: everything it
	// counts is already listable by this principal through /v1/_usage,
	// /v1/_credentialdef and the Path/Memory surfaces. It aggregates a view that
	// is reachable today, rather than granting new reach.
	//
	// POST (the erasure itself) shares the gate for the same reason — executing a
	// data-subject request is the same operator's job as answering one, and every
	// plane it deletes is already deletable by this principal one call at a time.
	// The protection against an accidental erasure is NOT the scope (an admin
	// token would be no less able to do it): it is dry_run defaulting true via a
	// *bool, and confirm having to equal subject on a live run.
	// The per-subject aggregate view. ScopeTenant like the /v1/_users list it drills
	// into: everything it aggregates is already readable by this principal through
	// /v1/_usage, /v1/_limits and the memory surfaces, so it saves five calls
	// without granting new reach. principalTenantScope confines it.
	case strings.HasPrefix(path, "/v1/_users/"):
		return auth.ScopeTenant
	// RFC FA P2: the password gate's management API (invites, resets,
	// credentials, logins) is the tenant operator's for its own tenant; admin
	// for any. An auth:manage credential is admitted by authManageRoute
	// before this switch. The handlers confine the tenant.
	case strings.HasPrefix(path, authGateRoutePrefix):
		return auth.ScopeTenant
	// RFC EY P4: groups, ACL rule lists and bindings are the tenant
	// operator's (admin focuses another tenant with ?tenant=); members are
	// carved out below. The evaluate / explain / simulate questions are
	// open to any authenticated principal: the handler confines a
	// non-operator to its OWN subject's decisions.
	case memberpath.IsACLManagementPath(path):
		return auth.ScopeTenant
	case strings.HasPrefix(path, "/v1/_authz/"):
		return ""
	// RFC FA P2: a signed-in person's own account (password, authenticator).
	// Any principal reaches the handler, which serves only a password-gate
	// Web UI session and re-authenticates every change.
	case strings.HasPrefix(path, "/v1/_auth/"):
		return ""
	// Tenant enumeration. ScopeAdmin, and the HANDLER re-checks: the counts are
	// unremarkable but the LIST ITSELF is cross-tenant information, which is
	// exactly what a tenant-confined principal must not learn.
	case path == "/v1/_tenants":
		return auth.ScopeAdmin
	case path == "/v1/_erasure":
		return auth.ScopeTenant
	// RFC BM: the data-retention view. Tenant-readable so a tenant operator's UI
	// can see whether retention is enabled + how it's tuned; the HANDLER strips
	// the cross-tenant purgeable counts + the export dir for a non-admin caller
	// (admin gets the full view). ScopeAdmin also satisfies. Read-only GET.
	case path == "/v1/_retention", path == "/v1/_retention/overrides":
		return auth.ScopeTenant
	// RFC AW: the token-budget management surface (GET list + PUT upsert + DELETE).
	// Tenant-readable/writable so a tenant operator manages its own tenant + user
	// budgets; the handler enforces the operator-global + cross-tenant admin
	// restriction (a scoped caller writing the operator scope or a foreign tenant
	// gets 403). ScopeAdmin also satisfies. Mirrors /v1/_usage's posture.
	case path == "/v1/_limits":
		return auth.ScopeTenant
	// Routing view (GET /v1/_routing). Tenant-readable so a tenant operator's UI
	// can see the resolved model cascade per tier; the HANDLER strips the live
	// provider reachability / infra detail for a non-admin caller (admin gets the
	// full availability view). ScopeAdmin also satisfies. Read-only GET.
	case path == "/v1/_routing":
		return auth.ScopeTenant
	// Configured model aliases (GET /v1/_models) — non-secret global config
	// (provider + model names). Tenant-readable so a tenant operator's UI can
	// offer aliases in a model picker + store the alias on a fork (so it tracks
	// the operator's local override). Not tenant-scoped data (the alias map is
	// global), so every authed caller sees the same set; ScopeAdmin also
	// satisfies. Read-only GET.
	case path == "/v1/_models":
		return auth.ScopeTenant
	// RFC BK P3 — resident interactive sub-agent visibility + operator control.
	// GET /v1/_resident lists (tenant-scoped in the handler); POST
	// /v1/_resident/{run_id}/close|cancel act on one (tenant-gated in the handler,
	// opaque-404 cross-tenant). Tenant-readable/actionable so a tenant operator
	// can see + reap its own runs' resident children; ScopeAdmin also satisfies.
	case path == "/v1/_resident" || strings.HasPrefix(path, "/v1/_resident/"):
		return auth.ScopeTenant
	// repair-tenant rewrites memory rows across EVERY scope in one statement to
	// re-stamp the legacy "" partition — a cross-tenant bulk rewrite that is not a
	// tenant operator's authority even over its own tenant. It STAYS operator-admin.
	// MUST precede the memory-family case below, which it would otherwise match on
	// the /v1/_memory/ prefix.
	case path == "/v1/_memory/repair-tenant":
		return auth.ScopeAdmin
	// The memory browse / search / embed-admin family (RFC BV). memory rows carry a
	// tenant_id (RFC BL added the tenant axis), and every handler already sources
	// the tenant from the authenticated principal (tenantFromCtx /
	// principalTenantScope), never the wire — so a substrate:tenant operator is
	// confined to its own tenant's memory and admin keeps its view. The routes were
	// pinned to ScopeAdmin by the /v1/_* catch-all below, so a tenant token 403'd
	// before the tenant-scoped handler ran (the same gap #575/#577 fixed for the
	// Library). Grant ScopeTenant so the reads/writes are reachable; the handlers
	// still confine. ScopeAdmin also satisfies. Note /v1/_memorybackenddef (a def
	// family, already ScopeTenant via isTenantConfinedDefPath) does NOT match this
	// prefix — it has no slash after "memory".
	case strings.HasPrefix(path, "/v1/_memory/"):
		return auth.ScopeTenant
	// The channel admin family (list/create/patch/delete/purge/publish/
	// subscribe/peek/ack/_await/_broadcast). channel_messages/_cursors/
	// the channels def table now carry a tenant_id (migration 0066), and
	// every handler sources the tenant from the authenticated principal
	// (tenantFromCtx / principalTenantScope), never the wire — so a
	// substrate:tenant operator is confined to its own tenant's channels
	// (list filters by tenant; the `global` cross-tenant scope stays
	// admin-only to create). The routes were pinned to ScopeAdmin by the
	// /v1/_* catch-all below; grant ScopeTenant so they're reachable, the
	// handlers still confine. ScopeAdmin also satisfies. (Matches the
	// RFC BV memory re-gate above; closes the RFC AS channels carve-out.)
	case strings.HasPrefix(path, "/v1/_channels"):
		return auth.ScopeTenant
	// A decision asked outside a run, and the list of models it may name. The
	// scope that creates a run: a decision is what a caller would otherwise
	// start a run to get, and it spends the same budget. Listed here because
	// the /v1/_* catch-all below would pin both to ScopeAdmin, and a tenant
	// token would be refused before the handler ran.
	case path == "/v1/_decide" || path == "/v1/_decide/models":
		return auth.ScopeRunsCreate
	// Everything else under /v1/_* is OPERATOR-admin: token minting
	// (_operatortokendef), runtime admin (pause/resume/state/snapshots/metrics),
	// resolver, cross-tenant user focus.
	case strings.HasPrefix(path, "/v1/_"):
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
	// RFC Y fan-out spawns N runs in one call — same create scope as /v1/runs.
	// Exact match (not the /v1/runs/ prefix), so it's not shadowed by the
	// per-run write cases below.
	case method == http.MethodPost && path == "/v1/runs:batch":
		return auth.ScopeRunsCreate
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/sessions/") && strings.HasSuffix(path, "/messages"):
		return auth.ScopeRunsCreate
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/sessions/") && strings.HasSuffix(path, "/replay"):
		// RFC BJ Phase 4 — mints a new run (seed) under a target agent.
		return auth.ScopeRunsCreate
	// Cancel a run — a write on run state. (The real route is POST
	// /v1/agents/{id}/cancel; the prior DELETE /v1/agents/ case matched no
	// registered route, so cancel fell through to any-authenticated.)
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/agents/") && strings.HasSuffix(path, "/cancel"):
		return auth.ScopeRunsCreate
	// Human-in-the-loop interrupt: resolve is a run-state write, list a read.
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/resolve"):
		return auth.ScopeRunsCreate
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/compact"):
		return auth.ScopeRunsCreate
	// Retune: a run-state write, same scope as steer / resolve / compact.
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/retune"):
		return auth.ScopeRunsCreate
	// Reading a run's own configuration is a READ, not a mutation.
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/config"):
		return auth.ScopeRunsRead
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/effective-config"):
		return auth.ScopeRunsRead
	// RFC BH turn-cancel: stop the current turn of an interactive run + park it —
	// a run-state mutation, same scope as steer/resolve/compact. Distinct path
	// from whole-run cancel (POST /v1/agents/{id}/cancel). Without this case the
	// POST /v1/runs/.../cancel would default-deny as ScopeAdmin below.
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/cancel"):
		return auth.ScopeRunsCreate
	// Operator steering / continuation input is a run-state MUTATION (it injects
	// text into a live run), so it requires runs:create like cancel/resolve/
	// compact — NOT the read scope. (exp7 I1: it previously fell through to the
	// default empty arm, letting a read-only bearer steer a run.)
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/input"):
		return auth.ScopeRunsCreate
	// A review verdict decides whether a held run's answer stands — a run
	// mutation like steering, at the same scope.
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/review"):
		return auth.ScopeRunsCreate
	// RFC DI D5: starting, editing and discarding a configured run are run
	// writes, the same scope that creates one. PATCH / DELETE address the run
	// itself (/v1/runs/{run_id}, no further segment).
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/start"):
		return auth.ScopeRunsCreate
	// Deleting a run tree (DELETE /v1/runs/{run_id}/tree) or a session with its
	// runs' descendants (DELETE /v1/sessions/{id}) removes stored records, so it
	// has a scope of its own. It is NOT substrate:tenant on purpose: a route at
	// that scope is opened to every non-isolated member by
	// tenantMemberAccessible, and a member must not delete run records unless
	// the scope was granted to its token. The handler additionally requires
	// read access to the root.
	case isRunTreeDeletePath(method, path):
		return auth.ScopeRunsDelete
	case (method == http.MethodPatch || method == http.MethodDelete) && strings.HasPrefix(path, "/v1/runs/") &&
		!strings.Contains(strings.TrimPrefix(path, "/v1/runs/"), "/"):
		return auth.ScopeRunsCreate
	// Arming a team walk's breakpoints holds work back and releases it — a
	// run-state mutation, same scope as steer/cancel/compact. Without this case
	// the PUT falls through to the default-deny admin arm, which would lock a
	// tenant operator out of debugging their own run. (The GET is covered by
	// the runs-read case below.)
	case method == http.MethodPut && strings.HasPrefix(path, "/v1/runs/") && strings.HasSuffix(path, "/breakpoints"):
		return auth.ScopeRunsCreate
	// The File twin and the inbox download read a run; the twin refuses its
	// writing ops to all but the run's owner or an admin itself, as the MCP
	// twin (runs:read too) does. Before the GET case below so HEAD and the
	// POST land here.
	case isRunFileRoute(method, path):
		return auth.ScopeRunsRead
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/runs/"):
		return auth.ScopeRunsRead
	// Listing a team walk's runs. Exact match: without it the bare path falls
	// to the any-authenticated GET default.
	case method == http.MethodGet && path == "/v1/runs":
		return auth.ScopeRunsRead
	// Run / agent / session / user reads.
	case method == http.MethodGet && (strings.HasPrefix(path, "/v1/agents/") ||
		strings.HasPrefix(path, "/v1/users/") ||
		strings.HasPrefix(path, "/v1/sessions/")):
		return auth.ScopeRunsRead
	default:
		// Default-deny for unlisted MUTATING routes (v0.34.0 security review
		// S2). The prior `return ""` shipped any unlisted route at
		// any-authenticated — so a new state-changing endpoint added without a
		// requiredScopeFor entry was silently reachable by a minimal-scope
		// tenant token. A mutating method now requires ScopeAdmin by default;
		// ScopeAdmin still satisfies every check (auth.HasScope), so the
		// default/legacy admin token is unaffected and the safe failure
		// direction for a forgotten route is over-restrict (a narrow token gets
		// 403), never silent exposure. GET/HEAD reads keep the any-authenticated
		// default — reads are tenant-gated per-handler (tenantVisible /
		// sessionOwnershipOK), and the consumer LLM-gateway POSTs that are
		// legitimately any-authenticated are explicit `return ""` cases above.
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			return auth.ScopeAdmin
		}
		return ""
	}
}

// isRunTreeDeletePath matches the two run-tree delete routes exactly:
// DELETE /v1/runs/{run_id}/tree and DELETE /v1/sessions/{id}. Exact, so the
// draft discard (DELETE /v1/runs/{run_id}) keeps its own scope even for a run
// whose id is the word "tree".
func isRunTreeDeletePath(method, path string) bool {
	if method != http.MethodDelete {
		return false
	}
	if rest, ok := strings.CutPrefix(path, "/v1/runs/"); ok {
		id, ok := strings.CutSuffix(rest, "/tree")
		return ok && id != "" && !strings.Contains(id, "/")
	}
	if id, ok := strings.CutPrefix(path, "/v1/sessions/"); ok {
		return id != "" && !strings.Contains(id, "/")
	}
	return false
}

// tenantMemberAccessible reports whether a NON-ISOLATED user token (a "tenant
// member" — RFC CB) may reach this route despite lacking substrate:tenant. A
// member already has whole-tenant DATA access inside a run (it is not isolated;
// auth/grants.go: "whole-tenant data access is conferred by NOT being isolated"),
// so this opens the matching HTTP tenant surfaces for read+write. The rule: any
// route that requires ScopeTenant, MINUS the operator-control carve-outs
// (memberpath.CarvedOut). Routes already at ScopeAdmin (cross-tenant enumeration,
// runtime admin, token minting via the /v1/_* catch-all) are excluded
// automatically — they are not ScopeTenant. The middleware still requires the
// caller to be non-isolated (auth.IsIsolated == false), and every one of these
// handlers still confines a member to its own tenant (principalTenantScope /
// tenantFromCtx), so opening the gate here does not widen a member cross-tenant.
func tenantMemberAccessible(method, path string) bool {
	return memberpath.Route(requiredScopeFor(method, path), method, path)
}

// userSelfServiceRoute marks the HTTP routes RFC CN opens for user SELF-SERVICE of
// scope=user credentials, as an ADDITIONAL authMiddleware admission path that also
// lets an isolated substrate:user user through (it otherwise 403s at the tenant-
// member isolation floor):
//
//   - POST /v1/_credentialdef — the credential store; handleSubstrateCredentialDef
//     confines an isolated caller to scope=user on its own subject (P1).
//   - POST + DELETE /v1/_mcp — the loomcycle-as-MCP-server transport, so the same
//     user can manage its tokens from the `loomcycle mcp --upstream` thin client.
//     The MCP per-tool gate (principalMayCallTool) restricts an isolated session to
//     ONLY the credentialdef meta-tool, and that dispatch applies the same scope=user
//     constraint — so admitting the session grants nothing beyond a user's own tokens.
//
// The routes stay nominally ScopeTenant, so a tenant operator's full tenant/agent
// authoring is unchanged and members reach them via the RFC CB member path; this is
// purely additive for isolated users.
func userSelfServiceRoute(method, path string) bool {
	switch path {
	case "/v1/_credentialdef":
		return method == http.MethodPost
	case "/v1/_mcp":
		return method == http.MethodPost || method == http.MethodDelete
	}
	return false
}

// authManageRoute is the whole reach of an auth:manage credential (RFC FA
// D16): the password gate's management API, and the user roster's list,
// create, update and delete. Not GET /v1/_users/{subject} (the per-subject
// view aggregates usage, limits and memory), not the token routes under it,
// not whoami. A deny-by-default allowlist: a route added later is closed to
// it until it is listed here.
func authManageRoute(method, path string) bool {
	if strings.HasPrefix(path, authGateRoutePrefix) {
		return true
	}
	if path == "/v1/_users" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if rest, ok := strings.CutPrefix(path, "/v1/_users/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return method == http.MethodPatch || method == http.MethodDelete
	}
	// The approval of a pending user (RFC FA §6.1.1: floor users only, which
	// the handler enforces).
	if rest, ok := strings.CutPrefix(path, "/v1/_users/"); ok && method == http.MethodPost {
		if sub, ok := strings.CutSuffix(rest, "/approve"); ok && sub != "" && !strings.Contains(sub, "/") {
			return true
		}
	}
	return false
}

// authGateRoutePrefix is the password gate's management API.
const authGateRoutePrefix = "/v1/_authgate/"

// isTenantConfinedDefPath matches the substrate def-authoring families that
// RFC AF moves from operator-admin (ScopeAdmin) to the tenant-confined
// ScopeTenant. The original 8 families each have two routes — POST
// /v1/_<fam>def (all ops dispatch via the body's `op` field) and GET
// /v1/_<fam>def/names — so exact matching is complete (there are no
// /{name} sub-paths). The RFC AH Phase 2a VolumeDef family (added below) has
// only the POST route (its `list` op dispatches via the body, not a GET
// /names route). It DELIBERATELY EXCLUDES:
//   - /v1/_operatortokendef — token minting stays operator-only.
//   - /v1/_mcp — handled by its OWN ScopeTenant case in requiredScopeFor (RFC AG
//     Phase 2 made the MCP transport per-principal: mcpPrincipalCtx stamps the
//     tenant, a per-tool gate withholds the admin meta-tools). It is NOT in this
//     def-family helper because it is a transport, not a def family. Note
//     /v1/_mcpserverdef (a def family, in the loop below) IS the tenant-confined
//     "dynamic MCP tools ingestion" surface — it mounts an external MCP server's
//     tools for the tenant, and is tenant-stamped.
//   - runtime-admin / resolver / audit — keep ScopeAdmin via the /v1/_* catch-all.
//
// The def handlers confine a non-admin principal to its own tenant (write-stamp
// + opaque-404), so opening these gates to ScopeTenant doesn't widen reach.
// memberReadable reports whether path is a USER-FACING READ surface (RFC BY): a
// route any member of a tenant may read — an end-user (isolated OR tenant-mode),
// a tenant operator, or admin — with the HANDLER doing the real per-principal
// confinement (own subject only for history; tiered by access mode for
// discovery). requiredScopeFor gates these on runs:read, the honest floor every
// such principal holds (auth.HasScope): substrate:user implies it (userImplied),
// tenant-mode user tokens hold it directly, substrate:tenant implies it
// (tenantImplied), and admin/legacy satisfy everything. It deliberately is NOT
// substrate:tenant (which no delegated user holds) nor substrate:user (which a
// tenant-mode user does not hold).
func memberReadable(path string) bool {
	switch path {
	case "/v1/_history", "/v1/_runnable-agents":
		return true
	}
	return false
}

// isAgentOverridablePath reports whether path is
// /v1/_agentdef/{name}/overridable.
func isAgentOverridablePath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/_agentdef/")
	if !ok {
		return false
	}
	name, ok := strings.CutSuffix(rest, "/overridable")
	return ok && name != "" && !strings.Contains(name, "/")
}

// isTeamChannelsPath reports whether path is one of the team-channel routes:
// /v1/_teamdef/{team}/channels, or anything under it.
func isTeamChannelsPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/_teamdef/")
	if !ok {
		return false
	}
	_, after, ok := strings.Cut(rest, "/")
	return ok && (after == "channels" || strings.HasPrefix(after, "channels/"))
}

func isTenantConfinedDefPath(path string) bool {
	// RFC BO — the image-asset serving GET lives UNDER /v1/_document with a
	// variable {chunk_id} suffix, so it needs a prefix match (the family list
	// below is exact-match). Same ScopeTenant posture as /v1/_document itself.
	if strings.HasPrefix(path, "/v1/_document/asset/") {
		return true
	}
	// RFC CD Part C — the document change feed lives under /v1/_document with a
	// /changes suffix, so (like the asset GET) it needs a prefix match; the
	// family list below is exact-match. Same ScopeTenant posture as /v1/_document
	// (and it is carved out of member access — see memberpath.CarvedOut).
	if strings.HasPrefix(path, "/v1/_document/changes") {
		return true
	}
	// A team's own channels are read under the team, with a variable
	// {team} (and {name}) segment, so they need a prefix match too. Same
	// ScopeTenant posture as /v1/_teamdef: the handlers confine a caller to
	// teams its own tenant holds and answer another tenant's with not-found.
	if isTeamChannelsPath(path) {
		return true
	}
	for _, fam := range []string{
		"/v1/_agentdef", "/v1/_skilldef", "/v1/_teamdef", "/v1/_mcpserverdef", "/v1/_scheduledef",
		// HookDef — reusable hook definitions, authored like an AgentDef (a
		// tenant's operators and its non-isolated members); the tool stamps the
		// caller's tenant + opaque-404s cross-tenant.
		"/v1/_hookdef",
		"/v1/_webhookdef", "/v1/_memorybackenddef", "/v1/_a2aagentdef", "/v1/_a2aservercarddef",
		// RFC CE — the DocumentSourceDef substrate write-stamps the caller's
		// tenant + opaque-404s cross-tenant, exactly like _memorybackenddef, so
		// it joins the ScopeTenant set. Without this it fell through to the
		// /v1/_* ScopeAdmin catch-all, making the HTTP transport stricter than
		// the gRPC/MCP ones (both grant ScopeTenant) and leaving the
		// tenant-scoping in handleListDocumentSourceDefNames dead for tenant tokens.
		"/v1/_documentsourcedef",
		// RFC FL P0b — app clients: a tenant operator registers its own
		// tenant's, an admin any tenant's (the implementation confines the
		// tenant and opaque-404s another's). Carved out of member access.
		"/v1/_clientdef",
		// RFC AH Phase 2a — the dynamic-volume substrate is tenant-confined
		// (write-stamps the caller's tenant + opaque-404s cross-tenant), so
		// it joins the ScopeTenant set. No /names sub-path: the list op
		// dispatches via the POST body, not a GET route.
		"/v1/_volumedef",
		// RFC AL Path VFS + RFC AK Document — scope-aware, tenant-isolated
		// tools (resolve scope from the operator-trust ctx + tenant-stamp via
		// RunIdentity). Same posture as _volumedef: ScopeTenant, no /names.
		"/v1/_path", "/v1/_document",
		// RFC BE History tool was here (ScopeTenant); RFC BY moved it to the
		// member-read gate (memberReadable → runs:read) so a delegated USER token
		// can read its OWN chats. The handler caps a member to [self, user]
		// history scopes (substrateAdminCtx), so opening the gate does not widen a
		// user's reach beyond its own subject.
		// RFC AR secure credential store — scope-aware, tenant-isolated
		// (scope_id derived from the operator-trust ctx's authoritative
		// tenant/subject, never the wire; fail-closed without
		// LOOMCYCLE_SECRET_KEY). Same posture as _path/_document: ScopeTenant,
		// no /names route.
		"/v1/_credentialdef",
		// RFC EP P1b — Crypto operations outside a run, with the caller's tenant
		// and own-subject keys (s.Crypto stamps them from the principal).
		// Carved out of member access (memberpath.CarvedOut).
		"/v1/_crypto",
	} {
		if path == fam || path == fam+"/names" {
			return true
		}
	}
	return false
}
