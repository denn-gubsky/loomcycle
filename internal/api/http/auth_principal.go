package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/webui"
	"go.opentelemetry.io/otel/propagation"
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
			// "wrong" (no oracle). The server-side line is debug-level: one
			// per rejected request is noise at info.
			slog.DebugContext(r.Context(), "auth: rejected credential (no matching token or session / expired / wrong secret)")
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
		rt := requestRoute(r)
		if required, admitted := scopeGate(p, rt, r.Method, r.URL.Path); !admitted && !ownIdentityRequest(p, r) {
			writeScopeError(w, required, "insufficient scope")
			return
		}
		// After the scope check, and independent of it: a credential limited
		// to certain agents is held to the run plane however wide its scopes.
		if confinedOffRoute(p, rt, r.Method, r.URL.Path) {
			writeJSONError(w, http.StatusForbidden, "restricted_token",
				"this token is limited to starting and following runs of its allowed agents")
			return
		}
		// RFC EY: the route's type-level ACL question, after every legacy
		// gate admitted the request. The mode and transport ride the ctx so
		// the instance checks downstream (the run intakes, the MCP per-tool
		// gate behind /v1/_mcp) evaluate under the same rules.
		ctx := authz.WithTransport(authz.WithMode(r.Context(), s.ACLModeFor(p.TenantID)), requestTransport(r, ui != nil))
		// The client address a source_cidr condition reads (RFC FY Part E):
		// the peer, or the client a trusted proxy names for it.
		ctx = authz.WithSourceIP(ctx, net.ParseIP(s.clientSource(r)))
		r = r.WithContext(ctx)
		ctx, ok = s.aclGateHTTP(w, r, p)
		if !ok {
			return
		}
		// The caller's trace context, read only now that every gate admitted
		// the request: it decides whether a run is sampled and which trace it
		// joins, which an unauthenticated caller must not choose. Open mode
		// above authenticates nobody, so it reads none.
		ctx = lcotel.ExtractInbound(ctx, propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(ctx, p)))
	})
}

// requestTransport is the transport a `transport` condition reads for an HTTP
// request: `mcp` for the loomcycle-as-MCP-server endpoint, `webui` for a
// request a Web UI session cookie authenticated, else `http`.
func requestTransport(r *http.Request, cookie bool) string {
	switch {
	case r.URL.Path == "/v1/_mcp":
		return "mcp"
	case cookie:
		return "webui"
	}
	return "http"
}

// requestRoute is the row of the route the mux served r under (r.Pattern), so
// the gates judge the route whose handler will run, however the path is
// spelled. A request that reached a handler outside the mux (a test calling
// the middleware directly) is looked up by its method and path.
func requestRoute(r *http.Request) authz.Route {
	if rt, ok := authz.HTTPRouteByPattern(r.Pattern); ok {
		return rt
	}
	return routeAt(r.Method, r.URL.Path)
}

// routeAt is the row of the route that serves method and path. A request no
// route serves gets a row of its own: a write needs substrate:admin, a read
// nothing, nothing is on the run plane and no ACL governs it.
func routeAt(method, path string) authz.Route {
	if rt, ok := authz.HTTPRoute(method, path); ok {
		return rt
	}
	return authz.Route{Scope: authz.UnroutedScope(method)}
}

// scopeGate is the route's scope gate for an authenticated principal p: the
// scope route rt requires (its row in authz.HTTPRoutes), and whether p is
// admitted — by holding it, or on one of the paths below that admit a
// principal without it. It returns the scope so the refusal can name it.
func scopeGate(p auth.Principal, rt authz.Route, method, path string) (required string, admitted bool) {
	// RFC FA D16: a user-management credential reaches its route list and
	// nothing else — not even the routes any authenticated principal may
	// call. Checked first so no member, self-service or gateway path below
	// can admit it elsewhere.
	if auth.IsAuthManager(p) {
		return auth.ScopeAuthManage, authManageRoute(method, path)
	}
	// A metrics scraper's credential reaches the Prometheus scrape and
	// nothing else, by the same rule.
	if auth.IsMetricsReader(p) {
		return auth.ScopeMetricsRead, metricsScrapeRoute(method, path)
	}
	// A workspace worker's credential reaches no route at all: it attaches
	// to the worker endpoint, which is not on this mux (RFC GW Q2).
	if auth.IsWorkerAttacher(p) {
		return auth.ScopeWorkersAttach, false
	}
	// RFC FA P6: a SCIM credential reaches /scim/v2 and nothing else, and
	// /scim/v2 is reserved to it — an admin or tenant token is refused
	// there too: the SCIM tenant is the credential's own, and no other
	// principal is meant to provision through it.
	if auth.IsSCIMProvisioner(p) || scimRoute(path) {
		return auth.ScopeSCIMProvision, auth.IsSCIMProvisioner(p) && scimRoute(path)
	}
	required = rt.Scope
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
	memberOK := memberpath.Route(required, method, path) && !auth.IsIsolated(p, true)
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
	return routeAdmits(p, routeAt(method, path), method, path)
}

func routeAdmits(p auth.Principal, rt authz.Route, method, path string) bool {
	if _, admitted := scopeGate(p, rt, method, path); !admitted {
		return false
	}
	return !confinedOffRoute(p, rt, method, path)
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
//
// surface, when not "", is the grantable surface the call belongs to: a
// credential granted it is not refused (auth.Restrictions.Surfaces).
func (s *Server) refuseAgentlessCall(ctx context.Context, what, surface string) error {
	if tools.RunID(ctx) != "" {
		return nil
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || !p.Restrictions.RunPlaneOnly() {
		return nil
	}
	if surface != "" && p.Restrictions.SurfaceAllowed(surface) {
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

// confinedOffRoute reports whether p's run-plane confinement keeps it off the
// route: p is limited to certain agents, the route is not on the run plane,
// and it is not an off-run surface p was granted (auth.Restrictions.Surfaces).
// Every HTTP check of the confinement asks this, so the gate, the parity
// helper and the whoami summary cannot disagree about a granted surface.
func confinedOffRoute(p auth.Principal, rt authz.Route, method, path string) bool {
	if !p.Restrictions.RunPlaneOnly() || rt.RunPlane {
		return false
	}
	for _, surface := range offRunSurfacesOf(method, path) {
		if p.Restrictions.SurfaceAllowed(surface) {
			return false
		}
	}
	return true
}

// offRunSurfacesOf names the grantable surfaces a route belongs to (RFC GA).
// The info route serves either direction, so either grant reaches it; the
// handler then shows only the directions the caller may use.
func offRunSurfacesOf(method, path string) []string {
	switch {
	case method == http.MethodPost && path == audioSpeechPath:
		return []string{auth.SurfaceSpeech}
	case method == http.MethodPost && path == audioTranscriptionsPath:
		return []string{auth.SurfaceTranscription}
	case method == http.MethodGet && path == speechInfoPath:
		return []string{auth.SurfaceSpeech, auth.SurfaceTranscription}
	case method == http.MethodPut && isInboxPutPath(path):
		return []string{auth.SurfaceInboxPut}
	}
	return nil
}

// isInboxPutPath reports whether path is PUT /v1/runs/{run_id}/inbox/{name}'s:
// exactly five segments, so a deeper path (which the mux does not route here)
// opens nothing.
func isInboxPutPath(path string) bool {
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(seg) == 5 && seg[0] == "v1" && seg[1] == "runs" && seg[2] != "" && seg[3] == "inbox" && seg[4] != ""
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
	s.tenantPolicyCache.flush()
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
	safego.Go(ctx, "http.token_invalidation_listener", func() {
		for range ch {
			// Recovered per event: a listener that stopped would leave this
			// replica serving retired tokens from its cache.
			safego.Run(ctx, "http.token_invalidation_listener", func() {
				s.tokenCache.flush()
				s.tenantPolicyCache.flush()
			})
		}
	})
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
// Authorization header; the client-tools WebSocket ticket (RFC GJ, every
// mode); the client-tools WebSocket subprotocol bearer (not in strict mode); a
// Web UI session cookie; and, in token mode only, the legacy bearer cookie of
// earlier releases (RFC FA decision 2). ui is set when a
// cookie authenticated the request, so the caller applies the CSRF check. A
// present credential that fails is a refusal — the next kind is not tried.
//
// The subprotocol bearer comes before the cookies on purpose: a browser
// extension holding its own bearer may also carry the user's session cookie,
// and its chrome-extension:// Origin would fail the cookie path's origin check.
func (s *Server) authenticateRequest(r *http.Request) (auth.Principal, *uiAuth, bool) {
	strict := s.cfg().ResolvedAuth.Strict()
	ctx := r.Context()
	// RFC GJ: a one-time ticket on the client-tools upgrade, in every mode.
	// After an Authorization header (a client that can send one does not
	// need a ticket, and the ticket is then left unspent), before the
	// subprotocol bearer and the cookies, for the reason the bearer comes
	// before the cookies.
	if r.Header.Get("Authorization") == "" {
		if t, ok := clientToolsTicketOf(r); ok {
			p, ok := s.redeemClientToolsTicket(r, t)
			return p, nil, ok
		}
		// A page socket's ticket (RFC GW P9, D-P9-42 as ruled), in every
		// mode, as the client-tools ticket.
		if t, id, ok := exposureTicketOf(r); ok {
			p, ok := s.redeemExposureTicket(r, t, id)
			return p, nil, ok
		}
	}
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
	if _, ok := clientToolsTicketOf(r); ok {
		return true
	}
	if _, _, ok := exposureTicketOf(r); ok {
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
	// a WebSocket upgrade to that route, or to a workspace's exposed page (RFC
	// GW §5.3, which names this same channel) (RFC ES #5): elsewhere the header
	// would be a second credential channel that proxies and logs do not treat
	// as one. Strict mode does not read it (RFC FA §4): a browser there
	// authenticates by its session.
	if strict || !(isClientToolsUpgrade(r) || isExposureSocketUpgrade(r)) {
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
// closed on. A store fault is an error; a cancelled request is only a debug
// line, because a client can cancel its own request at will.
func (s *Server) logAdminCountErr(ctx context.Context, err error) {
	if ctx.Err() != nil {
		slog.Debug("auth: admin-token count failed on a cancelled request; failing closed", "err", err)
		return
	}
	slog.Error("auth: admin-token count failed; failing closed", "err", err)
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
		if p, ok = s.liveTokenPrincipal(ctx, p); !ok || !s.issuedTokenAllowed(ctx, p) {
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
	// RFC FA P5: the tenant gate policy is checked outside the token cache,
	// like the user's status and the ACL set, so a policy that arrives without
	// a token change (config, a failed sweep) still holds.
	if p, found = s.liveTokenPrincipal(ctx, p); !found || !s.issuedTokenAllowed(ctx, p) {
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
//     still only narrow;
//   - under auth.strict.user_tokens: gate_only (RFC FA §4), a token no gate
//     issued is refused when its subject is a registered user: it is a
//     hand-minted per-user token. Decided here, per request and after the
//     token cache, so a user created after the token was minted, or a policy
//     switched on by a restart, holds at once on every transport.
//
// A frozen token from none of these paths is returned as it is, with no read.
// Fails closed: a user that cannot be read, or an access_mode that grants
// nothing, refuses the request.
func (s *Server) liveTokenPrincipal(ctx context.Context, p auth.Principal) (auth.Principal, bool) {
	gateOnly := s.gateOnlyApplies(p)
	if p.IssuedByGate == "" && !p.InheritsAuthority && !gateOnly {
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
		// Under gate_only a frozen token whose subject is no user is a
		// service token, and stands.
		if isNotFound(err) && gateOnly && !p.InheritsAuthority {
			return p, true
		}
		if !isNotFound(err) && ctx.Err() == nil {
			slog.Error("auth: reading a token's user failed (refusing)", "token_def", p.TokenDefID, "err", err)
		}
		return auth.Principal{}, false
	}
	if gateOnly {
		slog.DebugContext(ctx, "auth: refused a per-user token no gate issued (auth.strict.user_tokens: gate_only)", "token_def", p.TokenDefID)
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
		if isNotFound(err) && (s.wrappedTokens.Load() || h.HasPrevious() || h.HasLegacy()) {
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
			// Debug-level: one line per request for as long as the store is
			// down, and the store reports its own outage.
			slog.DebugContext(ctx, "auth: token-substrate lookup error (failing closed)", "err", err)
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

// otherUsersRuns reports whether the ctx principal is confined to its own runs
// (auth.OwnRowsOnly — every member, isolated or not) and is reaching for a
// DIFFERENT user's per-user run surface (a `/v1/users/{user_id}/…` route).
// Handlers answer it as a user with no runs (or an opaque 404) rather than
// with another user's data. A tenant operator and an admin keep the tenant
// view (returns false).
func (s *Server) otherUsersRuns(ctx context.Context, pathUserID string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return auth.OwnRowsOnly(p, ok) && pathUserID != p.Subject
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
// The rule is auth.OwnedRowVisible: a member (isolated or not) reads and
// continues only its OWN sessions — a continuation runs under sess.UserID, so
// another user's session would also hand it that user's user-scoped data; a
// tenant operator reaches every session of its tenant; a super-admin
// (substrate:admin), the legacy principal and open mode reach every session.
// A non-isolated member was once trusted with its whole tenant's sessions;
// people now read only their own conversations.
func sessionOwnershipOK(ctx context.Context, sess store.Session) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return auth.OwnedRowVisible(p, ok, sess.TenantID, sess.UserID)
}

// runOwnershipOK is sessionOwnershipOK for a run: whether the ctx principal
// may read the run's content — its prompt snapshot, result, spec or draft —
// or act on it. The tenant accessor alone admits a member to another user's
// run in the same tenant; every read and every action on a run applies this
// on top of it and answers a false with its own opaque not-found.
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
		out := map[string]any{
			"tenant_id": "default", "subject": "default",
			"scopes": []string{auth.ScopeAdmin}, "is_admin": true,
			"legacy": false, "open_mode": true,
			"capabilities": s.serverCapabilities(),
			// The rollout mode is the deployment's, not the principal's: an
			// open-mode console shows it like any other.
			"acl_mode":  string(s.aclMode()),
			"effective": s.withGatedSurfaces(s.withAudioSurfaces(r.Context(), openModeEffective())),
			"auth":      s.whoamiAuth(r.Context(), true),
		}
		s.addTraceURLTemplate(out)
		writeJSONOK(w, out)
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
	// RFC EY: the principal's groups and the rollout mode (its tenant's, RFC
	// EZ P5), so a UI can show why a surface is missing; the compiled set
	// itself is not serialised.
	out["acl_mode"] = string(s.ACLModeFor(p.TenantID))
	if p.Groups != nil {
		out["groups"] = p.Groups
	}
	if p.Effective != nil {
		out["acl_version"] = p.Effective.Version
	}
	// RFC EZ §4: which Web UI pages the principal may reach, asked of the
	// gates a request to each page's primary route would face.
	out["effective"] = s.withGatedSurfaces(s.withAudioSurfaces(r.Context(), navEffective(r.Context(), p)))
	// RFC FA: how the caller signed in — the mode always, the gate, methods
	// and session times when a Web UI session authenticated the request.
	out["auth"] = s.whoamiAuth(r.Context(), false)
	// A user awaiting approval may learn that it waits, nothing else.
	if ui, ok := uiAuthFrom(r.Context()); !ok || !ui.pending {
		s.addTraceURLTemplate(out)
	}
	writeJSONOK(w, out)
}

// addTraceURLTemplate adds the operator's link from a run to its trace
// (LOOMCYCLE_TRACE_URL_TEMPLATE, checked to be http(s) at boot); the run page
// fills in the run's trace_id. Absent when unset.
func (s *Server) addTraceURLTemplate(out map[string]any) {
	if t := s.cfg().Env.TraceURLTemplate; t != "" {
		out["trace_url_template"] = t
	}
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
	return memberpath.Route(routeAt(method, path).Scope, method, path)
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

// scimRoutePrefix is the SCIM 2.0 service (RFC FA P6), the whole reach of a
// scim:provision credential.
const scimRoutePrefix = "/scim/v2/"

func scimRoute(path string) bool { return strings.HasPrefix(path, scimRoutePrefix) }

// metricsScrapeRoute is the whole reach of a metrics:read credential: the
// Prometheus scrape. HEAD rides along because the mux serves HEAD on a GET
// pattern, and a scraper's health probe may send one.
func metricsScrapeRoute(method, path string) bool {
	return path == "/metrics" && (method == http.MethodGet || method == http.MethodHead)
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

// isDocumentExportPath matches /v1/_document/export/{document_id}.
func isDocumentExportPath(path string) bool {
	id, ok := strings.CutPrefix(path, "/v1/_document/export/")
	return ok && id != "" && !strings.Contains(id, "/")
}
