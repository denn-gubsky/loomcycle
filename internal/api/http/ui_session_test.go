package http

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/webui"
)

// RFC FA P0: Web UI sessions. Each test names the §8 control it pins.

const testUA = "Mozilla/5.0 loomcycle-test"

// sessionTestServer is tokenAuthServer with an audit sink, in token mode.
func sessionTestServer(t *testing.T, legacy string) (*Server, store.Store, *recordingSink) {
	t.Helper()
	s, st := tokenAuthServer(t, legacy)
	sink := &recordingSink{}
	s.tokenAudit = sink
	return s, st, sink
}

func strictPolicy() config.AuthPolicy {
	return config.AuthPolicy{Mode: config.AuthModeStrict, Gates: []config.AuthGate{{Name: "paste", Kind: "token", Label: "Access token"}}}
}

type loginOpt func(*http.Request)

func overTLS(r *http.Request) { r.TLS = &tls.ConnectionState{} }

// loginReq is the SPA's POST /ui/session.
func loginReq(bearer string, opts ...loginOpt) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/ui/session", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("User-Agent", testUA)
	for _, o := range opts {
		o(r)
	}
	return r
}

func doLogin(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleUILogin(rec, r)
	return rec
}

// cookieNamed returns the response's Set-Cookie for name (nil when absent).
func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// mustSession logs in and returns the session cookie.
func mustSession(t *testing.T, s *Server, bearer string, opts ...loginOpt) *http.Cookie {
	t.Helper()
	rec := doLogin(t, s, loginReq(bearer, opts...))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{sessionCookieSecure, sessionCookiePlain} {
		if c := cookieNamed(rec, name); c != nil && c.Value != "" {
			return c
		}
	}
	t.Fatalf("login set no session cookie: %v", rec.Result().Cookies())
	return nil
}

// sessionReq is a request the SPA makes with its session cookie.
func sessionReq(method, path string, c *http.Cookie, opts ...loginOpt) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.AddCookie(c)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("User-Agent", testUA)
	r.Header.Set(uiRequestHeader, "1")
	for _, o := range opts {
		o(r)
	}
	return r
}

// through runs r through authMiddleware and reports the status and whether the
// handler was reached.
func through(s *Server, r *http.Request) (int, bool) {
	var reached bool
	rec := httptest.NewRecorder()
	s.authMiddleware(passthroughHandler(&reached)).ServeHTTP(rec, r)
	return rec.Code, reached
}

func actions(sink *recordingSink) []string {
	out := make([]string, 0, len(sink.events))
	for _, e := range sink.events {
		out = append(out, e.Action)
	}
	return out
}

func lastEvent(t *testing.T, sink *recordingSink, action string) audit.Event {
	t.Helper()
	for i := len(sink.events) - 1; i >= 0; i-- {
		if sink.events[i].Action == action {
			return sink.events[i]
		}
	}
	t.Fatalf("no %s audit event; got %v", action, actions(sink))
	return audit.Event{}
}

// sessionRows counts every stored session (live or not): deleting with a
// far-future cut-off removes them all and reports how many there were.
func sessionRows(t *testing.T, st store.Store) int {
	t.Helper()
	n, err := st.UISessionDeleteExpired(context.Background(), time.Now().Add(100*365*24*time.Hour))
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

// §8 disclosure / audit: an invalid bearer on login is an opaque 401, recorded
// as auth_login_failed, and leaves no session behind.
func TestUILogin_InvalidBearerIs401OpaqueAuditedAndCreatesNoSession(t *testing.T) {
	s, st, sink := sessionTestServer(t, "legacy-secret")
	rec := doLogin(t, s, loginReq("wrong-token"))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("status = %d cookies = %v, want 401 and none", rec.Code, rec.Result().Cookies())
	}
	if body := rec.Body.String(); !strings.Contains(body, "invalid credentials") || strings.Contains(body, "wrong-token") {
		t.Errorf("body = %s, want the opaque message and no echo", body)
	}
	ev := lastEvent(t, sink, "auth_login_failed")
	if ev.AuthGate != "token" || ev.SourceAddr == "" || ev.ActorSubject != "" {
		t.Errorf("auth_login_failed = %+v", ev)
	}
	if n := sessionRows(t, st); n != 0 {
		t.Errorf("%d session rows after a failed login, want 0", n)
	}
}

// §8 bearer in the browser: over TLS the cookie is __Host-lc_session, Secure,
// HttpOnly, SameSite=Strict, Path=/, no Domain, no Max-Age — and its value is
// an opaque id, not the bearer. The legacy bearer cookie is deleted.
func TestUILogin_CookieCensusOverTLS(t *testing.T) {
	s, _, sink := sessionTestServer(t, "legacy-secret")
	rec := doLogin(t, s, loginReq("legacy-secret", overTLS))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body.String())
	}
	c := cookieNamed(rec, sessionCookieSecure)
	if c == nil {
		t.Fatalf("no %s cookie: %v", sessionCookieSecure, rec.Result().Cookies())
	}
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Errorf("cookie attributes = %+v", c)
	}
	if c.Value == "legacy-secret" || strings.Contains(c.Value, "legacy-secret") {
		t.Fatal("the session cookie carries the bearer")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err != nil || len(raw) != sessionIDBytes {
		t.Errorf("cookie value is not a %d-byte id", sessionIDBytes)
	}
	if old := cookieNamed(rec, webui.LegacyBearerCookie); old == nil || old.MaxAge >= 0 || old.Value != "" {
		t.Errorf("legacy cookie not deleted: %+v", old)
	}
	if plain := cookieNamed(rec, sessionCookiePlain); plain != nil {
		t.Errorf("an lc_session cookie was set over TLS: %+v", plain)
	}
	ev := lastEvent(t, sink, "auth_login")
	if ev.ActorTenant != "default" || ev.ActorSubject != "default" || ev.AuthGate != "token" || len(ev.AuthAMR) != 1 || ev.SourceAddr == "" {
		t.Errorf("auth_login = %+v", ev)
	}
}

// Behind a TLS terminator r.TLS is nil. LOOMCYCLE_PUBLIC_URL=https://… plus a
// request that came through the terminator — the public Host (Cloudflare
// Tunnel) or a loopback peer (tailscale serve) — gets the __Host- cookie; the
// same server reached directly on the LAN gets the plain one, which the
// browser would otherwise drop.
func TestUILogin_TLSTerminatorIssuesHostCookie(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	s.cfg().Env.PublicURL = "https://app.loomcycle.cloud"
	cases := []struct {
		name, host, remote, want string
	}{
		{"cloudflare tunnel keeps the public host", "app.loomcycle.cloud", "172.18.0.5:41000", sessionCookieSecure},
		{"tailscale serve proxies from loopback", "127.0.0.1:8787", "127.0.0.1:52000", sessionCookieSecure},
		{"direct LAN access stays plain", "192.168.0.77:8787", "192.168.0.10:50000", sessionCookiePlain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustSession(t, s, "legacy-secret", func(r *http.Request) { r.Host, r.RemoteAddr = tc.host, tc.remote })
			if c.Name != tc.want || c.Secure != (tc.want == sessionCookieSecure) {
				t.Errorf("cookie = %s Secure=%v, want %s", c.Name, c.Secure, tc.want)
			}
		})
	}
	// An http:// public URL is not a terminator.
	s.cfg().Env.PublicURL = "http://app.loomcycle.cloud"
	if c := mustSession(t, s, "legacy-secret", func(r *http.Request) { r.Host = "app.loomcycle.cloud" }); c.Name != sessionCookiePlain {
		t.Errorf("http public URL gave %s", c.Name)
	}
}

// Token mode over plain HTTP (a LAN install): login works with lc_session, not
// Secure, and the session authenticates.
func TestUISession_TokenModePlainHTTPUsesLcSession(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	c := mustSession(t, s, "legacy-secret")
	if c.Name != sessionCookiePlain || c.Secure {
		t.Fatalf("cookie = %+v, want plain lc_session", c)
	}
	if code, reached := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusOK || !reached {
		t.Errorf("session GET = %d reached=%v", code, reached)
	}
	// A plain cookie is never read over TLS, where it could have been
	// planted from an http:// page.
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c, overTLS)); code != http.StatusUnauthorized {
		t.Errorf("lc_session over TLS = %d, want 401", code)
	}
}

// §8 CSRF: a session-cookie POST from another origin is refused; a same-origin
// one without X-Loomcycle-UI is refused; with both it passes. An
// Authorization-header POST needs neither.
func TestUISession_CSRFChecks(t *testing.T) {
	s, st, _ := sessionTestServer(t, "legacy-secret")
	seedToken(t, st, "lct_alice", "acme", "alice", []string{auth.ScopeRunsCreate}, time.Time{})
	c := mustSession(t, s, "legacy-secret")

	crossSite := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
	noHeader := func(r *http.Request) { r.Header.Del(uiRequestHeader) }
	noFetchMeta := func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }
	sameOriginHdr := func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }
	foreignOrigin := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }

	cases := []struct {
		name string
		opts []loginOpt
		want int
	}{
		{"cross-site with a valid cookie", []loginOpt{crossSite}, http.StatusForbidden},
		{"same-origin without the UI header", []loginOpt{noHeader}, http.StatusForbidden},
		{"no Sec-Fetch-Site and no Origin", []loginOpt{noFetchMeta}, http.StatusForbidden},
		{"no Sec-Fetch-Site, foreign Origin", []loginOpt{noFetchMeta, foreignOrigin}, http.StatusForbidden},
		{"no Sec-Fetch-Site, same Origin", []loginOpt{noFetchMeta, sameOriginHdr}, http.StatusOK},
		{"same-origin with the UI header", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reached := through(s, sessionReq(http.MethodPost, "/v1/runs", c, tc.opts...))
			if code != tc.want || reached != (tc.want == http.StatusOK) {
				t.Errorf("status = %d reached=%v, want %d", code, reached, tc.want)
			}
		})
	}
	// Bearer clients are not browsers: no header, no origin needed.
	r := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	r.Header.Set("Authorization", "Bearer lct_alice")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if code, reached := through(s, r); code != http.StatusOK || !reached {
		t.Errorf("Authorization POST = %d reached=%v, want 200", code, reached)
	}
	// The legacy bearer cookie is a cookie too.
	legacy := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	legacy.AddCookie(&http.Cookie{Name: webui.LegacyBearerCookie, Value: "legacy-secret"})
	legacy.Header.Set("Sec-Fetch-Site", "cross-site")
	if code, _ := through(s, legacy); code != http.StatusForbidden {
		t.Errorf("cross-site POST with the legacy cookie = %d, want 403", code)
	}
}

// §8 login CSRF: the login route needs a same-origin request; a missing
// Sec-Fetch-Site no longer passes on its own.
func TestUILogin_RequiresSameOrigin(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	for name, opt := range map[string]loginOpt{
		"cross-site":            func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"no fetch metadata":     func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"foreign Origin only":   func(r *http.Request) { r.Header.Del("Sec-Fetch-Site"); r.Header.Set("Origin", "https://evil.example") },
		"same-site, not origin": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
	} {
		if rec := doLogin(t, s, loginReq("legacy-secret", opt)); rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: status %d cookies %v, want 403 and none", name, rec.Code, rec.Result().Cookies())
		}
	}
	ok := func(r *http.Request) { r.Header.Del("Sec-Fetch-Site"); r.Header.Set("Origin", "http://"+r.Host) }
	if rec := doLogin(t, s, loginReq("legacy-secret", ok)); rec.Code != http.StatusNoContent {
		t.Errorf("same Origin without fetch metadata: status %d, want 204", rec.Code)
	}
}

// §8 fixation: a login over an existing session ends it; the old id is not
// accepted afterwards, and the new id differs.
func TestUISession_ReLoginRevokesThePreviousSession(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	first := mustSession(t, s, "legacy-secret")
	second := mustSession(t, s, "legacy-secret", func(r *http.Request) { r.AddCookie(first) })
	if first.Value == second.Value {
		t.Fatal("a login over a session kept its id")
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", first)); code != http.StatusUnauthorized {
		t.Errorf("pre-login id after re-login = %d, want 401", code)
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", second)); code != http.StatusOK {
		t.Errorf("new id = %d, want 200", code)
	}
}

// §8 theft: the row's stored hash, presented as a cookie in either encoding,
// does not authenticate.
func TestUISession_StoredHashDoesNotAuthenticate(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	c := mustSession(t, s, "legacy-secret")
	raw, _ := base64.RawURLEncoding.DecodeString(c.Value)
	hash := sessionHash(raw)
	hashBytes, _ := hex.DecodeString(hash)
	for _, v := range []string{hash, base64.RawURLEncoding.EncodeToString(hashBytes)} {
		forged := &http.Cookie{Name: c.Name, Value: v}
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", forged)); code != http.StatusUnauthorized {
			t.Errorf("stored hash as cookie %q = %d, want 401", v[:8], code)
		}
	}
}

// §8 idle and absolute timeouts, enforced server-side, each ending the
// session once (audited auth_session_expired).
func TestUISession_IdleAndAbsoluteExpiry(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		s, st, sink := sessionTestServer(t, "legacy-secret")
		c := mustSession(t, s, "legacy-secret")
		raw, _ := base64.RawURLEncoding.DecodeString(c.Value)
		past := time.Now().Add(-time.Minute)
		if err := st.UISessionTouch(context.Background(), sessionHash(raw), past, past); err != nil {
			t.Fatal(err)
		}
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
			t.Errorf("idle-expired session = %d, want 401", code)
		}
		if ev := lastEvent(t, sink, "auth_session_expired"); ev.AuthReason != "expired" || ev.ActorSubject != "default" {
			t.Errorf("event = %+v", ev)
		}
	})
	t.Run("absolute, shortened by the operator", func(t *testing.T) {
		s, _, sink := sessionTestServer(t, "legacy-secret")
		c := mustSession(t, s, "legacy-secret")
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusOK {
			t.Fatalf("fresh session = %d", code)
		}
		s.cfg().ResolvedAuth.Absolute = time.Millisecond
		time.Sleep(5 * time.Millisecond)
		for i := 0; i < 2; i++ {
			if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
				t.Errorf("past absolute lifetime = %d, want 401", code)
			}
		}
		n := 0
		for _, a := range actions(sink) {
			if a == "auth_session_expired" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("auth_session_expired recorded %d times, want once", n)
		}
	})
}

// §8 binding: a cookie presented by another browser (User-Agent) ends the
// session, for the original browser too.
func TestUISession_UserAgentChangeEndsTheSession(t *testing.T) {
	s, _, sink := sessionTestServer(t, "legacy-secret")
	c := mustSession(t, s, "legacy-secret")
	other := func(r *http.Request) { r.Header.Set("User-Agent", "curl/8.0") }
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c, other)); code != http.StatusUnauthorized {
		t.Errorf("other UA = %d, want 401", code)
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
		t.Errorf("original UA after the mismatch = %d, want 401 (session ended)", code)
	}
	if ev := lastEvent(t, sink, "auth_session_revoked"); ev.AuthReason != "user_agent_changed" {
		t.Errorf("event = %+v", ev)
	}
}

// Invariant (c): a session made from a minted token stops authenticating once
// the token is retired, and carries the token's principal, never more.
func TestUISession_EndsWhenItsTokenIsRetired(t *testing.T) {
	s, st, sink := sessionTestServer(t, "legacy-secret")
	seedToken(t, st, "lct_alice", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	c := mustSession(t, s, "lct_alice")

	var got auth.Principal
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.PrincipalFromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), sessionReq(http.MethodGet, "/v1/_me", c))
	if got.TenantID != "acme" || got.Subject != "alice" || got.TokenDefID != "def_alice" || auth.HasScope(got.Scopes, auth.ScopeAdmin) {
		t.Fatalf("session principal = %+v, want the token's own", got)
	}
	if err := st.OperatorTokenDefSetRetiredAt(context.Background(), "def_alice", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
		t.Errorf("session after its token retired = %d, want 401", code)
	}
	if ev := lastEvent(t, sink, "auth_session_revoked"); ev.AuthReason != "credential_invalid" || ev.ActorSubject != "alice" {
		t.Errorf("event = %+v", ev)
	}
}

// Invariant (c) for config credentials: changing LOOMCYCLE_AUTH_TOKEN, or
// removing a declared principal, ends the sessions made from them.
func TestUISession_EndsWhenItsConfigCredentialChanges(t *testing.T) {
	t.Run("legacy secret changed", func(t *testing.T) {
		s, _, _ := sessionTestServer(t, "legacy-secret")
		c := mustSession(t, s, "legacy-secret")
		s.cfg().Env.AuthToken = "rotated-secret"
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
			t.Errorf("session after the legacy secret changed = %d, want 401", code)
		}
	})
	t.Run("declared principal removed", func(t *testing.T) {
		s, _, _ := sessionTestServer(t, "legacy-secret")
		s.cfg().ResolvedPrincipals = []auth.DeclaredPrincipal{{
			Secret:    "cfg-secret",
			Principal: auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}, TokenDefID: "cfg:ops"},
		}}
		c := mustSession(t, s, "cfg-secret")
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusOK {
			t.Fatalf("principal session = %d", code)
		}
		s.cfg().ResolvedPrincipals[0].Secret = "new-secret"
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
			t.Errorf("session after the principal's secret changed = %d, want 401", code)
		}
		s.cfg().ResolvedPrincipals = nil
		if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
			t.Errorf("session after the principal was removed = %d, want 401", code)
		}
	})
}

// §8 logout: the row is revoked on the server (the old cookie no longer
// authenticates), every UI cookie is cleared and Clear-Site-Data is sent. A
// cross-origin or header-less logout is refused.
func TestUILogout_RevokesServerSideAndClearsSiteData(t *testing.T) {
	s, _, sink := sessionTestServer(t, "legacy-secret")
	c := mustSession(t, s, "legacy-secret")

	refused := httptest.NewRecorder()
	r := sessionReq(http.MethodPost, "/v1/_auth/logout", c, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	s.handleAuthLogout(refused, r)
	if refused.Code != http.StatusForbidden {
		t.Errorf("cross-site logout = %d, want 403", refused.Code)
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusOK {
		t.Fatalf("session after a refused logout = %d, want still valid", code)
	}

	rec := httptest.NewRecorder()
	s.handleAuthLogout(rec, sessionReq(http.MethodPost, "/v1/_auth/logout", c))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Clear-Site-Data"); got != `"cookies", "storage"` {
		t.Errorf("Clear-Site-Data = %q", got)
	}
	for _, name := range []string{sessionCookieSecure, sessionCookiePlain, webui.LegacyBearerCookie} {
		if ck := cookieNamed(rec, name); ck == nil || ck.MaxAge >= 0 {
			t.Errorf("logout did not clear %s: %+v", name, ck)
		}
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
		t.Errorf("old cookie after logout = %d, want 401", code)
	}
	if ev := lastEvent(t, sink, "auth_logout"); ev.ActorSubject != "default" || ev.AuthGate != "token" {
		t.Errorf("auth_logout = %+v", ev)
	}
}

// Token mode keeps reading the legacy bearer cookie for one release (decision
// 2); strict mode never does, nor the WebSocket subprotocol bearer.
func TestUISession_StrictModeIgnoresLegacyCookieAndSubprotocolBearer(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	legacy := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/_me", nil)
		r.AddCookie(&http.Cookie{Name: webui.LegacyBearerCookie, Value: "legacy-secret"})
		return r
	}
	ws := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/client-tools", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Protocol", clientToolSubprotocol+", bearer.legacy-secret")
		return r
	}
	if code, _ := through(s, legacy()); code != http.StatusOK {
		t.Errorf("token mode, legacy cookie = %d, want 200", code)
	}
	if code, _ := through(s, ws()); code != http.StatusOK {
		t.Errorf("token mode, subprotocol bearer = %d, want 200", code)
	}
	s.cfg().ResolvedAuth = strictPolicy()
	if code, _ := through(s, legacy()); code != http.StatusUnauthorized {
		t.Errorf("strict mode, legacy cookie = %d, want 401", code)
	}
	if code, _ := through(s, ws()); code != http.StatusUnauthorized {
		t.Errorf("strict mode, subprotocol bearer = %d, want 401", code)
	}
}

// Strict mode over plain HTTP refuses to create a session unless the operator
// set LOOMCYCLE_UI_INSECURE_COOKIES=1 — which works on any host (a tailnet
// address), with the plain cookie. Over TLS it needs neither.
func TestUILogin_StrictModePlainHTTPNeedsInsecureCookies(t *testing.T) {
	s, st, _ := sessionTestServer(t, "legacy-secret")
	s.cfg().ResolvedAuth = strictPolicy()
	tailnet := func(r *http.Request) {
		r.Host, r.RemoteAddr = "loomcycle.tail1234.ts.net:8787", "100.101.102.103:51000"
	}

	rec := doLogin(t, s, loginReq("legacy-secret", tailnet))
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 || !strings.Contains(rec.Body.String(), "insecure_transport") {
		t.Fatalf("strict plain HTTP login = %d %s cookies=%v, want 403 insecure_transport", rec.Code, rec.Body.String(), rec.Result().Cookies())
	}
	if n := sessionRows(t, st); n != 0 {
		t.Errorf("%d rows after a refused login", n)
	}
	lc := s.loginConfig(loginReq("", tailnet))
	if lc.SessionRefused != "insecure_transport" || lc.Mode != "strict" || len(lc.Origins) != 0 {
		t.Errorf("login-config = %+v, want the refusal named, strict, no handoff origins", lc)
	}

	s.cfg().Env.UIInsecureCookies = true
	c := mustSession(t, s, "legacy-secret", tailnet)
	if c.Name != sessionCookiePlain || c.Secure {
		t.Errorf("insecure-cookies session cookie = %+v", c)
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c, tailnet)); code != http.StatusOK {
		t.Errorf("insecure-cookies session = %d, want 200", code)
	}

	s.cfg().Env.UIInsecureCookies = false
	if c := mustSession(t, s, "legacy-secret", overTLS); c.Name != sessionCookieSecure {
		t.Errorf("strict over TLS cookie = %s", c.Name)
	}
}

// Strict mode offers the paste box only when a kind: token gate is listed.
func TestUILogin_StrictModeWithoutTokenGateRefusesPaste(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	s.cfg().ResolvedAuth = config.AuthPolicy{Mode: config.AuthModeStrict}
	if rec := doLogin(t, s, loginReq("legacy-secret", overTLS)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "gate_unavailable") {
		t.Errorf("paste login with no token gate = %d %s, want 403 gate_unavailable", rec.Code, rec.Body.String())
	}
}

// §8 WebSocket: an upgrade to /v1/client-tools authenticated by the session
// cookie needs a same-origin request; a foreign Origin is refused.
func TestUISession_WebSocketUpgradeFromForeignOriginRefused(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	c := mustSession(t, s, "legacy-secret")
	upgrade := func(origin string) *http.Request {
		r := sessionReq(http.MethodGet, "/v1/client-tools", c)
		r.Header.Del("Sec-Fetch-Site")
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Origin", origin)
		return r
	}
	if code, reached := through(s, upgrade("https://evil.example")); code != http.StatusForbidden || reached {
		t.Errorf("foreign-origin upgrade = %d reached=%v, want 403", code, reached)
	}
	if code, reached := through(s, upgrade("http://example.com")); code != http.StatusOK || !reached {
		t.Errorf("same-origin upgrade = %d reached=%v, want 200", code, reached)
	}
}
