package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/oidc/oidctest"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// linkFixture: an OIDC gate "idp" (the one being linked) and an identity gate
// "google" the user is signed in through.
func linkFixture(t *testing.T, idpMap config.AuthIdentityMap) (*oidcFixture, *http.Cookie, string) {
	t.Helper()
	f := newOIDCFixture(t, idpMap, nil)
	f.s.cfg().ResolvedAuth.Gates = append(f.s.cfg().ResolvedAuth.Gates,
		identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}))
	c, out := identityLoginSession(t, f.s, "900", time.Now())
	return f, c, out.Subject
}

func linkReq(c *http.Cookie, subject, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/_users/"+subject+"/identities", strings.NewReader(body))
	r.SetPathValue("subject", subject)
	if c != nil {
		r.AddCookie(c)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("User-Agent", testUA)
	r.Header.Set(uiRequestHeader, "1")
	return r
}

// startLink posts the link request through the middleware.
func (f *oidcFixture) startLink(t *testing.T, r *http.Request) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.s.authMiddleware(http.HandlerFunc(f.s.handleLinkUserIdentity)).ServeHTTP(rec, r)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body["redirect"]
}

// RFC FA D14: linking takes a fresh sign-in at both gates in one browser,
// and afterwards the provider account signs in as the existing user.
func TestLink_FreshReauthThroughBothGatesLinksTheAccount(t *testing.T) {
	f, c, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
	rec, redirect := f.startLink(t, linkReq(c, subject, `{"gate":"idp"}`))
	if rec.Code != http.StatusOK || redirect == "" {
		t.Fatalf("start link = %d %s", rec.Code, rec.Body.String())
	}
	lc := cookieNamed(rec, loginCookiePlain)
	cb := f.authorize(t, redirect)
	if f.p.LastAuthorize.Get("prompt") != "login" || f.p.LastAuthorize.Get("max_age") != "0" {
		t.Fatalf("the provider was not asked to re-authenticate: %v", f.p.LastAuthorize)
	}
	done := f.complete(t, cb, lc)
	if done.Header().Get("Location") != "/ui/account?linked=idp" {
		t.Fatalf("link callback = %d %q", done.Code, done.Header().Get("Location"))
	}
	if cookieNamed(done, sessionCookiePlain) != nil {
		t.Fatal("a link callback started a new session")
	}
	id, err := f.st.UserIdentityGet(context.Background(), "idp", f.p.Issuer, "u-1")
	if err != nil || id.Subject != subject || id.TenantID != "acme" {
		t.Fatalf("linked identity = %+v %v", id, err)
	}
	// Signing in through idp now lands on the existing user.
	login, _, _ := f.login(t, "")
	_, me := meOf(t, f.s, cookieNamed(login, sessionCookiePlain))
	if me["subject"] != subject {
		t.Fatalf("idp login as %v, want %s", me["subject"], subject)
	}
	if ev := lastEvent(t, f.sink, "auth_identity_linked"); ev.TargetSubject != subject || ev.AuthGate != "idp" {
		t.Fatalf("audit = %+v", ev)
	}
}

// RFC FA §8: linking without a re-authentication is refused — a stale
// session, a provider that did not re-authenticate, a bearer instead of a
// session, or someone else's subject.
func TestLink_WithoutReauthIsRefused(t *testing.T) {
	t.Run("stale session", func(t *testing.T) {
		f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme"}, nil)
		f.s.cfg().ResolvedAuth.Gates = append(f.s.cfg().ResolvedAuth.Gates,
			identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}))
		c, out := identityLoginSession(t, f.s, "901", time.Now().Add(-time.Hour))
		rec, _ := f.startLink(t, linkReq(c, out.Subject, `{"gate":"idp"}`))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "reauth_required") {
			t.Fatalf("stale = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("provider did not re-authenticate", func(t *testing.T) {
		f, c, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
		f.p.Set(func(p *oidctest.Provider) {
			p.Mutate = func(m map[string]any) { m["auth_time"] = time.Now().Add(-3 * time.Hour).Unix() }
		})
		rec, redirect := f.startLink(t, linkReq(c, subject, `{"gate":"idp"}`))
		done := f.complete(t, f.authorize(t, redirect), cookieNamed(rec, loginCookiePlain))
		if loc := done.Header().Get("Location"); !strings.Contains(loc, "error=login_failed") {
			t.Fatalf("callback = %q", loc)
		}
		if _, err := f.st.UserIdentityGet(context.Background(), "idp", f.p.Issuer, "u-1"); !isNotFound(err) {
			t.Fatal("linked without a fresh provider authentication")
		}
	})
	t.Run("bearer, not a session", func(t *testing.T) {
		f, _, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
		r := linkReq(nil, subject, `{"gate":"idp"}`)
		r.Header.Set("Authorization", "Bearer legacy")
		rec, _ := f.startLink(t, r)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("bearer = %d", rec.Code)
		}
	})
	t.Run("another user", func(t *testing.T) {
		f, c, _ := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
		rec, _ := f.startLink(t, linkReq(c, "someone-else", `{"gate":"idp"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("other subject = %d", rec.Code)
		}
	})
}

func TestLink_RefusesAnAccountOfAnotherTenantOrAlreadyLinked(t *testing.T) {
	t.Run("other tenant", func(t *testing.T) {
		f, c, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "other"})
		rec, redirect := f.startLink(t, linkReq(c, subject, `{"gate":"idp"}`))
		done := f.complete(t, f.authorize(t, redirect), cookieNamed(rec, loginCookiePlain))
		if !strings.Contains(done.Header().Get("Location"), "link_error") {
			t.Fatalf("callback = %q", done.Header().Get("Location"))
		}
	})
	t.Run("already another user's", func(t *testing.T) {
		f, c, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
		if err := f.st.UserIdentityCreate(context.Background(), store.UserIdentityRow{TenantID: "acme", Subject: "bob",
			Gate: "idp", Issuer: f.p.Issuer, ExternalSubject: "u-1"}); err != nil {
			t.Fatal(err)
		}
		rec, redirect := f.startLink(t, linkReq(c, subject, `{"gate":"idp"}`))
		done := f.complete(t, f.authorize(t, redirect), cookieNamed(rec, loginCookiePlain))
		if !strings.Contains(done.Header().Get("Location"), "link_error") {
			t.Fatalf("callback = %q", done.Header().Get("Location"))
		}
		if id, _ := f.st.UserIdentityGet(context.Background(), "idp", f.p.Issuer, "u-1"); id.Subject != "bob" {
			t.Fatal("the link moved bob's account")
		}
	})
	t.Run("the session ended meanwhile", func(t *testing.T) {
		f, c, subject := linkFixture(t, config.AuthIdentityMap{Tenant: "acme"})
		rec, redirect := f.startLink(t, linkReq(c, subject, `{"gate":"idp"}`))
		out := httptest.NewRecorder()
		f.s.handleAuthLogout(out, sessionReq(http.MethodPost, "/v1/_auth/logout", c))
		done := f.complete(t, f.authorize(t, redirect), cookieNamed(rec, loginCookiePlain))
		if !strings.Contains(done.Header().Get("Location"), "link_error") {
			t.Fatalf("callback = %q", done.Header().Get("Location"))
		}
	})
}

// Unlinking ends the sessions made through that identity, and the list
// shows what is linked.
func TestUnlink_EndsTheIdentitysSessions(t *testing.T) {
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	login, _, _ := f.login(t, "")
	sc := cookieNamed(login, sessionCookiePlain)
	list := httptest.NewRecorder()
	lr := sessionReq(http.MethodGet, "/v1/_users/idp:u-1/identities", sc)
	lr.SetPathValue("subject", "idp:u-1")
	f.s.authMiddleware(http.HandlerFunc(f.s.handleListUserIdentities)).ServeHTTP(list, lr)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"gate":"idp"`) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	del := httptest.NewRecorder()
	dr := sessionReq(http.MethodDelete, "/v1/_users/idp:u-1/identities/idp", sc)
	dr.SetPathValue("subject", "idp:u-1")
	dr.SetPathValue("gate", "idp")
	f.s.authMiddleware(http.HandlerFunc(f.s.handleUnlinkUserIdentity)).ServeHTTP(del, dr)
	if del.Code != http.StatusNoContent {
		t.Fatalf("unlink = %d %s", del.Code, del.Body.String())
	}
	if code, _ := meOf(t, f.s, sc); code != http.StatusUnauthorized {
		t.Fatalf("session after unlink = %d", code)
	}
}

func TestIdentityRoutes_AreSelfServiceButNotOtherUsers(t *testing.T) {
	alice := auth.Principal{TenantID: "acme", Subject: "a", Scopes: []string{auth.ScopeRunsRead}}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/_users/a/identities"}, {http.MethodPost, "/v1/_users/a/identities"},
		{http.MethodDelete, "/v1/_users/a/identities/g"},
	} {
		if !ownIdentityRoute(alice, tc.method, tc.path) {
			t.Errorf("%s %s not self-service", tc.method, tc.path)
		}
		if ownIdentityRoute(alice, tc.method, strings.Replace(tc.path, "/a/", "/b/", 1)) {
			t.Errorf("%s on another user's identities is self-service", tc.method)
		}
	}
	legacy := alice
	legacy.Legacy = true
	if ownIdentityRoute(alice, http.MethodPost, "/v1/_users/a/tokens") || ownIdentityRoute(alice, http.MethodDelete, "/v1/_users/a/identities") ||
		ownIdentityRoute(legacy, http.MethodGet, "/v1/_users/a/identities") {
		t.Error("self-service reaches too far")
	}
	// A member listing another user's identities is an opaque miss.
	f := newOIDCFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	login, _, _ := f.login(t, "")
	r := sessionReq(http.MethodGet, "/v1/_users/bob/identities", cookieNamed(login, sessionCookiePlain))
	r.SetPathValue("subject", "bob")
	rec := httptest.NewRecorder()
	f.s.authMiddleware(http.HandlerFunc(f.s.handleListUserIdentities)).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("other user's identities = %d", rec.Code)
	}
}
