package http

// oauth_server.go — RFC FL: loomcycle as an OAuth 2.0 authorization server for
// its own apps (loomboard's web SPA, Chrome extension and desktop app).
//
// An app sends the person to GET /v1/_oauth/authorize. loomcycle signs them in
// through its own login page and gates — the app never sees a password, a
// provider secret or the gate configuration — and hands the app a single-use
// code, which POST /v1/_oauth/token exchanges (with the PKCE verifier) for a
// short-lived access token and a rotating refresh token. The access token is
// an ordinary gate-issued user token (issueGateTokenFor), so loomcycle
// authorizes every call the app makes exactly as it authorizes the user
// anywhere else.
//
// The standards it follows: authorization code with PKCE S256 only (RFC 6749,
// RFC 7636), native-app redirects (RFC 8252), current practice (RFC 9700:
// exact redirect matching, refresh rotation with reuse detection), metadata
// (RFC 8414), revocation (RFC 7009) and the iss response parameter (RFC 9207).
//
// What it is not: an OpenID Connect provider (no id_token, D1), and it has no
// consent screen (D3) — only operator-registered clients exist.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// oauthCodeTTL is an authorization code's life (RFC FL §2): long enough
	// for the redirect and the exchange, short enough that a leaked one is
	// stale by the time anyone could use it.
	oauthCodeTTL = 60 * time.Second
	// oauthSecretBytes is the entropy of a code and of a refresh token.
	oauthSecretBytes = 32
	// oauthRefreshPrefix marks a refresh token so a secret scanner and a
	// person can tell it from an access token.
	oauthRefreshPrefix = "lcrt_"
	// oauthMaxFormBytes bounds the token and revoke request bodies.
	oauthMaxFormBytes = 16 << 10
	// oauthMaxStateLen bounds the opaque state an app round-trips.
	oauthMaxStateLen = 1024
	// Throttling per (client, source), like the other unauthenticated sign-in
	// routes. Generous: a NAT'd office refreshes through one address.
	oauthThrottleWindow = 10 * time.Minute
	oauthThrottleLimit  = 300
)

// Refusal reasons for a family, in its row and the audit record.
const (
	oauthRevokeCodeReuse      = "code_reuse"
	oauthRevokeRefreshReuse   = "refresh_reuse"
	oauthRevokeClientMismatch = "client_mismatch"
	oauthRevokeByApp          = "revoked_by_app"
	oauthRevokeClientRetired  = "client_retired"
)

// hashOAuthSecret is a code's or a refresh token's row key. They are 256-bit
// random values, so a plain SHA-256 (as a session id has) is enough: a
// database read yields nothing that authenticates.
func hashOAuthSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func newOAuthSecret() (string, error) {
	b := make([]byte, oauthSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newOAuthFamilyID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "fam_" + hex.EncodeToString(b), nil
}

// pkceS256Matches reports whether verifier hashes to challenge (RFC 7636
// §4.6), compared in constant time.
func pkceS256Matches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// pkceUnreserved reports whether v is 43–128 characters of the PKCE alphabet
// (RFC 7636 §4.1). An S256 challenge is always exactly 43.
func pkceUnreserved(v string, min, max int) bool {
	if len(v) < min || len(v) > max {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// appClient resolves a client by id: a config client first, then a live
// ClientDef. ok is false when there is none (or it is retired); err is a
// store fault, which callers fail closed on.
func (s *Server) appClient(ctx context.Context, id string) (config.AppClient, bool, error) {
	if c, ok := s.cfg().ResolvedAuth.Client(id); ok {
		return c, true, nil
	}
	return s.clientDefAppClient(ctx, id)
}

// oauthIssuer is the issuer identifier (RFC 8414 §2): LOOMCYCLE_PUBLIC_URL,
// "" when it is not set.
func (s *Server) oauthIssuer() string {
	return strings.TrimRight(strings.TrimSpace(s.cfg().Env.PublicURL), "/")
}

// oauthAllowed counts one request for (client, source) and reports whether it
// may go ahead. A throttle read failure refuses.
func (s *Server) oauthAllowed(ctx context.Context, r *http.Request, client string) bool {
	h, err := s.hasher()
	if err != nil {
		return false
	}
	key := "oauth:" + h.Hash("oauth-client-source\x00"+client+"\x00"+throttleSourceKey(s.clientSource(r)))
	now := time.Now()
	if _, until, err := s.store.AuthThrottleGet(ctx, key, now); err != nil || until.After(now) {
		return false
	}
	n, _, err := s.store.AuthThrottleRecordFailure(ctx, key, now, oauthThrottleWindow, func(n int) time.Duration {
		if n >= oauthThrottleLimit {
			return oauthThrottleWindow
		}
		return 0
	})
	return err == nil && n <= oauthThrottleLimit
}

// singleParams reads a form or query, refusing a parameter sent twice (RFC
// 6749 §3.1, §3.2): which copy a server picks is exactly the kind of
// ambiguity an attacker splits two parsers with.
func singleParams(v url.Values) (map[string]string, bool) {
	out := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) != 1 {
			return nil, false
		}
		out[k] = vs[0]
	}
	return out, true
}

// ---- GET /v1/_oauth/authorize ----

// handleOAuthAuthorize starts or continues an app sign-in. Errors that cannot
// be sent back safely (an unknown client, a redirect_uri the client did not
// register) end on loomcycle's own page, so an attacker cannot use this route
// to redirect anywhere; every other error goes back to the app's registered
// redirect_uri, as RFC 6749 §4.1.2.1 asks.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	ctx := r.Context()
	if !s.authConfigured(ctx) {
		oauthErrorPage(w, http.StatusConflict, "Sign-in is off", "This server has no authentication configured, so it cannot sign you in to an app.")
		return
	}
	if s.store == nil {
		oauthErrorPage(w, http.StatusServiceUnavailable, "Sign-in is unavailable", "This server cannot keep sign-ins.")
		return
	}
	q, ok := singleParams(r.URL.Query())
	if !ok {
		oauthErrorPage(w, http.StatusBadRequest, "Invalid sign-in link", "A parameter was sent more than once.")
		return
	}
	client, ok, err := s.appClient(ctx, q["client_id"])
	if err != nil {
		slog.Error("oauth: reading the client failed", "err", err)
		oauthErrorPage(w, http.StatusServiceUnavailable, "Sign-in is unavailable", "Try again in a moment.")
		return
	}
	if !ok {
		oauthErrorPage(w, http.StatusBadRequest, "Unknown app", "The app that sent you here is not registered with this server.")
		return
	}
	redirectURI := q["redirect_uri"]
	if redirectURI == "" || !client.MatchRedirectURI(redirectURI) {
		s.recordAuthEvent(audit.Event{Action: "oauth_refused", AuthClient: client.ID, AuthReason: "redirect_uri", SourceAddr: s.clientSource(r)})
		oauthErrorPage(w, http.StatusBadRequest, "Invalid sign-in link",
			"The address this sign-in would return to is not one the app "+client.Label+" registered.")
		return
	}
	state := q["state"]
	if len(state) > oauthMaxStateLen {
		oauthErrorPage(w, http.StatusBadRequest, "Invalid sign-in link", "The sign-in state is too long.")
		return
	}
	back := func(code, desc string) { s.oauthRedirectError(w, redirectURI, state, code, desc) }
	if q["response_type"] != "code" {
		back("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q["code_challenge_method"] != "S256" {
		back("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if !pkceUnreserved(q["code_challenge"], 43, 43) {
		back("invalid_request", "code_challenge must be a base64url SHA-256 (43 characters)")
		return
	}
	if !s.oauthAllowed(ctx, r, client.ID) {
		oauthErrorPage(w, http.StatusTooManyRequests, "Too many sign-in attempts", "Wait a few minutes and try again.")
		return
	}
	// The browser's session, if it has one. Without one, the person signs in
	// on loomcycle's login page, which comes back here with this same query
	// once they have (and, through its own same-site fetch, also finds a
	// session the browser already holds: SameSite=Strict keeps the cookie off
	// the cross-site navigation that brought the person here).
	id, ok := s.sessionIDFromCookie(r)
	var (
		p  auth.Principal
		ui uiAuth
	)
	if ok {
		p, ui, ok = s.sessionPrincipal(r, id)
	}
	if !ok || ui.session == nil {
		http.Redirect(w, r, "/ui/login?"+url.Values{"oauth": {r.URL.RawQuery}}.Encode(), http.StatusSeeOther)
		return
	}
	sess := ui.session
	refuse := func(reason string) {
		s.recordAuthEvent(audit.Event{Action: "oauth_refused", ActorTenant: sess.TenantID, ActorSubject: sess.Subject,
			AuthGate: sess.Gate, AuthClient: client.ID, AuthReason: reason, SourceAddr: s.clientSource(r)})
	}
	// RFC FL D10: a tenant's client signs in only that tenant's people. Never
	// a code, and the person learns why on loomcycle's page, not the app's.
	if !client.ServesTenant(sess.TenantID) {
		refuse("tenant")
		oauthErrorPage(w, http.StatusForbidden, "This app is not for your organisation",
			"The app "+client.Label+" signs in only people of the organisation that registered it.")
		return
	}
	if ui.pending {
		refuse("pending")
		back("access_denied", "pending")
		return
	}
	user, err := s.store.UserGet(ctx, sess.TenantID, sess.Subject)
	switch {
	case isNotFound(err):
		refuse("no_user")
		back("access_denied", "no_user")
		return
	case err != nil:
		slog.Error("oauth: reading the user failed", "err", err)
		back("temporarily_unavailable", "")
		return
	case user.Status == store.UserStatusPending:
		refuse("pending")
		back("access_denied", "pending")
		return
	case user.Status != "active":
		refuse("inactive")
		back("access_denied", "")
		return
	}
	// The token the sign-in mints never holds more than this session does:
	// its scopes now, its restrictions, and the client's own restrictions.
	restr, err := auth.ParseRestrictions(client.RestrictionsJSON)
	if err == nil {
		restr, err = restr.Within(p.Restrictions)
	}
	var rj string
	if err == nil {
		rj, err = auth.MarshalRestrictions(restr)
	}
	if err != nil {
		slog.Error("oauth: the client's restrictions do not compose", "client", client.ID, "err", err)
		back("server_error", "")
		return
	}
	scopeCap := append([]string{}, p.Scopes...)
	code, err := newOAuthSecret()
	if err != nil {
		back("server_error", "")
		return
	}
	now := time.Now().UTC()
	if err := s.store.OAuthCodeCreate(ctx, store.OAuthCodeRow{
		CodeHash: hashOAuthSecret(code), ClientID: client.ID, RedirectURI: redirectURI, CodeChallenge: q["code_challenge"],
		TenantID: sess.TenantID, Subject: sess.Subject, Gate: sess.Gate, AMR: sess.AMR, SessionHash: sess.SessionHash,
		ScopeCap: scopeCap, RestrictionsJSON: rj, CreatedAt: now, ExpiresAt: now.Add(oauthCodeTTL),
	}); err != nil {
		slog.Error("oauth: storing a code failed", "err", err)
		back("server_error", "")
		return
	}
	s.recordAuthEvent(audit.Event{Action: "oauth_code_issued", ActorTenant: sess.TenantID, ActorSubject: sess.Subject,
		AuthGate: sess.Gate, AuthAMR: sess.AMR, AuthClient: client.ID, SourceAddr: s.clientSource(r)})
	s.oauthRedirect(w, redirectURI, url.Values{"code": {code}}, state)
}

// oauthRedirect sends the browser to the app's registered redirect_uri with
// params added to its query (RFC 6749 §3.1.2 keeps the registered query).
func (s *Server) oauthRedirect(w http.ResponseWriter, redirectURI string, params url.Values, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// A registered URI always parses; refuse rather than guess.
		oauthErrorPage(w, http.StatusInternalServerError, "Sign-in failed", "The app's return address could not be read.")
		return
	}
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	if state != "" {
		q.Set("state", state)
	}
	if iss := s.oauthIssuer(); iss != "" {
		q.Set("iss", iss)
	}
	u.RawQuery = q.Encode()
	// Location only: http.Redirect would also echo the URL (and the code in
	// it) into an HTML body.
	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) oauthRedirectError(w http.ResponseWriter, redirectURI, state, code, desc string) {
	p := url.Values{"error": {code}}
	if desc != "" {
		p.Set("error_description", desc)
	}
	s.oauthRedirect(w, redirectURI, p, state)
}

// oauthErrorPage is loomcycle's own page for an authorize error that must not
// go back to the app.
func oauthErrorPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<title>%s</title><style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#222}`+
		`h1{font-size:1.3rem}</style></head><body><h1>%s</h1><p>%s</p><p><a href="/ui/">Go to loomcycle</a></p></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// ---- POST /v1/_oauth/token ----

// oauthTokenError writes an RFC 6749 §5.2 error.
func oauthTokenError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}

// oauthForm reads a token or revoke request: POST, form-encoded, nothing in
// the URL (a code or token must never travel in one), each parameter once,
// and a known client. The client's CORS origin is answered here, and a
// browser request from an origin the client did not allow is refused before
// anything is consumed.
func (s *Server) oauthForm(w http.ResponseWriter, r *http.Request) (map[string]string, config.AppClient, bool) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	if r.URL.RawQuery != "" {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "parameters go in the form body, never the URL")
		return nil, config.AppClient{}, false
	}
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/x-www-form-urlencoded" {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return nil, config.AppClient{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthMaxFormBytes)
	if err := r.ParseForm(); err != nil {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "the form could not be read")
		return nil, config.AppClient{}, false
	}
	f, ok := singleParams(r.PostForm)
	if !ok {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "a parameter was sent more than once")
		return nil, config.AppClient{}, false
	}
	if s.store == nil {
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return nil, config.AppClient{}, false
	}
	client, ok, err := s.appClient(r.Context(), f["client_id"])
	if err != nil {
		slog.Error("oauth: reading the client failed", "err", err)
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return nil, config.AppClient{}, false
	}
	if !ok {
		oauthTokenError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return nil, config.AppClient{}, false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !client.OriginAllowed(origin) {
			oauthTokenError(w, http.StatusForbidden, "invalid_client", "this origin may not call the token endpoint for this client")
			return nil, config.AppClient{}, false
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
	}
	if !s.oauthAllowed(r.Context(), r, client.ID) {
		h.Set("Retry-After", strconv.Itoa(int(oauthThrottleWindow/time.Second)))
		oauthTokenError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests; retry later")
		return nil, config.AppClient{}, false
	}
	return f, client, true
}

// handleOAuthPreflight answers a CORS preflight for the token and revoke
// endpoints: an origin some client allows may POST a form. The real request
// is checked against its own client's origins again.
func (s *Server) handleOAuthPreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !s.anyClientAllowsOrigin(r.Context(), origin) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "POST")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// anyClientAllowsOrigin reports whether a config client or a live ClientDef
// lists origin.
func (s *Server) anyClientAllowsOrigin(ctx context.Context, origin string) bool {
	for _, c := range s.cfg().ResolvedAuth.Clients {
		if c.OriginAllowed(origin) {
			return true
		}
	}
	return s.clientDefsAllowOrigin(ctx, origin)
}

// oauthTokenResponse is RFC 6749 §5.1.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	f, client, ok := s.oauthForm(w, r)
	if !ok {
		return
	}
	switch f["grant_type"] {
	case "authorization_code":
		s.oauthCodeGrant(w, r, f, client)
	case "refresh_token":
		s.oauthRefreshGrant(w, r, f, client)
	case "":
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		oauthTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

// oauthCodeGrant exchanges a code. The code is consumed before anything is
// checked, so whatever the outcome it is used up; a second presentation finds
// it used and revokes the family the first one created (RFC FL §5.3).
func (s *Server) oauthCodeGrant(w http.ResponseWriter, r *http.Request, f map[string]string, client config.AppClient) {
	ctx := r.Context()
	codeStr, redirectURI, verifier := f["code"], f["redirect_uri"], f["code_verifier"]
	if codeStr == "" || redirectURI == "" || verifier == "" {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "code, redirect_uri and code_verifier are required")
		return
	}
	if !pkceUnreserved(verifier, 43, 128) {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "code_verifier must be 43 to 128 characters of the PKCE alphabet")
		return
	}
	famID, err := newOAuthFamilyID()
	if err != nil {
		oauthTokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	now := time.Now().UTC()
	code, first, err := s.store.OAuthCodeConsume(ctx, hashOAuthSecret(codeStr), famID, now)
	if err != nil {
		if isNotFound(err) {
			oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
			return
		}
		slog.Error("oauth: consuming a code failed", "err", err)
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	refused := func(reason string) {
		s.recordAuthEvent(audit.Event{Action: "oauth_token_refused", TargetTenant: code.TenantID, TargetSubject: code.Subject,
			AuthGate: code.Gate, AuthClient: client.ID, AuthReason: reason, SourceAddr: s.clientSource(r)})
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
	}
	if !first {
		// A replayed code: whoever has it is not (only) the app. End the
		// sign-in it produced, on every replica.
		s.revokeAppFamily(ctx, code.ClientID, code.FamilyID, oauthRevokeCodeReuse)
		s.recordAuthEvent(audit.Event{Action: "oauth_code_reuse", TargetTenant: code.TenantID, TargetSubject: code.Subject,
			AuthGate: code.Gate, AuthClient: code.ClientID, AuthRecordID: code.FamilyID, SourceAddr: s.clientSource(r)})
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
		return
	}
	switch {
	case code.ClientID != client.ID:
		refused("client")
		return
	case !now.Before(code.ExpiresAt):
		refused("expired")
		return
	case code.RedirectURI != redirectURI:
		refused("redirect_uri")
		return
	case !pkceS256Matches(verifier, code.CodeChallenge):
		refused("pkce")
		return
	case !client.ServesTenant(code.TenantID):
		refused("tenant")
		return
	case !s.codeSessionLive(ctx, code, now):
		// The person signed out (or the session ended) between approving
		// the sign-in and the app exchanging the code.
		refused("session")
		return
	}
	fam := store.OAuthFamilyRow{
		FamilyID: famID, ClientID: client.ID, TenantID: code.TenantID, Subject: code.Subject, Gate: code.Gate,
		AMR: code.AMR, ScopeCap: code.ScopeCap, RestrictionsJSON: code.RestrictionsJSON, CreatedAt: now, LastUsedAt: now,
		IdleExpiresAt: now.Add(client.RefreshIdle), AbsoluteExpiresAt: now.Add(client.RefreshAbs),
	}
	if err := s.store.OAuthFamilyCreate(ctx, fam); err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			// Revoked by a replay of this code before it existed.
			refused("revoked")
			return
		}
		slog.Error("oauth: creating a refresh family failed", "err", err)
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	s.oauthIssue(w, r, client, fam, "authorization_code")
}

// codeSessionLive reports whether the session that approved code still
// stands, for the same person. A store fault refuses.
func (s *Server) codeSessionLive(ctx context.Context, code store.OAuthCodeRow, now time.Time) bool {
	row, err := s.store.UISessionGet(ctx, code.SessionHash)
	if err != nil {
		return false
	}
	return row.RevokedAt.IsZero() && now.Before(sessionExpiresAt(row, s.cfg().ResolvedAuth)) &&
		row.TenantID == code.TenantID && row.Subject == code.Subject
}

// oauthRefreshGrant rotates a refresh token. A used one presented again
// revokes its whole family (reuse detection, RFC 9700 §4.14).
func (s *Server) oauthRefreshGrant(w http.ResponseWriter, r *http.Request, f map[string]string, client config.AppClient) {
	ctx := r.Context()
	rt := f["refresh_token"]
	if rt == "" {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	now := time.Now().UTC()
	tok, first, err := s.store.OAuthRefreshUse(ctx, hashOAuthSecret(rt), now)
	if err == nil {
		var fam store.OAuthFamilyRow
		fam, err = s.store.OAuthFamilyGet(ctx, tok.FamilyID)
		if err == nil {
			s.oauthRefreshFamily(w, r, client, fam, first, now)
			return
		}
	}
	if isNotFound(err) {
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
		return
	}
	slog.Error("oauth: reading a refresh token failed", "err", err)
	oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
}

func (s *Server) oauthRefreshFamily(w http.ResponseWriter, r *http.Request, client config.AppClient, fam store.OAuthFamilyRow, first bool, now time.Time) {
	ctx := r.Context()
	refused := func(reason string) {
		s.recordAuthEvent(audit.Event{Action: "oauth_token_refused", TargetTenant: fam.TenantID, TargetSubject: fam.Subject,
			AuthGate: fam.Gate, AuthClient: client.ID, AuthRecordID: fam.FamilyID, AuthReason: reason, SourceAddr: s.clientSource(r)})
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
	}
	if fam.ClientID != client.ID {
		// A refresh token is bound to its client (RFC 6749 §10.4); another
		// client holding it means it leaked.
		s.revokeAppFamily(ctx, fam.ClientID, fam.FamilyID, oauthRevokeClientMismatch)
		refused("client")
		return
	}
	if !first {
		s.revokeAppFamily(ctx, fam.ClientID, fam.FamilyID, oauthRevokeRefreshReuse)
		s.recordAuthEvent(audit.Event{Action: "oauth_refresh_reuse", TargetTenant: fam.TenantID, TargetSubject: fam.Subject,
			AuthGate: fam.Gate, AuthClient: fam.ClientID, AuthRecordID: fam.FamilyID, SourceAddr: s.clientSource(r)})
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
		return
	}
	if !fam.RevokedAt.IsZero() {
		refused("revoked")
		return
	}
	if !now.Before(appFamilyExpiresAt(fam, client)) {
		refused("expired")
		return
	}
	if !client.ServesTenant(fam.TenantID) {
		refused("tenant")
		return
	}
	idle := now.Add(client.RefreshIdle)
	if idle.After(fam.AbsoluteExpiresAt) {
		idle = fam.AbsoluteExpiresAt
	}
	if err := s.store.OAuthFamilyTouch(ctx, fam.FamilyID, now, idle); err != nil {
		slog.Warn("oauth: recording a refresh failed", "err", err)
	}
	s.oauthIssue(w, r, client, fam, "refresh_token")
}

// appFamilyExpiresAt is when a family's refresh stops working: its own idle
// and absolute times, held to the client's current lifetimes too, so an
// operator who shortens them shortens the sign-ins already issued.
func appFamilyExpiresAt(fam store.OAuthFamilyRow, client config.AppClient) time.Time {
	end := fam.AbsoluteExpiresAt
	for _, t := range []time.Time{fam.IdleExpiresAt, fam.CreatedAt.Add(client.RefreshAbs), fam.LastUsedAt.Add(client.RefreshIdle)} {
		if t.Before(end) {
			end = t
		}
	}
	return end
}

// oauthIssue mints an access token and a new refresh token in fam and writes
// the token response. It re-reads the family afterwards: a replay that
// revoked it while the tokens were being minted must not leave them live.
func (s *Server) oauthIssue(w http.ResponseWriter, r *http.Request, client config.AppClient, fam store.OAuthFamilyRow, grant string) {
	ctx := r.Context()
	restr, err := auth.ParseRestrictions(fam.RestrictionsJSON)
	if err != nil {
		slog.Error("oauth: a family's restrictions are unreadable (refusing)", "family", fam.FamilyID, "err", err)
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
		return
	}
	scopeCap := fam.ScopeCap
	if scopeCap == nil {
		scopeCap = []string{}
	}
	access, row, err := s.issueGateTokenFor(ctx, gateTokenSpec{
		tenant: fam.TenantID, subject: fam.Subject, gate: fam.Gate, ttl: client.AccessTTL, restrictions: restr,
		scopeCap: scopeCap, oauthClient: client.ID, oauthFamily: fam.FamilyID,
	})
	if err != nil {
		if errors.Is(err, errGateTokenUser) || errors.Is(err, errGateTokenGate) {
			// The user is no longer active, holds no scope the session did, or
			// the gate that signed them in was taken out of the config.
			s.recordAuthEvent(audit.Event{Action: "oauth_token_refused", TargetTenant: fam.TenantID, TargetSubject: fam.Subject,
				AuthGate: fam.Gate, AuthClient: client.ID, AuthRecordID: fam.FamilyID, AuthReason: "user", SourceAddr: s.clientSource(r)})
			oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
			return
		}
		slog.Error("oauth: minting an access token failed", "err", err)
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	refresh, err := newOAuthSecret()
	if err == nil {
		refresh = oauthRefreshPrefix + refresh
		err = s.store.OAuthRefreshCreate(ctx, store.OAuthRefreshRow{TokenHash: hashOAuthSecret(refresh), FamilyID: fam.FamilyID, CreatedAt: time.Now().UTC()})
	}
	if err != nil {
		s.retireAppToken(ctx, row.DefID)
		slog.Error("oauth: storing a refresh token failed", "err", err)
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	if cur, err := s.store.OAuthFamilyGet(ctx, fam.FamilyID); err != nil || !cur.RevokedAt.IsZero() {
		s.revokeAppFamily(ctx, client.ID, fam.FamilyID, oauthRevokeCodeReuse)
		oauthTokenError(w, http.StatusBadRequest, "invalid_grant", "")
		return
	}
	s.recordAuthEvent(audit.Event{Action: "oauth_token_issued", TargetTenant: fam.TenantID, TargetSubject: fam.Subject,
		AuthGate: fam.Gate, AuthAMR: fam.AMR, AuthClient: client.ID, AuthRecordID: fam.FamilyID, AuthReason: grant,
		TargetDefID: row.DefID, SourceAddr: s.clientSource(r)})
	writeJSON(w, http.StatusOK, oauthTokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(time.Until(row.ExpiresAt).Round(time.Second) / time.Second),
		RefreshToken: refresh,
	})
}

// revokeAppFamily ends a sign-in: the family is revoked (a tombstone if it
// does not exist yet) and its access tokens retired, and every replica's
// token cache is flushed so none keeps answering for them.
func (s *Server) revokeAppFamily(ctx context.Context, clientID, familyID, reason string) {
	if familyID == "" {
		return
	}
	now := time.Now().UTC()
	if _, err := s.store.OAuthFamilyRevoke(ctx, familyID, reason, now); err != nil {
		slog.Error("oauth: revoking a family failed", "family", familyID, "err", err)
	}
	if clientID != "" {
		if _, err := s.store.OAuthTokenRetire(ctx, clientID, familyID, now); err != nil {
			slog.Error("oauth: retiring a family's tokens failed", "family", familyID, "err", err)
		}
	}
	s.invalidateTokenCache(ctx)
}

// retireAppToken retires one access token at once.
func (s *Server) retireAppToken(ctx context.Context, defID string) {
	if err := s.store.OperatorTokenDefSetRetiredAt(ctx, defID, time.Now().UTC()); err != nil {
		slog.Error("oauth: retiring an access token failed", "def_id", defID, "err", err)
	}
	s.invalidateTokenCache(ctx)
}

// ---- POST /v1/_oauth/revoke (RFC 7009) ----

// handleOAuthRevoke revokes a refresh token (its whole family) or an access
// token the client was issued. It answers 200 for a token it does not know or
// that belongs to another client: the answer must not tell a caller which
// tokens exist (RFC 7009 §2.2).
func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	f, client, ok := s.oauthForm(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	tok := f["token"]
	if tok == "" {
		oauthTokenError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	if strings.HasPrefix(tok, oauthRefreshPrefix) {
		rt, err := s.store.OAuthRefreshGet(ctx, hashOAuthSecret(tok))
		if err != nil && !isNotFound(err) {
			slog.Error("oauth: reading a refresh token failed", "err", err)
			oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
			return
		}
		if err == nil {
			fam, err := s.store.OAuthFamilyGet(ctx, rt.FamilyID)
			if err == nil && fam.ClientID == client.ID {
				s.revokeAppFamily(ctx, client.ID, fam.FamilyID, oauthRevokeByApp)
				s.recordAuthEvent(audit.Event{Action: "oauth_revoked", TargetTenant: fam.TenantID, TargetSubject: fam.Subject,
					AuthGate: fam.Gate, AuthClient: client.ID, AuthRecordID: fam.FamilyID, AuthReason: "refresh_token",
					SourceAddr: s.clientSource(r)})
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	h, err := s.hasher()
	if err != nil {
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, h.Hash(tok))
	if err != nil && !isNotFound(err) {
		slog.Error("oauth: reading an access token failed", "err", err)
		oauthTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	if err == nil && row.OAuthClient == client.ID && row.ValidAt(time.Now()) {
		s.retireAppToken(ctx, row.DefID)
		s.recordAuthEvent(audit.Event{Action: "oauth_revoked", TargetTenant: row.TenantID, TargetSubject: row.Subject,
			AuthGate: row.IssuedByGate, AuthClient: client.ID, AuthRecordID: row.OAuthFamily, AuthReason: "access_token",
			TargetDefID: row.DefID, SourceAddr: s.clientSource(r)})
	}
	w.WriteHeader(http.StatusOK)
}

// ---- GET /.well-known/oauth-authorization-server (RFC 8414) ----

func (s *Server) handleOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	iss := s.oauthIssuer()
	if iss == "" {
		writeJSONError(w, http.StatusNotFound, "oauth_issuer_unset",
			"set LOOMCYCLE_PUBLIC_URL to this server's public address to publish its authorization server metadata")
		return
	}
	// Public, credential-free metadata a browser app reads cross-origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         iss,
		"authorization_endpoint":                         iss + "/v1/_oauth/authorize",
		"token_endpoint":                                 iss + "/v1/_oauth/token",
		"revocation_endpoint":                            iss + "/v1/_oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---- the login page and whoami ----

// loginApp is the app a login page is signing a person in to, from the
// authorize query it was handed (?oauth=). The page names the app and where
// the person will be sent; nil when the query names no registered client and
// redirect_uri.
func (s *Server) loginApp(r *http.Request) (string, string, bool) {
	raw := r.URL.Query().Get("oauth")
	if raw == "" {
		return "", "", false
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", "", false
	}
	c, ok, err := s.appClient(r.Context(), q.Get("client_id"))
	if err != nil || !ok || !c.MatchRedirectURI(q.Get("redirect_uri")) {
		return "", "", false
	}
	return c.Label, redirectDestination(q.Get("redirect_uri")), true
}

// redirectDestination is how the login page names where a sign-in goes: the
// web origin, "this computer" for a loopback app, or the app scheme.
func redirectDestination(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
		return u.Host
	case "http":
		return "an app on this computer"
	}
	return u.Scheme + ":"
}

// whoamiApp adds an app token's sign-in to /v1/_me's auth block: the app,
// and the gate and methods the person signed in with.
func (s *Server) whoamiApp(ctx context.Context, out map[string]any) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.OAuthClient == "" {
		return
	}
	out["app"] = p.OAuthClient
	out["gate"] = p.IssuedByGate
	if s.store == nil || p.OAuthFamily == "" {
		return
	}
	if fam, err := s.store.OAuthFamilyGet(ctx, p.OAuthFamily); err == nil {
		out["amr"] = fam.AMR
	}
}

// appTokenStands is the on-use half of a client's removal: an app's access
// token authenticates only while its client is registered and still serves
// the token's tenant. Fails closed on a store fault.
func (s *Server) appTokenStands(ctx context.Context, p auth.Principal) bool {
	if p.OAuthClient == "" {
		return true
	}
	c, ok, err := s.appClient(ctx, p.OAuthClient)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("auth: reading an app token's client failed (refusing)", "err", err)
		}
		return false
	}
	return ok && c.ServesTenant(p.TenantID)
}
