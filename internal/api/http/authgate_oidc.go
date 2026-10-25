package http

// authgate_oidc.go — RFC FA P1: the redirect-login routes and the OIDC gates
// behind them.
//
//	GET /v1/_auth/login/{gate}?return_to=&reauth=1   start: login cookie + state row, 303 to the provider
//	GET /v1/_auth/complete/{gate}?code=&state=       callback: consume the state, verify, provision, session
//
// The login-state machinery here is the shared one for every redirect gate
// (OIDC now, OAuth in P5): the browser's login cookie holds a 32-byte secret,
// the state row holds hashes, and the PKCE verifier and nonce derive from the
// two (internal/authgate/codeflow). A callback needs the row (single-use,
// ten minutes) AND the cookie of the browser that started the login.
//
// The login cookie is SameSite=Lax, not Strict: the callback is a top-level
// navigation from the provider's site, on which a Strict cookie is never
// sent. Lax is still not sent on a cross-site POST or subresource, and the
// cookie carries no authority — only the binding of one login to one browser.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/codeflow"
	"github.com/denn-gubsky/loomcycle/internal/authgate/identity"
	"github.com/denn-gubsky/loomcycle/internal/authgate/jwtverify"
	"github.com/denn-gubsky/loomcycle/internal/authgate/oidc"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// loginCookieSecure / loginCookiePlain follow the session cookie's
	// transport rule (P0): __Host- over HTTPS, a plain name over HTTP.
	loginCookieSecure = "__Host-lc_login"
	loginCookiePlain  = "lc_login"
	// loginStartWindow / loginStartLimit bound how many logins one client
	// source may start: each start writes a state row, so an unauthenticated
	// caller must not be able to fill the table.
	loginStartWindow = 10 * time.Minute
	loginStartLimit  = 30
)

// redirectGate is a gate whose interactive login is a redirect to a
// provider: Begin's URL, without writing it, for a start that answers JSON
// (an account link from the Web UI).
type redirectGate interface {
	authgate.Gate
	AuthURL(ctx context.Context, ls authgate.LoginState) (string, error)
	EndSessionURL(ctx context.Context, postLogout string) string
}

// oidcGateCache keeps one oidc.Gate per configured gate across requests (its
// discovery document and key set are cached in it). A config reload that
// changes a gate's settings gets a fresh one.
type oidcGateCache struct {
	mu sync.Mutex
	m  map[string]oidcCached
}

type oidcCached struct {
	cfg  *config.AuthOIDCGate
	gate *oidc.Gate
}

// oidcGateNamed returns the configured OIDC gate of that name.
func (s *Server) oidcGateNamed(name string) (*oidc.Gate, config.AuthGate, bool) {
	g, ok := s.configuredGate(name)
	if !ok || g.Kind != config.AuthGateKindOIDC || g.OIDC == nil || g.Identity == nil {
		return nil, config.AuthGate{}, false
	}
	c := &s.oidcGates
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]oidcCached{}
	}
	if e, ok := c.m[name]; ok && e.cfg == g.OIDC {
		return e.gate, g, true
	}
	gate := oidc.New(name, *g.OIDC, s.oidcHTTP)
	c.m[name] = oidcCached{cfg: g.OIDC, gate: gate}
	return gate, g, true
}

// redirectGateNamed is the redirect gate of that name (OIDC; OAuth next).
func (s *Server) redirectGateNamed(name string) (redirectGate, config.AuthGate, bool) {
	if g, cfg, ok := s.oidcGateNamed(name); ok {
		return g, cfg, true
	}
	if g, cfg, ok := s.oauthGateNamed(name); ok {
		return g, cfg, true
	}
	return nil, config.AuthGate{}, false
}

// gateRedirectURI and gateAllowedReturnTo read the redirect gate's settings.
func gateRedirectURI(g config.AuthGate) string {
	if g.OIDC != nil {
		return g.OIDC.RedirectURI
	}
	if g.OAuth != nil {
		return g.OAuth.RedirectURI
	}
	return ""
}

func gateAllowedReturnTo(g config.AuthGate) []string {
	if g.OIDC != nil {
		return g.OIDC.AllowedReturnTo
	}
	if g.OAuth != nil {
		return g.OAuth.AllowedReturnTo
	}
	return nil
}

func (s *Server) loginCookieName(r *http.Request) string {
	if s.requestSecure(r) {
		return loginCookieSecure
	}
	return loginCookiePlain
}

// loginRedirect sends the browser back to the login page with an opaque
// error code (the reason is in the log and the audit record).
func loginRedirect(w http.ResponseWriter, r *http.Request, gate, code string) {
	q := url.Values{"error": {code}}
	if gate != "" {
		q.Set("gate", gate)
	}
	http.Redirect(w, r, "/ui/login?"+q.Encode(), http.StatusSeeOther)
}

// loginStartAllowed counts one login start for the client source and
// reports whether it may go ahead. A throttle read failure refuses.
func (s *Server) loginStartAllowed(ctx context.Context, r *http.Request) bool {
	h, err := s.hasher()
	if err != nil {
		return false
	}
	key := "loginstart:" + h.Hash("login-start-source\x00"+throttleSourceKey(s.clientSource(r)))
	now := time.Now()
	if _, until, err := s.store.AuthThrottleGet(ctx, key, now); err != nil || until.After(now) {
		return false
	}
	n, _, err := s.store.AuthThrottleRecordFailure(ctx, key, now, loginStartWindow, func(n int) time.Duration {
		if n >= loginStartLimit {
			return loginStartWindow
		}
		return 0
	})
	return err == nil && n <= loginStartLimit
}

// loginStart is what a started login is for.
type loginStart struct {
	purpose  string
	returnTo string
	reauth   bool
	// link target, for purpose link.
	linkTenant, linkSubject, linkSession string
}

// beginRedirectLogin writes the state row and the login cookie and returns the
// provider URL. The shared start of every redirect gate.
func (s *Server) beginRedirectLogin(w http.ResponseWriter, r *http.Request, cfg config.AuthGate, gate redirectGate, st loginStart) (string, error) {
	ctx := r.Context()
	l, err := codeflow.NewLogin()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	row := store.AuthLoginStateRow{StateHash: codeflow.StateHash(l.State), Gate: cfg.Name, BindingHash: codeflow.BindingHash(l.Secret),
		Purpose: st.purpose, ReturnTo: st.returnTo, Reauth: st.reauth, LinkTenant: st.linkTenant, LinkSubject: st.linkSubject,
		LinkSessionHash: st.linkSession, CreatedAt: now, ExpiresAt: now.Add(codeflow.TTL)}
	if err := s.store.AuthLoginStateCreate(ctx, row); err != nil {
		return "", err
	}
	u, err := gate.AuthURL(ctx, codeflow.State(l.Secret, l.State, gateRedirectURI(cfg), st.reauth, now))
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: s.loginCookieName(r), Value: codeflow.EncodeSecret(l.Secret), Path: "/",
		MaxAge: int(codeflow.TTL / time.Second), HttpOnly: true, Secure: s.requestSecure(r), SameSite: http.SameSiteLaxMode})
	return u, nil
}

// handleRedirectLogin serves GET /v1/_auth/login/{gate}: start a login.
// Unauthenticated; throttled per client source.
func (s *Server) handleRedirectLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("gate")
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	gate, cfg, ok := s.redirectGateNamed(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "gate_not_found", "no sign-in gate of that name")
		return
	}
	if !s.requireStore(w) {
		return
	}
	if s.sessionRefusal(r) != "" {
		loginRedirect(w, r, name, "insecure_transport")
		return
	}
	returnTo, ok := identity.SafeReturnTo(r.URL.Query().Get("return_to"), gateAllowedReturnTo(cfg))
	if !ok {
		loginRedirect(w, r, name, "return_to_refused")
		return
	}
	if !s.loginStartAllowed(r.Context(), r) {
		loginRedirect(w, r, name, "too_many_attempts")
		return
	}
	u, err := s.beginRedirectLogin(w, r, cfg, gate, loginStart{purpose: store.LoginPurposeLogin, returnTo: returnTo,
		reauth: r.URL.Query().Get("reauth") == "1"})
	if err != nil {
		slog.Error("auth: starting a sign-in failed", "gate", name, "err", err)
		loginRedirect(w, r, name, "gate_unavailable")
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// takeLoginState finishes the start half of a callback: the browser's cookie
// secret, the single-use state row, and their binding. The cookie is cleared
// whatever the outcome.
func (s *Server) takeLoginState(w http.ResponseWriter, r *http.Request, cfg config.AuthGate) (store.AuthLoginStateRow, authgate.LoginState, string) {
	secure := s.requestSecure(r)
	http.SetCookie(w, &http.Cookie{Name: s.loginCookieName(r), Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode})
	c, err := r.Cookie(s.loginCookieName(r))
	if err != nil {
		return store.AuthLoginStateRow{}, authgate.LoginState{}, "login_expired"
	}
	secret, ok := codeflow.DecodeSecret(c.Value)
	state := r.URL.Query().Get("state")
	if !ok || state == "" || len(state) > 256 {
		return store.AuthLoginStateRow{}, authgate.LoginState{}, "login_expired"
	}
	row, err := s.store.AuthLoginStateConsume(r.Context(), codeflow.StateHash(state), time.Now())
	if err != nil {
		if !isNotFound(err) {
			slog.Error("auth: reading a login state failed", "gate", cfg.Name, "err", err)
			return store.AuthLoginStateRow{}, authgate.LoginState{}, "gate_unavailable"
		}
		return store.AuthLoginStateRow{}, authgate.LoginState{}, "login_expired"
	}
	if row.Gate != cfg.Name || !jwtverify.ConstantEqual(row.BindingHash, codeflow.BindingHash(secret)) {
		// The state was started by another browser (or for another gate): a
		// login CSRF or a code injected into this browser.
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: cfg.Name, AuthReason: "login state not bound to this browser",
			SourceAddr: s.clientSource(r)})
		return store.AuthLoginStateRow{}, authgate.LoginState{}, "login_failed"
	}
	return row, codeflow.State(secret, state, gateRedirectURI(cfg), row.Reauth, row.CreatedAt), ""
}

// handleRedirectComplete serves GET /v1/_auth/complete/{gate}: the
// provider's callback.
func (s *Server) handleRedirectComplete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("gate")
	ctx := r.Context()
	if !s.authConfigured(ctx) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	gate, cfg, ok := s.redirectGateNamed(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "gate_not_found", "no sign-in gate of that name")
		return
	}
	if !s.requireStore(w) {
		return
	}
	row, ls, code := s.takeLoginState(w, r, cfg)
	if code != "" {
		loginRedirect(w, r, name, code)
		return
	}
	source := s.clientSource(r)
	a, err := gate.Complete(ctx, r, ls)
	if err != nil {
		reason := err.Error()
		code := "login_failed"
		if r.URL.Query().Get("error") == "access_denied" {
			code = "access_denied"
		} else if !errors.Is(err, authgate.ErrInvalidCredential) {
			code = "gate_unavailable"
		}
		slog.Warn("auth: sign-in refused", "gate", name, "reason", reason)
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: name, AuthReason: reason, SourceAddr: source})
		loginRedirect(w, r, name, code)
		return
	}
	if row.Purpose == store.LoginPurposeLink {
		s.completeLink(w, r, cfg, row, a)
		return
	}
	if s.sessionRefusal(r) != "" {
		loginRedirect(w, r, name, "insecure_transport")
		return
	}
	out, err := s.provisionOrRefuse(ctx, cfg, a, source)
	if err != nil {
		if !errors.Is(err, errIdentityRefused) && !errors.Is(err, errUserDisabled) {
			slog.Error("auth: provisioning a sign-in failed", "gate", name, "err", err)
			loginRedirect(w, r, name, "gate_unavailable")
			return
		}
		loginRedirect(w, r, name, "login_failed")
		return
	}
	if err := s.establishSession(w, r, sessionIdentity{TenantID: out.TenantID, Subject: out.Subject, Gate: name,
		AMR: a.AMR, AuthTime: a.AuthTime}); err != nil {
		slog.Error("auth: creating a Web UI session failed", "gate", name, "err", err)
		loginRedirect(w, r, name, "gate_unavailable")
		return
	}
	dest := row.ReturnTo
	if dest == "" {
		dest = "/ui/"
	}
	if out.Pending {
		dest = "/ui/pending"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// endSessionURL is where a logout from gate's session sends the browser
// next, "" for none.
func (s *Server) endSessionURL(ctx context.Context, gate string) string {
	g, _, ok := s.redirectGateNamed(gate)
	if !ok {
		return ""
	}
	post := ""
	if pub := s.cfg().Env.PublicURL; pub != "" {
		post = pub + "/ui/login"
	}
	return g.EndSessionURL(ctx, post)
}
