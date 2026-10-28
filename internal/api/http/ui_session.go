package http

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/awsiam"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/webui"
)

// Web UI sessions (RFC FA P0): the browser holds an opaque session id, never a
// bearer. A gate (internal/authgate) verifies the person; establishSession
// turns its assertion into a session row and a cookie, for every gate alike;
// sessionPrincipal authenticates each later request from the row.
//
// How a session stays bound to the credential it came from. A session made
// through the token gate must never outlive or out-reach the bearer that was
// pasted, yet the bearer must not be stored retrievably. The row records WHERE
// the credential lives — a minted token's def id, a config principal's id, or
// "the legacy secret" — and an HMAC, keyed by the session id itself, over the
// hash that credential has now. On every request the server looks the
// credential up again, recomputes the HMAC from the live value and resolves the
// principal through the same code as a bearer would. So a retired or expired
// token, a principal removed from config or given a new secret, and a changed
// LOOMCYCLE_AUTH_TOKEN all end the session on the next request, and the session
// carries the credential's CURRENT scopes, never more. The key being the session
// id (which only the cookie holds) means a database reader cannot test a guess
// at a weak legacy secret against the row. Encrypting the bearer instead was
// rejected: it would put a decryptable API credential at rest for every session.

const (
	// sessionCookieSecure is the cookie over TLS (or behind a TLS terminator):
	// the __Host- prefix makes the browser refuse it unless it is Secure, has
	// Path=/ and no Domain, so no sibling host can plant or read it.
	sessionCookieSecure = "__Host-lc_session"
	// sessionCookiePlain is the cookie over plain HTTP, which cannot carry the
	// __Host- prefix. Token mode uses it so LAN installs keep working (warned);
	// strict mode only with LOOMCYCLE_UI_INSECURE_COOKIES=1.
	sessionCookiePlain = "lc_session"
	// uiRequestHeader is the CSRF header every state-changing request a
	// session cookie authenticates must carry. A cross-site page cannot add a
	// custom header without a CORS preflight, which this server never grants.
	uiRequestHeader = "X-Loomcycle-UI"

	// sessionIDBytes is the cookie id's entropy (OWASP asks for 64 bits).
	sessionIDBytes = 32
	// sessionTouchEvery bounds how often a request writes last_seen_at; the
	// idle timeout is hours, so a minute of slack costs nothing.
	sessionTouchEvery = time.Minute
	// uiWarnEvery spaces the deprecation and plain-HTTP warnings.
	uiWarnEvery = 10 * time.Minute

	credKindToken     = "token"
	credKindPrincipal = "principal"
	credKindLegacy    = "legacy"

	// hasherProbe is hashed with the token hasher to name the pepper in force.
	// A session's binding covers it, so changing LOOMCYCLE_OPERATOR_TOKEN_PEPPER
	// or LOOMCYCLE_SECRET_KEY ends every session even where a stored token hash
	// does not change with it (an explicit pepper's hashes are untagged).
	hasherProbe = "loomcycle-ui-session-hasher-probe"
)

// uiAuth marks a request authenticated by a Web UI cookie: a session (row
// set) or, in token mode, the legacy bearer cookie (row nil).
type uiAuth struct {
	session *store.UISessionRow
	// pending: the session's user awaits an operator's approval (RFC FA
	// §3.7a). Its principal holds no scope and an empty ACL set, and the
	// middleware admits it to GET /v1/_me alone.
	pending bool
}

type ctxKeyUIAuth struct{}

func withUIAuth(ctx context.Context, a uiAuth) context.Context {
	return context.WithValue(ctx, ctxKeyUIAuth{}, a)
}

func uiAuthFrom(ctx context.Context) (uiAuth, bool) {
	a, ok := ctx.Value(ctxKeyUIAuth{}).(uiAuth)
	return a, ok
}

// requestSecure reports whether the browser reached this server over HTTPS,
// and so whether the session cookie may be Secure with the __Host- prefix.
//
// r.TLS covers a server terminating TLS itself. Behind a TLS terminator
// (Cloudflare Tunnel, tailscale serve, a reverse proxy) r.TLS is nil, and the
// operator says the deployment is served over HTTPS with LOOMCYCLE_PUBLIC_URL.
// That alone is not enough, because the same server is often also reached over
// plain HTTP on the LAN, where a Secure cookie would be dropped by the browser
// and the login would silently fail. So the request must also look like it came
// through that terminator: its Host is the public URL's host (Cloudflare
// forwards the public hostname), or its peer is on this machine (a terminator
// on the same host, which is how tailscale serve proxies). No forwarded header
// is trusted: anyone can send one.
func (s *Server) requestSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	u, err := url.Parse(s.cfg().Env.PublicURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return false
	}
	if strings.EqualFold(r.Host, u.Host) {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sessionCookieName is the cookie a request of this transport uses. Each
// transport reads only its own name: over HTTPS a plain lc_session cookie, which
// a network attacker can plant from an http:// page, is never read.
func (s *Server) sessionCookieName(r *http.Request) string {
	if s.requestSecure(r) {
		return sessionCookieSecure
	}
	return sessionCookiePlain
}

// sessionIDFromCookie returns the raw session id the request's cookie carries.
// A value that does not decode to exactly sessionIDBytes is not an id (this is
// also what keeps a stored row hash, used as a cookie, from reaching the store).
func (s *Server) sessionIDFromCookie(r *http.Request) ([]byte, bool) {
	c, err := r.Cookie(s.sessionCookieName(r))
	if err != nil || c.Value == "" {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != sessionIDBytes {
		return nil, false
	}
	return raw, true
}

// sessionHash is the row key for a session id.
func sessionHash(id []byte) string {
	sum := sha256.Sum256(id)
	return hex.EncodeToString(sum[:])
}

// sessionMAC is an HMAC keyed by the session id, domain-separated by label.
func sessionMAC(id []byte, label, msg string) string {
	m := hmac.New(sha256.New, id)
	m.Write([]byte(label))
	m.Write([]byte{0})
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// macEqual compares two hex MACs in constant time.
func macEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// credentialMAC binds a session to a credential's current hash under the
// pepper in force.
func credentialMAC(id []byte, h auth.TokenHasher, credHash string) string {
	return sessionMAC(id, "cred", h.Hash(hasherProbe)+"\x00"+credHash)
}

// sameOrigin reports whether a browser request came from this server's own
// pages. Sec-Fetch-Site is authoritative when the browser sends it (every
// current browser does); without it the Origin header must name this host. A
// request with neither is refused: a missing header is not evidence of a
// same-origin request.
func sameOrigin(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// uiCSRFRefusal is the CSRF check for a request a Web UI cookie authenticated:
// "" to admit it, else the refusal code. A WebSocket upgrade is a GET but opens
// a two-way channel, so it needs a same-origin Origin like a write does. Other
// reads are admitted: SameSite=Strict keeps the cookie off cross-site requests,
// and a cross-site read cannot see the response.
func uiCSRFRefusal(r *http.Request) string {
	if isClientToolsUpgrade(r) {
		if !sameOrigin(r) {
			return "cross_origin_refused"
		}
		return ""
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ""
	}
	if r.Header.Get(uiRequestHeader) != "1" {
		return "ui_header_required"
	}
	if !sameOrigin(r) {
		return "cross_origin_refused"
	}
	return ""
}

func writeCSRFRefusal(w http.ResponseWriter, code string) {
	msg := "a Web UI session request must come from this server's own pages"
	if code == "ui_header_required" {
		msg = "a Web UI session request that changes state must send " + uiRequestHeader + ": 1"
	}
	writeJSONError(w, http.StatusForbidden, code, msg)
}

// warnEvery logs msg at most once per uiWarnEvery for the given slot.
func warnEvery(slot *atomic.Int64, msg string, args ...any) {
	now := time.Now().UnixNano()
	last := slot.Load()
	if last != 0 && now-last < int64(uiWarnEvery) {
		return
	}
	if slot.CompareAndSwap(last, now) {
		slog.Warn(msg, args...)
	}
}

// loginConfig is what /ui/login-config.json reports for this request.
func (s *Server) loginConfig(r *http.Request) webui.LoginConfig {
	cfg := s.cfg()
	policy := cfg.ResolvedAuth
	lc := webui.LoginConfig{Mode: policy.ModeName()}
	if !s.authConfigured(r.Context()) {
		lc.Mode = "open"
		return lc
	}
	for _, g := range policy.LoginGates() {
		if g.Kind == config.AuthGateKindAWSIAM {
			continue // a machine gate: no browser signs in through it
		}
		lc.Gates = append(lc.Gates, webui.LoginGate{Name: g.Name, Kind: g.Kind, Label: g.Label})
	}
	// The postMessage handoff gives a bearer to the paste-box path; strict
	// mode does not offer it (RFC FA §4: only configured gates sign a person
	// in), even when a token gate is listed for the paste box.
	if !policy.Strict() {
		lc.Origins = cfg.Env.UILoginOrigins
	}
	if s.sessionRefusal(r) != "" {
		lc.SessionRefused = "insecure_transport"
	}
	if label, dest, ok := s.loginApp(r); ok {
		lc.App = &webui.LoginApp{Label: label, Destination: dest}
	}
	return lc
}

// sessionRefusal is why this request may not start a session, "" if it may.
// Strict mode refuses plain HTTP unless the operator opted in with
// LOOMCYCLE_UI_INSECURE_COOKIES=1 (a tailnet, a lab).
func (s *Server) sessionRefusal(r *http.Request) string {
	cfg := s.cfg()
	if cfg.ResolvedAuth.Strict() && !s.requestSecure(r) && !cfg.Env.UIInsecureCookies {
		return "Web UI sessions need HTTPS in strict mode. Serve loomcycle over TLS, set " +
			"LOOMCYCLE_PUBLIC_URL=https://… behind a TLS terminator, or set " +
			"LOOMCYCLE_UI_INSECURE_COOKIES=1 on a private network"
	}
	return ""
}

// gateFor builds the gate a configured entry names. Each kind the RFC adds is
// one more case; the session code below does not change.
func (s *Server) gateFor(g config.AuthGate) (authgate.Gate, error) {
	switch g.Kind {
	case config.AuthGateKindToken:
		return authgate.NewBaseAuthGate(s.resolvePrincipal), nil
	case config.AuthGateKindPassword:
		if pw, ok := s.passwordGateNamed(g.Name); ok {
			return pw, nil
		}
		return nil, fmt.Errorf("auth gate %q: the password gate is not running (see the boot log)", g.Name)
	case config.AuthGateKindOIDC:
		if og, _, ok := s.oidcGateNamed(g.Name); ok {
			return og, nil
		}
		return nil, fmt.Errorf("auth gate %q: the OIDC gate is misconfigured", g.Name)
	case config.AuthGateKindOAuth:
		if og, _, ok := s.oauthGateNamed(g.Name); ok {
			return og, nil
		}
		return nil, fmt.Errorf("auth gate %q: the OAuth gate is misconfigured", g.Name)
	case config.AuthGateKindHeader:
		if hg, ok := s.headerGateNamed(g.Name); ok {
			return hg, nil
		}
		return nil, fmt.Errorf("auth gate %q: the header gate is not running (see the boot log)", g.Name)
	case config.AuthGateKindAWSIAM:
		if ag, ok := s.awsiamGateNamed(g.Name); ok {
			return awsiam.AuthGate{G: ag}, nil
		}
		return nil, fmt.Errorf("auth gate %q: the AWS IAM gate is not running (see the boot log)", g.Name)
	}
	return nil, fmt.Errorf("auth gate %q: kind %q has no implementation", g.Name, g.Kind)
}

// handleUILogin is POST /ui/session: the token gate's login, used by the paste
// box and by the postMessage handoff. The bearer rides the Authorization
// header, so it never appears in a URL, history or an access log.
func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	// Login CSRF: only this server's own page may sign a browser in, or a
	// cross-site page could make a victim's browser adopt an attacker's
	// account.
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross_site_login_refused", "session login must be same-origin")
		return
	}
	cfgGate, ok := s.cfg().ResolvedAuth.TokenGate()
	if !ok {
		writeJSONError(w, http.StatusForbidden, "gate_unavailable", "this server does not accept a pasted token")
		return
	}
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	gate, err := s.gateFor(cfgGate)
	if err != nil {
		slog.Error("auth: login gate unavailable", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "gate_unavailable", "sign-in is unavailable")
		return
	}
	a, err := gate.Complete(r.Context(), r, authgate.LoginState{})
	switch {
	case errors.Is(err, authgate.ErrNoCredential):
		writeJSONError(w, http.StatusBadRequest, "missing_bearer", "Authorization: Bearer <token> required")
		return
	case err != nil:
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: cfgGate.Name, SourceAddr: s.clientSource(r)})
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	}
	if a.Credential == nil {
		slog.Error("auth: the token gate asserted no credential", "gate", cfgGate.Name)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	p := a.Credential.Principal
	ident := sessionIdentity{
		TenantID: p.TenantID, Subject: p.Subject, Gate: cfgGate.Name, AMR: a.AMR, Credential: a.Credential,
	}
	if err := s.establishSession(w, r, ident); err != nil {
		if writeTenantPolicyRefusal(w, err) {
			return
		}
		if errors.Is(err, errSessionsUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "sessions_unavailable", "this server cannot keep Web UI sessions")
			return
		}
		slog.Error("auth: creating a Web UI session failed", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	errSessionsUnavailable = errors.New("web ui sessions need a store")
	// errNoSessionBinding: the identity names no credential this release can
	// bind a session to.
	errNoSessionBinding = errors.New("web ui session: nothing to bind the session to")
)

// sessionIdentity is who a session is for, as a gate verified them: the
// authoritative (tenant, subject), the configured gate's name and the methods
// it used. Every gate's login ends in one, and establishSession takes nothing
// else, so a new gate is a new caller.
type sessionIdentity struct {
	TenantID string
	Subject  string
	Gate     string
	AMR      []string
	// Credential is the loomcycle bearer the person presented, for a gate that
	// verifies one (BaseAuthGate). The session then stays bound to it and is
	// never more than it. Nil for a gate whose verified identity stands on its
	// own; credentialBinding and sessionCredentialPrincipal are where such a
	// session's binding and principal are decided, one case per kind.
	Credential *authgate.Credential
	// AuthTime is when the person authenticated, as the gate reports it. Zero
	// for an identity gate that could not say (no re-authentication check
	// counts that as recent); the token and password gates authenticate the
	// person here and now, so zero there means the login's own time.
	AuthTime time.Time
}

// establishSession turns a verified identity into a session: a new row, a new
// cookie, the audit record. It is the one place a session is created, whichever
// gate verified the person.
func (s *Server) establishSession(w http.ResponseWriter, r *http.Request, ident sessionIdentity) error {
	if s.store == nil {
		return errSessionsUnavailable
	}
	if ident.TenantID == "" || ident.Subject == "" || ident.Gate == "" {
		return errors.New("web ui session: tenant, subject and gate are required")
	}
	ctx := r.Context()
	// RFC FA P5: a tenant whose people must come through named gates.
	if err := s.tenantGateAllowed(ctx, ident.TenantID, ident.Gate); err != nil {
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", ActorTenant: ident.TenantID, ActorSubject: ident.Subject,
			AuthGate: ident.Gate, AuthReason: "tenant_policy", SourceAddr: s.clientSource(r)})
		return err
	}
	h, err := s.hasher()
	if err != nil {
		return err
	}
	kind, ref, credHash, err := s.credentialBinding(ctx, h, ident)
	if err != nil {
		return err
	}
	id := make([]byte, sessionIDBytes)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	now := time.Now()
	policy := s.cfg().ResolvedAuth
	// Fixation: a login never continues an earlier session. The id is new,
	// and the session the browser held before is ended.
	if old, ok := s.sessionIDFromCookie(r); ok {
		if _, err := s.store.UISessionRevoke(ctx, sessionHash(old), now); err != nil {
			slog.Warn("auth: ending the previous Web UI session failed", "err", err)
		}
	}
	authTime := ident.AuthTime
	if _, identityKind := s.identityGate(ident.Gate); authTime.IsZero() && !identityKind {
		authTime = now
	}
	if authTime.After(now) {
		authTime = now
	}
	row := store.UISessionRow{
		AuthTime:          authTime,
		SessionHash:       sessionHash(id),
		TenantID:          ident.TenantID,
		Subject:           ident.Subject,
		Gate:              ident.Gate,
		AMR:               ident.AMR,
		CredKind:          kind,
		CredRef:           ref,
		CredMAC:           credentialMAC(id, h, credHash),
		UAHash:            sessionMAC(id, "ua", r.UserAgent()),
		IPHash:            sessionMAC(id, "ip", s.clientSource(r)),
		CreatedAt:         now,
		LastSeenAt:        now,
		IdleExpiresAt:     now.Add(policy.SessionIdle()),
		AbsoluteExpiresAt: now.Add(policy.SessionAbsolute()),
	}
	if err := s.store.UISessionCreate(ctx, row); err != nil {
		return err
	}
	secure := s.requestSecure(r)
	if !secure {
		warnEvery(&s.plainSessionWarnedAt, "auth: Web UI sessions are being issued over plain HTTP, so the "+
			"session cookie is not Secure and can be read on the network. Serve over HTTPS, or set "+
			"LOOMCYCLE_PUBLIC_URL=https://… behind a TLS terminator")
	}
	// No Max-Age: the cookie lives as long as the browser session, and the
	// server enforces the idle and absolute timeouts on the row.
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(r),
		Value:    base64.RawURLEncoding.EncodeToString(id),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	// The bearer cookie of earlier releases goes the first time the browser
	// signs in again.
	http.SetCookie(w, expiredCookie(webui.LegacyBearerCookie, secure))
	s.recordAuthEvent(audit.Event{
		Action: "auth_login", ActorTenant: ident.TenantID, ActorSubject: ident.Subject,
		AuthGate: ident.Gate, AuthAMR: ident.AMR, SourceAddr: s.clientSource(r),
	})
	return nil
}

// credentialBinding decides what a new session is bound to: the kind and
// reference stored on the row, and the hash its binding MAC covers. For a
// bearer, that is where the credential lives and the hash it has now. It is
// the only place a binding kind is chosen; sessionCredentialPrincipal is the
// only place one is resolved again.
func (s *Server) credentialBinding(ctx context.Context, h auth.TokenHasher, ident sessionIdentity) (kind, ref, credHash string, err error) {
	c := ident.Credential
	if c == nil {
		// A password-gate session is bound to the stored password.
		if _, ok := s.passwordGateNamed(ident.Gate); ok {
			return s.passwordSessionBinding(ctx, ident)
		}
		// An identity-gate session is bound to the linked identity and the
		// user row it names.
		if _, ok := s.identityGate(ident.Gate); ok {
			return s.identitySessionBinding(ctx, ident)
		}
		return "", "", "", fmt.Errorf("%w: gate %q verified an identity with no loomcycle credential", errNoSessionBinding, ident.Gate)
	}
	p := c.Principal
	if p.TenantID != ident.TenantID || p.Subject != ident.Subject {
		return "", "", "", errors.New("web ui session: the identity is not the credential's")
	}
	switch {
	case p.Legacy:
		return credKindLegacy, "", h.Hash(c.Bearer), nil
	case strings.HasPrefix(p.TokenDefID, "cfg:"):
		return credKindPrincipal, p.TokenDefID, h.Hash(c.Bearer), nil
	case p.TokenDefID != "":
		// The row as it is now: resolution may have just moved it onto the
		// current pepper.
		row, err := s.store.OperatorTokenDefGet(ctx, p.TokenDefID)
		if err != nil {
			return "", "", "", err
		}
		return credKindToken, row.DefID, row.TokenHash, nil
	}
	return "", "", "", errors.New("the credential names no token, principal or legacy secret")
}

// sessionPrincipal authenticates a request by its session id. It ends the
// session (revokes the row, audited once) when the session expired, the
// browser changed, or the credential it was bound to is gone; a store error
// refuses this request only.
func (s *Server) sessionPrincipal(r *http.Request, id []byte) (auth.Principal, uiAuth, bool) {
	ctx := r.Context()
	if s.store == nil {
		return auth.Principal{}, uiAuth{}, false
	}
	row, err := s.store.UISessionGet(ctx, sessionHash(id))
	if err != nil {
		if !isNotFound(err) && ctx.Err() == nil {
			slog.Error("auth: reading a Web UI session failed (refusing the request)", "err", err)
		}
		return auth.Principal{}, uiAuth{}, false
	}
	if !row.RevokedAt.IsZero() {
		return auth.Principal{}, uiAuth{}, false
	}
	now := time.Now()
	policy := s.cfg().ResolvedAuth
	if !now.Before(sessionExpiresAt(row, policy)) {
		s.endSession(ctx, r, row, "auth_session_expired", "expired")
		return auth.Principal{}, uiAuth{}, false
	}
	if !macEqual(sessionMAC(id, "ua", r.UserAgent()), row.UAHash) {
		s.endSession(ctx, r, row, "auth_session_revoked", "user_agent_changed")
		return auth.Principal{}, uiAuth{}, false
	}
	p, pending, ok, definitive := s.sessionCredentialPrincipal(ctx, id, row)
	if !ok {
		if definitive {
			s.endSession(ctx, r, row, "auth_session_revoked", "credential_invalid")
		}
		return auth.Principal{}, uiAuth{}, false
	}
	// RFC FA P5: the tenant's gate policy, on every request: the gate the
	// session came through, and the gate that issued its token, if one did.
	for _, gate := range []string{row.Gate, p.IssuedByGate} {
		if gate == "" {
			continue
		}
		if err := s.tenantGateAllowed(ctx, row.TenantID, gate); err != nil {
			if errors.Is(err, errTenantGatePolicy) {
				s.endSession(ctx, r, row, "auth_session_revoked", "tenant_policy")
			}
			return auth.Principal{}, uiAuth{}, false
		}
	}
	// A header-gate session lasts only while its proxy still vouches for the
	// same person on each request (authgate_edge.go).
	if holds, definitive := s.edgeSessionHolds(r, row); !holds {
		if definitive {
			s.endSession(ctx, r, row, "auth_session_revoked", "proxy_assertion_lost")
		}
		return auth.Principal{}, uiAuth{}, false
	}
	if now.Sub(row.LastSeenAt) >= sessionTouchEvery {
		idle := now.Add(policy.SessionIdle())
		if idle.After(row.AbsoluteExpiresAt) {
			idle = row.AbsoluteExpiresAt
		}
		if err := s.store.UISessionTouch(ctx, row.SessionHash, now, idle); err != nil {
			slog.Warn("auth: recording Web UI session activity failed", "err", err)
		} else {
			row.LastSeenAt, row.IdleExpiresAt = now, idle
		}
	}
	p.Login = sessionLogin(row)
	return p, uiAuth{session: &row, pending: pending}, true
}

// sessionLogin is the sign-in provenance a session's principal carries into
// the runs it starts (Context op=self reports it): the gate, its methods and
// when the person authenticated — the creation time when the gate could not
// say, as /v1/_me reports it. Nothing of the session's own identity.
func sessionLogin(row store.UISessionRow) *auth.Login {
	authTime := row.AuthTime
	if authTime.IsZero() {
		authTime = row.CreatedAt
	}
	return &auth.Login{Gate: row.Gate, AMR: append([]string(nil), row.AMR...), AuthTime: authTime}
}

// sessionExpiresAt is when the session stops authenticating: its own idle and
// absolute times, held to the configured lifetimes too, so an operator who
// shortens them shortens the sessions already issued.
func sessionExpiresAt(row store.UISessionRow, policy config.AuthPolicy) time.Time {
	end := row.AbsoluteExpiresAt
	for _, t := range []time.Time{
		row.IdleExpiresAt,
		row.CreatedAt.Add(policy.SessionAbsolute()),
		row.LastSeenAt.Add(policy.SessionIdle()),
	} {
		if t.Before(end) {
			end = t
		}
	}
	return end
}

// sessionCredentialPrincipal re-resolves the credential a session is bound to.
// ok is false when it no longer authenticates as the same credential;
// definitive is false when that could not be decided (a store error), so the
// session is kept for the next request. pending is set for a user awaiting
// approval (identity gates only).
func (s *Server) sessionCredentialPrincipal(ctx context.Context, id []byte, row store.UISessionRow) (p auth.Principal, pending, ok, definitive bool) {
	p, ok, definitive = s.sessionCredentialPrincipalOf(ctx, id, row, &pending)
	return p, pending, ok, definitive
}

// sessionCredentialPrincipalOf is the per-kind resolution. Only the identity
// case sets *pending; the other kinds keep their three-value returns.
func (s *Server) sessionCredentialPrincipalOf(ctx context.Context, id []byte, row store.UISessionRow, pending *bool) (p auth.Principal, ok, definitive bool) {
	h, err := s.hasher()
	if err != nil {
		return auth.Principal{}, false, false
	}
	bound := func(credHash string) bool { return macEqual(credentialMAC(id, h, credHash), row.CredMAC) }
	return s.boundCredentialPrincipal(ctx, row, bound, pending)
}

// boundCredentialPrincipal re-resolves the credential row names, accepting it
// only when bound says its current hash is the one the caller bound to. The
// session binds with a MAC keyed by its id; a device approval binds with one
// keyed by the device code, so its redemption can re-check the credential
// without ever holding the session id.
func (s *Server) boundCredentialPrincipal(ctx context.Context, row store.UISessionRow, bound func(credHash string) bool, pending *bool) (p auth.Principal, ok, definitive bool) {
	h, err := s.hasher()
	if err != nil {
		return auth.Principal{}, false, false
	}
	switch row.CredKind {
	case credKindToken:
		tr, err := s.store.OperatorTokenDefGet(ctx, row.CredRef)
		if err != nil {
			return auth.Principal{}, false, isNotFound(err)
		}
		if !bound(tr.TokenHash) {
			return auth.Principal{}, false, true
		}
		p, ok, _ := tokenRowPrincipal(tr, time.Now())
		if !ok {
			return auth.Principal{}, false, true
		}
		// A gate-issued token pasted into the UI holds only while its user
		// is active, and an inherit token takes its user's current scopes,
		// as on the API (liveTokenPrincipal).
		if p, ok = s.liveTokenPrincipal(ctx, p); !ok {
			return auth.Principal{}, false, false
		}
		return s.enrichPrincipal(ctx, p), true, true
	case credKindPrincipal:
		for _, d := range s.cfg().ResolvedPrincipals {
			if d.Principal.TokenDefID != row.CredRef || d.Secret == "" || !bound(h.Hash(d.Secret)) {
				continue
			}
			// The full bearer path, so a minted token that now shadows the
			// secret, or anything else the bearer lookup would decide, decides
			// here too.
			p, ok := s.resolvePrincipal(ctx, d.Secret)
			if !ok {
				return auth.Principal{}, false, false
			}
			return p, p.TokenDefID == row.CredRef, true
		}
		return auth.Principal{}, false, true
	case credKindPassword:
		return s.passwordSessionPrincipal(ctx, row, bound)
	case credKindIdentity:
		return s.identitySessionPrincipal(ctx, row, bound, pending)
	case credKindLegacy:
		secret := s.cfg().Env.AuthToken
		if secret == "" || !bound(h.Hash(secret)) {
			return auth.Principal{}, false, true
		}
		p, ok := s.resolvePrincipal(ctx, secret)
		if !ok {
			// Retired by an admin token, or the store could not say: refuse
			// this request and keep the row, the sweeper ends it at expiry.
			return auth.Principal{}, false, false
		}
		return p, p.Legacy, true
	}
	return auth.Principal{}, false, true
}

// endSession revokes a session and records why, once.
func (s *Server) endSession(ctx context.Context, r *http.Request, row store.UISessionRow, action, reason string) {
	changed, err := s.store.UISessionRevoke(ctx, row.SessionHash, time.Now())
	if err != nil {
		slog.Warn("auth: ending a Web UI session failed", "err", err)
		return
	}
	if changed {
		s.recordAuthEvent(audit.Event{
			Action: action, ActorTenant: row.TenantID, ActorSubject: row.Subject,
			AuthGate: row.Gate, AuthReason: reason, SourceAddr: s.clientSource(r),
		})
	}
}

// handleAuthLogout is POST /v1/_auth/logout: it ends the browser's session on
// the server, clears every Web UI cookie and asks the browser to drop its
// cookies and storage. It runs without the auth middleware, so an expired
// session can still log out cleanly; the CSRF check stands in for it, so no
// other site can sign a user out.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(uiRequestHeader) != "1" || !sameOrigin(r) {
		writeCSRFRefusal(w, "cross_origin_refused")
		return
	}
	endSession := ""
	if id, ok := s.sessionIDFromCookie(r); ok && s.store != nil {
		row, err := s.store.UISessionGet(r.Context(), sessionHash(id))
		if err == nil {
			// RFC FA §5: an OIDC gate may also end the provider's session
			// (RP-initiated logout); the page follows the URL if one is given.
			if row.RevokedAt.IsZero() {
				endSession = s.endSessionURL(r.Context(), row.Gate)
			}
			changed, rerr := s.store.UISessionRevoke(r.Context(), row.SessionHash, time.Now())
			if rerr != nil {
				slog.Error("auth: logout could not end the session", "err", rerr)
				writeJSONError(w, http.StatusServiceUnavailable, "logout_failed", "the session could not be ended; try again")
				return
			}
			if changed {
				s.recordAuthEvent(audit.Event{
					Action: "auth_logout", ActorTenant: row.TenantID, ActorSubject: row.Subject,
					AuthGate: row.Gate, SourceAddr: s.clientSource(r),
				})
			}
		}
	}
	secure := s.requestSecure(r)
	for _, name := range []string{sessionCookieSecure, sessionCookiePlain, webui.LegacyBearerCookie} {
		http.SetCookie(w, expiredCookie(name, secure || name == sessionCookieSecure))
	}
	w.Header().Set("Clear-Site-Data", `"cookies", "storage"`)
	if endSession != "" {
		writeJSON(w, http.StatusOK, map[string]string{"end_session_url": endSession})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// expiredCookie deletes a cookie. A __Host- cookie can only be deleted by a
// Set-Cookie that is itself Secure.
func expiredCookie(name string, secure bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode}
}

// recordAuthEvent writes a sign-in audit record and its log line. Best-effort:
// it never fails the request.
func (s *Server) recordAuthEvent(ev audit.Event) {
	slog.Info("auth: "+ev.Action, "gate", ev.AuthGate, "tenant", ev.ActorTenant, "subject", ev.ActorSubject,
		"reason", ev.AuthReason, "source", ev.SourceAddr)
	if s.tokenAudit == nil {
		return
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.Error("auth: audit write failed", "err", err)
	}
}

// whoamiAuth is /v1/_me's `auth` block: the mode always, and the session's
// gate, methods and times when a session authenticated the request.
func (s *Server) whoamiAuth(ctx context.Context, open bool) map[string]any {
	mode := s.cfg().ResolvedAuth.ModeName()
	if open {
		mode = "open"
	}
	out := map[string]any{"mode": mode}
	if a, ok := uiAuthFrom(ctx); ok && a.session != nil {
		out["gate"] = a.session.Gate
		out["amr"] = a.session.AMR
		out["auth_time"] = sessionLogin(*a.session).AuthTime.UTC()
		if a.pending {
			// RFC FA §3.7a: the UI shows "awaiting approval" and nothing else.
			out["status"] = store.UserStatusPending
		}
		out["session_expires_at"] = sessionExpiresAt(*a.session, s.cfg().ResolvedAuth).UTC()
		return out
	}
	s.whoamiApp(ctx, out)
	return out
}
