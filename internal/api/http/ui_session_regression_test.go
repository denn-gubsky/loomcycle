package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression tests for the pre-session Web UI login (RFC FA §2): driven
// through the real mux, so they read the same before and after the session
// substrate, and each failed on the bearer-cookie implementation.

func muxLogin(t *testing.T, h http.Handler, bearer string, sameOriginHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/ui/session", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	if sameOriginHeader {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// The login route set a cookie for any bearer, valid or not.
func TestUILoginRoute_InvalidBearerSetsNoCookie(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy-secret")
	rec := muxLogin(t, s.Mux(), "not-a-token", true)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("invalid bearer: status %d cookies %v, want 401 and no cookie", rec.Code, rec.Result().Cookies())
	}
}

// The login route's cookie VALUE was the bearer itself.
func TestUILoginRoute_CookieNeverCarriesTheBearer(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy-secret")
	rec := muxLogin(t, s.Mux(), "legacy-secret", true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid login status = %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Value == "legacy-secret" {
			t.Fatalf("cookie %s carries the bearer", c.Name)
		}
	}
}

// A login request with no Sec-Fetch-Site (and no Origin) passed the
// same-origin guard.
func TestUILoginRoute_MissingFetchMetadataIsRefused(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy-secret")
	rec := muxLogin(t, s.Mux(), "legacy-secret", false)
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("no fetch metadata: status %d cookies %v, want 403 and no cookie", rec.Code, rec.Result().Cookies())
	}
}

// Logout cleared the browser's cookie and revoked nothing: the same cookie
// still authenticated afterwards.
func TestUILogoutRoute_OldCookieNoLongerAuthenticates(t *testing.T) {
	s, _ := tokenAuthServer(t, "legacy-secret")
	h := s.Mux()
	rec := muxLogin(t, h, "legacy-secret", true)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusNoContent || len(cookies) == 0 {
		t.Fatalf("login: status %d cookies %v", rec.Code, cookies)
	}
	withCookies := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		for _, c := range cookies {
			if c.Value != "" {
				r.AddCookie(c)
			}
		}
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Loomcycle-UI", "1")
		return r
	}
	// Both the old GET navigation and the new POST route are tried: the fix
	// is that whatever logs out ends the session on the server.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/ui/logout"
		if method == http.MethodPost {
			path = "/v1/_auth/logout"
		}
		h.ServeHTTP(httptest.NewRecorder(), withCookies(method, path))
	}
	after := httptest.NewRecorder()
	h.ServeHTTP(after, withCookies(http.MethodGet, "/v1/_me"))
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/_me with the logged-out cookie = %d, want 401", after.Code)
	}
}
