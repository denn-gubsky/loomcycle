package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authgate/oidc/oidctest"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const oidcTestRedirect = "https://lc.example/v1/_auth/complete/idp"

// oidcFixture is a server with one OIDC gate "idp" in front of a fake
// provider.
type oidcFixture struct {
	s    *Server
	st   store.Store
	sink *recordingSink
	p    *oidctest.Provider
}

func newOIDCFixture(t *testing.T, m config.AuthIdentityMap, mutate func(o *config.AuthOIDCGate)) *oidcFixture {
	t.Helper()
	p := oidctest.New("client-1", "secret-1")
	t.Cleanup(p.Close)
	p.Claims = map[string]any{"sub": "u-1", "email": "a@acme.example", "email_verified": true, "name": "Alice"}
	s, st, sink := sessionTestServer(t, "legacy")
	o := config.AuthOIDCGate{Preset: config.OIDCPresetGeneric, ClientID: "client-1", ClientSecret: "secret-1", Issuer: p.Issuer,
		DiscoveryURL: p.Server.URL + "/.well-known/openid-configuration", Scopes: []string{"openid", "email"},
		RedirectURI: oidcTestRedirect, AllowedReturnTo: []string{"https://app.example/after"}}
	if mutate != nil {
		mutate(&o)
	}
	g := identityGateCfg("idp", m)
	g.OIDC = &o
	s.cfg().ResolvedAuth.Gates = append(s.cfg().ResolvedAuth.Gates, g)
	s.cfg().Env.PublicURL = "https://lc.example"
	s.oidcHTTP = p.Server.Client()
	return &oidcFixture{s: s, st: st, sink: sink, p: p}
}

// start runs GET /v1/_auth/login/idp and returns the response.
func (f *oidcFixture) start(t *testing.T, query string, src string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/_auth/login/idp"+query, nil)
	r.SetPathValue("gate", "idp")
	r.Header.Set("User-Agent", testUA)
	if src != "" {
		r.RemoteAddr = src
	}
	rec := httptest.NewRecorder()
	f.s.handleRedirectLogin(rec, r)
	return rec
}

// authorize follows the provider redirect and returns the callback query.
func (f *oidcFixture) authorize(t *testing.T, location string) string {
	t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(location)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cb := resp.Header.Get("Location")
	if !strings.HasPrefix(cb, oidcTestRedirect+"?") {
		t.Fatalf("provider redirect = %q (%d)", cb, resp.StatusCode)
	}
	return strings.TrimPrefix(cb, oidcTestRedirect)
}

// complete runs the callback with the given login cookie (nil: none).
func (f *oidcFixture) complete(t *testing.T, query string, loginCookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/_auth/complete/idp"+query, nil)
	r.SetPathValue("gate", "idp")
	r.Header.Set("User-Agent", testUA)
	if loginCookie != nil {
		r.AddCookie(loginCookie)
	}
	rec := httptest.NewRecorder()
	f.s.handleRedirectComplete(rec, r)
	return rec
}

// login runs a whole browser login and returns the callback response.
func (f *oidcFixture) login(t *testing.T, query string) (*httptest.ResponseRecorder, string, *http.Cookie) {
	t.Helper()
	rec := f.start(t, query, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	lc := cookieNamed(rec, loginCookiePlain)
	if lc == nil || lc.SameSite != http.SameSiteLaxMode || !lc.HttpOnly {
		t.Fatalf("login cookie = %+v", lc)
	}
	cb := f.authorize(t, rec.Header().Get("Location"))
	return f.complete(t, cb, lc), cb, lc
}

func loginErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if rec.Code != http.StatusSeeOther || err != nil || u.Path != "/ui/login" {
		t.Fatalf("not an error redirect: %d %q", rec.Code, loc)
	}
	return u.Query().Get("error")
}

func TestOIDCLogin_FullFlowProvisionsAndStartsASession(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	rec, _, _ := f.login(t, "?return_to=/ui/runs")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/runs" {
		t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	sc := cookieNamed(rec, sessionCookiePlain)
	if sc == nil || sc.Value == "" {
		t.Fatalf("no session cookie: %v", rec.Result().Cookies())
	}
	if lc := cookieNamed(rec, loginCookiePlain); lc == nil || lc.MaxAge >= 0 {
		t.Fatalf("login cookie not cleared: %+v", lc)
	}
	// The browser never sees a provider token.
	for _, c := range rec.Result().Cookies() {
		if strings.Count(c.Value, ".") == 2 {
			t.Fatalf("a JWT-shaped value reached the browser: %s", c.Name)
		}
	}
	code, me := meOf(t, f.s, sc)
	if code != http.StatusOK || me["subject"] != "idp:u-1" || me["tenant_id"] != "acme" {
		t.Fatalf("me = %d %v", code, me)
	}
	if a, _ := me["auth"].(map[string]any); a["gate"] != "idp" {
		t.Fatalf("auth = %v", me["auth"])
	}
	if ev := lastEvent(t, f.sink, "auth_login"); ev.ActorSubject != "idp:u-1" || ev.AuthGate != "idp" {
		t.Fatalf("auth_login = %+v", ev)
	}
}

// RFC FA §8: a replayed callback, a callback in another browser (login CSRF)
// and a callback with no login cookie are refused.
func TestOIDCLogin_RefusesReplayAndForeignBrowsers(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	rec, cb, lc := f.login(t, "")
	if rec.Code != http.StatusSeeOther || cookieNamed(rec, sessionCookiePlain) == nil {
		t.Fatalf("first login failed: %d", rec.Code)
	}
	if code := loginErrorCode(t, f.complete(t, cb, lc)); code != "login_expired" {
		t.Fatalf("replay = %q", code)
	}
	// The attacker starts a login, gets the provider's code for their own
	// account, and sends the callback link to a victim whose browser has its
	// own (unrelated) login cookie.
	attacker := f.start(t, "", "")
	attackerCB := f.authorize(t, attacker.Header().Get("Location"))
	victim := cookieNamed(f.start(t, "", ""), loginCookiePlain)
	rec = f.complete(t, attackerCB, victim)
	if code := loginErrorCode(t, rec); code != "login_failed" || cookieNamed(rec, sessionCookiePlain) != nil {
		t.Fatalf("foreign browser = %q", code)
	}
	// And the callback with no cookie at all.
	other := f.start(t, "", "")
	if code := loginErrorCode(t, f.complete(t, f.authorize(t, other.Header().Get("Location")), nil)); code != "login_expired" {
		t.Fatalf("no cookie = %q", code)
	}
}

// RFC FA §8 open redirect: return_to must be a Web UI path or listed.
func TestOIDCLogin_RefusesAForeignReturnTo(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	for _, rt := range []string{"https://evil.example/", "//evil.example", "/v1/_me"} {
		rec := f.start(t, "?return_to="+url.QueryEscape(rt), "")
		if code := loginErrorCode(t, rec); code != "return_to_refused" {
			t.Errorf("%q = %q", rt, code)
		}
		if cookieNamed(rec, loginCookiePlain) != nil {
			t.Errorf("%q started a login", rt)
		}
	}
	rec, _, _ := f.login(t, "?return_to="+url.QueryEscape("https://app.example/after"))
	if rec.Header().Get("Location") != "https://app.example/after" {
		t.Fatalf("allowlisted return_to = %q", rec.Header().Get("Location"))
	}
}

// Replay caches: starting a login is throttled per client source, so an
// unauthenticated caller cannot fill the login-state table.
func TestOIDCLogin_StartIsThrottledPerSource(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme"}, nil)
	for i := 0; i < loginStartLimit; i++ {
		if rec := f.start(t, "", "198.51.100.7:1000"); rec.Code != http.StatusSeeOther || cookieNamed(rec, loginCookiePlain) == nil {
			t.Fatalf("start %d refused", i)
		}
	}
	if code := loginErrorCode(t, f.start(t, "", "198.51.100.7:1001")); code != "too_many_attempts" {
		t.Fatalf("over the limit = %q", code)
	}
	if rec := f.start(t, "", "203.0.113.9:1000"); cookieNamed(rec, loginCookiePlain) == nil {
		t.Fatal("another source was held back")
	}
}

func TestOIDCLogin_PendingUserLandsOnTheAwaitingPage(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto,
		NewUser: config.AuthNewUser{AccessMode: "isolated", Status: "pending"}}, nil)
	rec, _, _ := f.login(t, "")
	if rec.Header().Get("Location") != "/ui/pending" {
		t.Fatalf("dest = %q", rec.Header().Get("Location"))
	}
	_, me := meOf(t, f.s, cookieNamed(rec, sessionCookiePlain))
	if a, _ := me["auth"].(map[string]any); a["status"] != "pending" {
		t.Fatalf("me auth = %v", me["auth"])
	}
}

func TestOIDCLogin_RefusedIdentityGetsNoSession(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionKnown}, nil)
	rec, _, _ := f.login(t, "")
	if code := loginErrorCode(t, rec); code != "login_failed" || cookieNamed(rec, sessionCookiePlain) != nil {
		t.Fatalf("unknown user = %q", code)
	}
	if ev := lastEvent(t, f.sink, "auth_login_failed"); !strings.Contains(ev.AuthReason, "provision: known") {
		t.Fatalf("reason = %q", ev.AuthReason)
	}
	// A forged ID token is refused the same opaque way.
	f.p.Set(func(p *oidctest.Provider) { p.Mutate = func(c map[string]any) { c["aud"] = "other" } })
	rec, _, _ = f.login(t, "")
	if code := loginErrorCode(t, rec); code != "login_failed" {
		t.Fatalf("forged token = %q", code)
	}
}

func TestOIDCLogout_SendsTheProvidersEndSessionURL(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto},
		func(o *config.AuthOIDCGate) { o.EndSession = true })
	rec, _, _ := f.login(t, "")
	sc := cookieNamed(rec, sessionCookiePlain)
	out := httptest.NewRecorder()
	f.s.handleAuthLogout(out, sessionReq(http.MethodPost, "/v1/_auth/logout", sc))
	if out.Code != http.StatusOK {
		t.Fatalf("logout = %d", out.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(out.Body.Bytes(), &body)
	if !strings.HasPrefix(body["end_session_url"], f.p.Server.URL+"/logout?") ||
		!strings.Contains(body["end_session_url"], url.QueryEscape("https://lc.example/ui/login")) {
		t.Fatalf("end_session_url = %q", body["end_session_url"])
	}
	if code, _ := meOf(t, f.s, sc); code != http.StatusUnauthorized {
		t.Fatalf("session after logout = %d", code)
	}
}

// gateFor builds the configured OIDC gate, so a strict deployment with only
// an OIDC gate can sign people in.
func TestGateFor_BuildsTheOIDCGate(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme"}, nil)
	g, _ := f.s.configuredGate("idp")
	gate, err := f.s.gateFor(g)
	if err != nil || gate.Kind() != config.AuthGateKindOIDC {
		t.Fatalf("gateFor = %v %v", gate, err)
	}
}
