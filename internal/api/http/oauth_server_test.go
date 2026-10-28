package http

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FL §5: each security requirement of the app sign-in is a test here.

const (
	appTestRedirect = "https://board.example/cb"
	appTestOrigin   = "https://board.example"
	appTestVerifier = "verifier-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEF"
)

func pkceChallenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type appFixture struct {
	s      *Server
	st     store.Store
	sink   *recordingSink
	cookie *http.Cookie
}

// newAppFixture is a server with an identity gate, a config client
// "board", and a signed-in user google:100 of tenant acme.
func newAppFixture(t *testing.T, accessMode string) *appFixture {
	t.Helper()
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto,
		NewUser: config.AuthNewUser{AccessMode: accessMode, Status: "active"}}
	s, st, sink := identitySessionServer(t, m)
	cl, err := config.ResolveAppClient("test", "board", config.AuthClientConfig{Kind: "public",
		RedirectURIs: []string{appTestRedirect, "http://127.0.0.1/cb"}, AllowedOrigins: []string{appTestOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	s.cfg().ResolvedAuth.Clients = append(s.cfg().ResolvedAuth.Clients, cl)
	c, _ := identityLoginSession(t, s, "100", time.Now())
	return &appFixture{s: s, st: st, sink: sink, cookie: c}
}

func appAuthorizeParams(mut ...func(url.Values)) url.Values {
	v := url.Values{"response_type": {"code"}, "client_id": {"board"}, "redirect_uri": {appTestRedirect},
		"code_challenge": {pkceChallenge(appTestVerifier)}, "code_challenge_method": {"S256"}, "state": {"st-1"}}
	for _, m := range mut {
		m(v)
	}
	return v
}

func (f *appFixture) authorize(t *testing.T, q url.Values, withCookie bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/_oauth/authorize?"+q.Encode(), nil)
	r.Header.Set("User-Agent", testUA)
	if withCookie {
		r.AddCookie(f.cookie)
	}
	rec := httptest.NewRecorder()
	f.s.handleOAuthAuthorize(rec, r)
	return rec
}

// code runs a successful authorize and returns the code.
func (f *appFixture) code(t *testing.T) string {
	t.Helper()
	rec := f.authorize(t, appAuthorizeParams(), true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("authorize = %d %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", loc)
	}
	return code
}

func (f *appFixture) post(t *testing.T, path string, form url.Values, hdr ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	switch path {
	case "/v1/_oauth/token":
		f.s.handleOAuthToken(rec, r)
	case "/v1/_oauth/revoke":
		f.s.handleOAuthRevoke(rec, r)
	default:
		t.Fatalf("no handler for %s", path)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func (f *appFixture) exchange(t *testing.T, code string, mut ...func(url.Values)) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appTestRedirect},
		"client_id": {"board"}, "code_verifier": {appTestVerifier}}
	for _, m := range mut {
		m(form)
	}
	return f.post(t, "/v1/_oauth/token", form)
}

func (f *appFixture) refresh(t *testing.T, rt string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return f.post(t, "/v1/_oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {"board"}})
}

// signIn runs the whole flow and returns the access and refresh tokens.
func (f *appFixture) signIn(t *testing.T) (string, string) {
	t.Helper()
	rec, body := f.exchange(t, f.code(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange = %d %s", rec.Code, rec.Body.String())
	}
	at, _ := body["access_token"].(string)
	rt, _ := body["refresh_token"].(string)
	if at == "" || rt == "" || body["token_type"] != "Bearer" {
		t.Fatalf("token response = %v", body)
	}
	return at, rt
}

// bearerMe is GET /v1/_me with the bearer.
func (f *appFixture) bearerMe(t *testing.T, tok string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/_me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	f.s.authMiddleware(http.HandlerFunc(f.s.handleWhoami)).ServeHTTP(rec, r)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query().Get("error"), loc.Query().Get("error_description")
}

// The whole flow: the code becomes the user's own token, which /v1/_me
// reports as an app sign-in through the gate the person used.
func TestOAuthFlow_SignedInSessionGetsTheUsersOwnAppToken(t *testing.T) {
	f := newAppFixture(t, "tenant")
	at, _ := f.signIn(t)
	code, me := f.bearerMe(t, at)
	if code != http.StatusOK || me["subject"] != "google:100" || me["tenant_id"] != "acme" || me["is_admin"] != false {
		t.Fatalf("me = %d %v", code, me)
	}
	a, _ := me["auth"].(map[string]any)
	if a["app"] != "board" || a["gate"] != "google" {
		t.Fatalf("auth = %v", a)
	}
	if amr, _ := a["amr"].([]any); len(amr) != 1 || amr[0] != "oidc" {
		t.Fatalf("amr = %v", a["amr"])
	}
	if ev := lastEvent(t, f.sink, "oauth_code_issued"); ev.AuthClient != "board" || ev.ActorSubject != "google:100" {
		t.Fatalf("code audit = %+v", ev)
	}
	if ev := lastEvent(t, f.sink, "oauth_token_issued"); ev.AuthClient != "board" || ev.AuthReason != "authorization_code" {
		t.Fatalf("token audit = %+v", ev)
	}
}

// §5.1: PKCE S256 is mandatory; plain is refused; no implicit grant; state is
// echoed on every return to the app.
func TestOAuthAuthorize_RequiresPKCES256AndEchoesState(t *testing.T) {
	f := newAppFixture(t, "tenant")
	for name, mut := range map[string]func(url.Values){
		"plain":     func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"none":      func(v url.Values) { v.Del("code_challenge_method") },
		"no chal":   func(v url.Values) { v.Del("code_challenge") },
		"short":     func(v url.Values) { v.Set("code_challenge", "abc") },
		"implicit":  func(v url.Values) { v.Set("response_type", "token") },
		"no rtype":  func(v url.Values) { v.Del("response_type") },
		"dup state": func(v url.Values) { v.Add("client_id", "board") },
	} {
		rec := f.authorize(t, appAuthorizeParams(mut), true)
		if name == "dup state" {
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
				t.Errorf("%s: %d %q, want loomcycle's own error page", name, rec.Code, rec.Header().Get("Location"))
			}
			continue
		}
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if loc.Query().Get("code") != "" || loc.Query().Get("error") == "" || loc.Query().Get("state") != "st-1" {
			t.Errorf("%s: redirect %s, want an error with state and no code", name, loc)
		}
	}
	// A wrong verifier is refused at the exchange.
	rec, body := f.exchange(t, f.code(t), func(v url.Values) { v.Set("code_verifier", strings.Repeat("x", 43)) })
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier = %d %v", rec.Code, body)
	}
	// The success redirect echoes state too.
	rec = f.authorize(t, appAuthorizeParams(), true)
	if loc, _ := url.Parse(rec.Header().Get("Location")); loc.Query().Get("state") != "st-1" {
		t.Fatalf("state not echoed: %s", loc)
	}
}

func TestOAuthToken_RefusesThePasswordGrantAndUnknownGrants(t *testing.T) {
	f := newAppFixture(t, "tenant")
	for _, g := range []string{"password", "client_credentials", "implicit", "urn:ietf:params:oauth:grant-type:device_code"} {
		rec, body := f.post(t, "/v1/_oauth/token", url.Values{"grant_type": {g}, "client_id": {"board"},
			"username": {"google:100"}, "password": {"x"}})
		if rec.Code != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
			t.Errorf("%s = %d %v", g, rec.Code, body)
		}
	}
}

// §5.2: an unregistered redirect_uri never receives a redirect; the loopback
// form matches on any port.
func TestOAuthAuthorize_UnregisteredRedirectEndsOnLoomcyclesPage(t *testing.T) {
	f := newAppFixture(t, "tenant")
	for _, uri := range []string{"https://evil.example/cb", "https://board.example/cb/", "https://board.example/cb?x=1",
		"http://board.example/cb", ""} {
		rec := f.authorize(t, appAuthorizeParams(func(v url.Values) { v.Set("redirect_uri", uri) }), true)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%q: %d Location=%q", uri, rec.Code, rec.Header().Get("Location"))
		}
	}
	rec := f.authorize(t, appAuthorizeParams(func(v url.Values) { v.Set("client_id", "nope") }), true)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Errorf("unknown client: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Loopback: any port, exact path.
	rec = f.authorize(t, appAuthorizeParams(func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:53111/cb") }), true)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "http://127.0.0.1:53111/cb?") {
		t.Fatalf("loopback = %d %q", rec.Code, loc)
	}
}

// §5.3: a code is single-use; a second presentation revokes what the first
// produced — its access token and its refresh family.
func TestOAuthToken_CodeReplayRevokesTheSignInItProduced(t *testing.T) {
	f := newAppFixture(t, "tenant")
	code := f.code(t)
	rec, body := f.exchange(t, code)
	if rec.Code != http.StatusOK {
		t.Fatalf("first exchange = %d", rec.Code)
	}
	at, rt := body["access_token"].(string), body["refresh_token"].(string)
	if c, _ := f.bearerMe(t, at); c != http.StatusOK {
		t.Fatalf("token before replay = %d", c)
	}
	rec, body = f.exchange(t, code)
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("replay = %d %v", rec.Code, body)
	}
	if c, _ := f.bearerMe(t, at); c != http.StatusUnauthorized {
		t.Fatalf("access token after replay = %d, want 401", c)
	}
	if rec, body := f.refresh(t, rt); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh after replay = %d %v", rec.Code, body)
	}
	if ev := lastEvent(t, f.sink, "oauth_code_reuse"); ev.AuthClient != "board" || ev.AuthRecordID == "" {
		t.Fatalf("reuse audit = %+v", ev)
	}
}

// §5.3: the code is bound to its client, its redirect_uri, its challenge, a
// 60-second life and the session that approved it.
func TestOAuthToken_CodeIsBoundToClientRedirectChallengeTimeAndSession(t *testing.T) {
	f := newAppFixture(t, "tenant")
	other, err := config.ResolveAppClient("test", "other", config.AuthClientConfig{Kind: "public", RedirectURIs: []string{appTestRedirect}})
	if err != nil {
		t.Fatal(err)
	}
	f.s.cfg().ResolvedAuth.Clients = append(f.s.cfg().ResolvedAuth.Clients, other)
	cases := map[string]func(url.Values){
		"other client":   func(v url.Values) { v.Set("client_id", "other") },
		"other redirect": func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1/cb") },
		"wrong verifier": func(v url.Values) { v.Set("code_verifier", appTestVerifier+"x") },
	}
	for name, mut := range cases {
		rec, body := f.exchange(t, f.code(t), mut)
		if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
			t.Errorf("%s = %d %v", name, rec.Code, body)
		}
	}
	// Expired: a code stored a minute and a second ago.
	ctx := context.Background()
	past := time.Now().Add(-61 * time.Second)
	if err := f.st.OAuthCodeCreate(ctx, store.OAuthCodeRow{CodeHash: hashOAuthSecret("old-code"), ClientID: "board",
		RedirectURI: appTestRedirect, CodeChallenge: pkceChallenge(appTestVerifier), TenantID: "acme", Subject: "google:100",
		Gate: "google", SessionHash: "x", ScopeCap: []string{"runs:read"}, CreatedAt: past, ExpiresAt: past.Add(oauthCodeTTL)}); err != nil {
		t.Fatal(err)
	}
	if rec, body := f.exchange(t, "old-code"); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("expired = %d %v", rec.Code, body)
	}
	// Session: signing out between approval and exchange voids the code.
	code := f.code(t)
	logout := sessionReq(http.MethodPost, "/v1/_auth/logout", f.cookie)
	f.s.handleAuthLogout(httptest.NewRecorder(), logout)
	if rec, body := f.exchange(t, code); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("after logout = %d %v", rec.Code, body)
	}
}

// §5.4: refresh rotates; a used refresh token presented again revokes the
// whole family; a revoked family, a non-active user and a removed client are
// refused.
func TestOAuthToken_RefreshRotatesAndReuseRevokesTheFamily(t *testing.T) {
	f := newAppFixture(t, "tenant")
	at1, rt1 := f.signIn(t)
	rec, body := f.refresh(t, rt1)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %v", rec.Code, body)
	}
	at2, rt2 := body["access_token"].(string), body["refresh_token"].(string)
	if rt2 == rt1 || at2 == at1 || !strings.HasPrefix(rt2, oauthRefreshPrefix) {
		t.Fatal("refresh did not rotate")
	}
	if c, _ := f.bearerMe(t, at2); c != http.StatusOK {
		t.Fatalf("rotated token = %d", c)
	}
	// Reuse of rt1.
	if rec, body := f.refresh(t, rt1); rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reuse = %d %v", rec.Code, body)
	}
	for _, tok := range []string{at1, at2} {
		if c, _ := f.bearerMe(t, tok); c != http.StatusUnauthorized {
			t.Fatalf("family token after reuse = %d, want 401", c)
		}
	}
	if rec, _ := f.refresh(t, rt2); rec.Code != http.StatusBadRequest {
		t.Fatalf("newest refresh after reuse = %d, want refused", rec.Code)
	}
	lastEvent(t, f.sink, "oauth_refresh_reuse")
}

func TestOAuthToken_RefreshRefusedForInactiveUserAndRemovedClient(t *testing.T) {
	ctx := context.Background()
	t.Run("disabled user", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		at, rt := f.signIn(t)
		off := "disabled"
		if err := f.st.UserUpdate(ctx, "acme", "google:100", store.UserPatch{Status: &off}); err != nil {
			t.Fatal(err)
		}
		if rec, _ := f.refresh(t, rt); rec.Code != http.StatusBadRequest {
			t.Fatalf("refresh for a disabled user = %d", rec.Code)
		}
		if c, _ := f.bearerMe(t, at); c != http.StatusUnauthorized {
			t.Fatalf("access token of a disabled user = %d", c)
		}
	})
	t.Run("client removed from config", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		at, rt := f.signIn(t)
		f.s.cfg().ResolvedAuth.Clients = nil
		if rec, body := f.refresh(t, rt); rec.Code != http.StatusUnauthorized || body["error"] != "invalid_client" {
			t.Fatalf("refresh for a removed client = %d %v", rec.Code, body)
		}
		// Enforced on use: the access token stops with its client.
		f.s.tokenCache.flush()
		if c, _ := f.bearerMe(t, at); c != http.StatusUnauthorized {
			t.Fatalf("access token of a removed client = %d", c)
		}
	})
	t.Run("revoked family", func(t *testing.T) {
		f := newAppFixture(t, "tenant")
		_, rt := f.signIn(t)
		if rec, _ := f.post(t, "/v1/_oauth/revoke", url.Values{"token": {rt}, "client_id": {"board"}}); rec.Code != http.StatusOK {
			t.Fatalf("revoke = %d", rec.Code)
		}
		if rec, _ := f.refresh(t, rt); rec.Code != http.StatusBadRequest {
			t.Fatalf("refresh of a revoked family = %d", rec.Code)
		}
	})
}

// §5.5: the token never exceeds the user: scopes from access_mode, within
// the signing-in session's own, the client's restrictions carried, and the
// tenant from the session whatever the request says.
func TestOAuthToken_NeverExceedsTheUserOrTheSession(t *testing.T) {
	f := newAppFixture(t, "isolated")
	cl, _ := f.s.cfg().ResolvedAuth.Client("board")
	cl.RestrictionsJSON = `{"retention":"none"}`
	f.s.cfg().ResolvedAuth.Clients = []config.AppClient{cl}
	code := f.code(t)
	rec, body := f.exchange(t, code, func(v url.Values) { v.Set("tenant_id", "other"); v.Set("scope", "substrate:admin") })
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange = %d %v", rec.Code, body)
	}
	p, ok := f.s.resolvePrincipal(context.Background(), body["access_token"].(string))
	want, _ := auth.GrantableUserScopes("isolated")
	if !ok || p.TenantID != "acme" || p.Subject != "google:100" || len(p.Scopes) != len(want) || p.Scopes[0] != want[0] ||
		p.Restrictions.Retention != "none" || p.OAuthClient != "board" {
		t.Fatalf("principal = %+v %v", p, ok)
	}
}

// §5.5: a session bound to a narrower credential caps the app's token.
func TestOAuthToken_SessionScopesCapTheAppToken(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := context.Background()
	// A session through the token gate on a read-only token of the same user.
	seedToken(t, f.st, "lc_read_only_token_for_google_100", "acme", "google:100", []string{auth.ScopeRunsRead}, time.Time{})
	f.cookie = mustSession(t, f.s, "lc_read_only_token_for_google_100")
	rec, body := f.exchange(t, f.code(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange = %d %v", rec.Code, body)
	}
	p, ok := f.s.resolvePrincipal(ctx, body["access_token"].(string))
	if !ok || len(p.Scopes) != 1 || p.Scopes[0] != auth.ScopeRunsRead {
		t.Fatalf("app token scopes = %v, want only the session's runs:read", p.Scopes)
	}
}

// Under LOOMCYCLE_TOKEN_AUTHORITY_INHERIT an app token follows its user's
// access_mode — unless the signing-in session capped it: an inherit token
// re-derives its scopes on every request, which would lose the cap and widen
// a session opened with a narrow pasted token to the user's full scopes.
func TestOAuthToken_ACappedAppTokenIsFrozenEvenUnderInherit(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := context.Background()
	f.s.cfg().Env.TokenAuthorityInherit = true

	// Uncapped: a full session's app token inherits.
	rec, body := f.exchange(t, f.code(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange = %d %v", rec.Code, body)
	}
	if p, ok := f.s.resolvePrincipal(ctx, body["access_token"].(string)); !ok || !p.InheritsAuthority {
		t.Fatalf("uncapped app token: inherit = %v (ok %v), want inherit", p.InheritsAuthority, ok)
	}

	// Capped: a session on a read-only token of the same user stays frozen
	// at the session's scopes.
	seedToken(t, f.st, "lc_read_only_token_for_google_100", "acme", "google:100", []string{auth.ScopeRunsRead}, time.Time{})
	f.cookie = mustSession(t, f.s, "lc_read_only_token_for_google_100")
	rec, body = f.exchange(t, f.code(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("capped exchange = %d %v", rec.Code, body)
	}
	p, ok := f.s.resolvePrincipal(ctx, body["access_token"].(string))
	if !ok || p.InheritsAuthority || len(p.Scopes) != 1 || p.Scopes[0] != auth.ScopeRunsRead {
		t.Fatalf("capped app token = inherit %v scopes %v (ok %v), want frozen at runs:read", p.InheritsAuthority, p.Scopes, ok)
	}
}

// §5.6: authorize is unframeable and only ever redirects to a registered URI.
func TestOAuthAuthorize_IsUnframeableAndNotCached(t *testing.T) {
	f := newAppFixture(t, "tenant")
	rec := f.authorize(t, appAuthorizeParams(), true)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") ||
		rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), appTestRedirect+"?") {
		t.Fatalf("Location = %q", rec.Header().Get("Location"))
	}
}

// §5.6/§2: no session sends the person to the login page carrying the
// request, which names the app and where the sign-in goes.
func TestOAuthAuthorize_WithoutASessionGoesToTheLoginPage(t *testing.T) {
	f := newAppFixture(t, "tenant")
	rec := f.authorize(t, appAuthorizeParams(), false)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || loc.Path != "/ui/login" {
		t.Fatalf("no session = %d %s", rec.Code, loc)
	}
	inner, err := url.ParseQuery(loc.Query().Get("oauth"))
	if err != nil || inner.Get("client_id") != "board" || inner.Get("code_challenge") == "" {
		t.Fatalf("carried request = %v %v", inner, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/ui/login-config.json?"+url.Values{"oauth": {loc.Query().Get("oauth")}}.Encode(), nil)
	lc := f.s.loginConfig(r)
	if lc.App == nil || lc.App.Label != "board" || lc.App.Destination != "board.example" {
		t.Fatalf("login app = %+v", lc.App)
	}
	// A carried request with an unregistered redirect names no app.
	bad := appAuthorizeParams(func(v url.Values) { v.Set("redirect_uri", "https://evil.example/cb") }).Encode()
	if lc := f.s.loginConfig(httptest.NewRequest(http.MethodGet, "/ui/login-config.json?"+url.Values{"oauth": {bad}}.Encode(), nil)); lc.App != nil {
		t.Fatalf("unregistered redirect named an app: %+v", lc.App)
	}
}

// A pending user completes the gate but gets no code.
func TestOAuthAuthorize_PendingUserIsDeniedWithPending(t *testing.T) {
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto,
		NewUser: config.AuthNewUser{AccessMode: "isolated", Status: "pending"}}
	s, st, sink := identitySessionServer(t, m)
	cl, _ := config.ResolveAppClient("test", "board", config.AuthClientConfig{Kind: "public", RedirectURIs: []string{appTestRedirect}})
	s.cfg().ResolvedAuth.Clients = []config.AppClient{cl}
	c, _ := identityLoginSession(t, s, "300", time.Now())
	f := &appFixture{s: s, st: st, sink: sink, cookie: c}
	rec := f.authorize(t, appAuthorizeParams(), true)
	if code, desc := errorOf(t, rec); code != "access_denied" || desc != "pending" {
		t.Fatalf("pending = %s %s", code, desc)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "code=") {
		t.Fatalf("a pending user got a code: %s", loc)
	}
}

// §5.7: an app token is a gate-issued token, tagged with the gate and app.
func TestOAuthToken_AppTokenIsGateIssued(t *testing.T) {
	f := newAppFixture(t, "tenant")
	_, _ = f.signIn(t)
	ev := lastEvent(t, f.sink, "oauth_token_issued")
	row, err := f.st.OperatorTokenDefGet(context.Background(), ev.TargetDefID)
	if err != nil || row.IssuedByGate != "google" || row.OAuthClient != "board" || row.OAuthFamily == "" ||
		row.Name != "u-app-board" || row.ExpiresAt.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Fatalf("token row = %+v %v", row, err)
	}
}

// §5.8: no code or token in a URL; the token endpoint is never cached.
func TestOAuthToken_RefusesParametersInTheURLAndIsNotCached(t *testing.T) {
	f := newAppFixture(t, "tenant")
	code := f.code(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/_oauth/token?code="+code, strings.NewReader(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appTestRedirect},
		"client_id": {"board"}, "code_verifier": {appTestVerifier}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.s.handleOAuthToken(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("query-carried token request = %d", rec.Code)
	}
	rec, _ = f.exchange(t, code)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("token response = %d %v", rec.Code, rec.Header())
	}
	// JSON is not the token endpoint's format.
	r = httptest.NewRequest(http.MethodPost, "/v1/_oauth/token", strings.NewReader(`{"grant_type":"refresh_token"}`))
	r.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	f.s.handleOAuthToken(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("json body = %d", rec.Code)
	}
}

// §5.9: throttled per client and source.
func TestOAuthToken_IsThrottledPerClientAndSource(t *testing.T) {
	f := newAppFixture(t, "tenant")
	var last *httptest.ResponseRecorder
	for i := 0; i <= oauthThrottleLimit; i++ {
		last, _ = f.refresh(t, "lcrt_nope")
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("after %d requests = %d", oauthThrottleLimit+1, last.Code)
	}
}

// CORS: only the client's own origins, on the token and revoke endpoints.
func TestOAuthToken_CORSOnlyForTheClientsOrigins(t *testing.T) {
	f := newAppFixture(t, "tenant")
	rec, _ := f.post(t, "/v1/_oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"lcrt_x"},
		"client_id": {"board"}}, "Origin", appTestOrigin)
	if rec.Header().Get("Access-Control-Allow-Origin") != appTestOrigin {
		t.Fatalf("allowed origin got ACAO %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	code := f.code(t)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appTestRedirect},
		"client_id": {"board"}, "code_verifier": {appTestVerifier}}
	rec, _ = f.post(t, "/v1/_oauth/token", form, "Origin", "https://evil.example")
	if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin = %d ACAO=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	// Refused before the code was consumed: the app can still exchange it.
	if rec, _ := f.post(t, "/v1/_oauth/token", form); rec.Code != http.StatusOK {
		t.Fatalf("code after a foreign-origin attempt = %d", rec.Code)
	}
	pre := func(origin string) int {
		r := httptest.NewRequest(http.MethodOptions, "/v1/_oauth/token", nil)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		f.s.handleOAuthPreflight(rec, r)
		return rec.Code
	}
	if pre(appTestOrigin) != http.StatusNoContent || pre("https://evil.example") != http.StatusForbidden {
		t.Fatal("preflight answers the wrong origins")
	}
}

// RFC 7009: revoking a refresh token ends its family; an access token is
// retired; another client's token or an unknown one is a quiet 200.
func TestOAuthRevoke_RefreshEndsTheFamilyAccessIsRetiredOthersUntouched(t *testing.T) {
	f := newAppFixture(t, "tenant")
	other, _ := config.ResolveAppClient("test", "other", config.AuthClientConfig{Kind: "public", RedirectURIs: []string{appTestRedirect}})
	f.s.cfg().ResolvedAuth.Clients = append(f.s.cfg().ResolvedAuth.Clients, other)
	at, rt := f.signIn(t)
	// Another client cannot revoke board's sign-in.
	if rec, _ := f.post(t, "/v1/_oauth/revoke", url.Values{"token": {rt}, "client_id": {"other"}}); rec.Code != http.StatusOK {
		t.Fatalf("foreign revoke = %d", rec.Code)
	}
	if c, _ := f.bearerMe(t, at); c != http.StatusOK {
		t.Fatalf("another client revoked board's token: %d", c)
	}
	if fam, err := f.st.OAuthFamilyGet(context.Background(), lastEvent(t, f.sink, "oauth_token_issued").AuthRecordID); err != nil || !fam.RevokedAt.IsZero() {
		t.Fatalf("another client revoked board's sign-in: %+v %v", fam, err)
	}
	if rec, _ := f.post(t, "/v1/_oauth/revoke", url.Values{"token": {"lcrt_unknown"}, "client_id": {"board"}}); rec.Code != http.StatusOK {
		t.Fatalf("unknown token = %d", rec.Code)
	}
	if rec, _ := f.post(t, "/v1/_oauth/revoke", url.Values{"token": {rt}, "client_id": {"board"}}); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if c, _ := f.bearerMe(t, at); c != http.StatusUnauthorized {
		t.Fatalf("access token after the family's revoke = %d", c)
	}
	lastEvent(t, f.sink, "oauth_revoked")
	// An access token on its own.
	at2, rt2 := f.signIn(t)
	if rec, _ := f.post(t, "/v1/_oauth/revoke", url.Values{"token": {at2}, "client_id": {"board"}}); rec.Code != http.StatusOK {
		t.Fatalf("access revoke = %d", rec.Code)
	}
	if c, _ := f.bearerMe(t, at2); c != http.StatusUnauthorized {
		t.Fatalf("revoked access token = %d", c)
	}
	if rec, _ := f.refresh(t, rt2); rec.Code != http.StatusOK {
		t.Fatalf("refresh after an access-token revoke = %d, want the sign-in kept", rec.Code)
	}
}

// RFC 8414: the metadata names the issuer and only what is supported.
func TestOAuthMetadata_PublishesTheIssuerAndS256Only(t *testing.T) {
	f := newAppFixture(t, "tenant")
	rec := httptest.NewRecorder()
	f.s.handleOAuthMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no public URL = %d", rec.Code)
	}
	f.s.cfg().Env.PublicURL = "https://lc.example/"
	rec = httptest.NewRecorder()
	f.s.handleOAuthMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	var md map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &md)
	if md["issuer"] != "https://lc.example" || md["token_endpoint"] != "https://lc.example/v1/_oauth/token" {
		t.Fatalf("metadata = %v", md)
	}
	if m, _ := md["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Fatalf("methods = %v", md["code_challenge_methods_supported"])
	}
	// The authorize redirect then carries iss (RFC 9207).
	rec = f.authorize(t, appAuthorizeParams(), true)
	if loc, _ := url.Parse(rec.Header().Get("Location")); loc.Query().Get("iss") != "https://lc.example" {
		t.Fatalf("iss = %q", loc.Query().Get("iss"))
	}
}
