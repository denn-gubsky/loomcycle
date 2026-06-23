package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_TokenQueryRedirectsAndSetsCookie(t *testing.T) {
	h := Handler("/ui", false)
	req := httptest.NewRequest("GET", "/ui?token=secret-token-123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (token-set redirect)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui" {
		t.Errorf("Location = %q, want /ui (without the ?token= so refresh doesn't re-set)", loc)
	}
	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == SessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("no %s cookie set; tokens = %+v", SessionCookie, cookies)
	}
	if found.Value != "secret-token-123" {
		t.Errorf("cookie value = %q, want secret-token-123", found.Value)
	}
	if !found.HttpOnly {
		t.Error("cookie not HttpOnly — must be (XSS exposure of bearer token)")
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", found.SameSite)
	}
	if found.Path != "/" {
		t.Errorf("cookie path = %q, want /", found.Path)
	}
}

func TestHandler_LogoutClearsCookieAndRedirects(t *testing.T) {
	h := Handler("/ui", false)
	req := httptest.NewRequest("GET", "/ui/logout", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (logout redirect)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/login" {
		t.Errorf("Location = %q, want /ui/login", loc)
	}
	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("no %s cookie in response — logout must emit a clearing Set-Cookie", SessionCookie)
	}
	// A deletion cookie: empty value + MaxAge<0 (the browser drops it).
	if found.Value != "" || found.MaxAge >= 0 {
		t.Errorf("logout cookie not a deletion: value=%q MaxAge=%d (want \"\" and <0)", found.Value, found.MaxAge)
	}
}

func TestHandler_SecureCookieFlag(t *testing.T) {
	// secureCookie=true forces the Secure attribute even when r.TLS is
	// nil — covers the operator behind a TLS terminator.
	h := Handler("/ui", true)
	req := httptest.NewRequest("GET", "/ui?token=x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	if !cookies[0].Secure {
		t.Error("cookie missing Secure attribute despite secureCookie=true")
	}
}

func TestHandler_IndexReturns503WhenUINotBuilt(t *testing.T) {
	// dist/ in the source tree contains only .gitkeep at this point
	// (assuming tests run before `make build-ui`). Hitting /ui without
	// an index.html should produce the documented diagnostic.
	h := Handler("/ui", false)
	req := httptest.NewRequest("GET", "/ui", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// If a previous test build populated dist/index.html, treat 200 as
	// pass (the test would still cover a different path; the 503 case
	// is covered when run from a fresh checkout).
	if rec.Code == http.StatusOK {
		t.Skipf("dist/ already contains a built UI (this run came after `npm run build`); the 503 path can't be observed here")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (UI not built)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ui_not_built") {
		t.Errorf("body = %s, want ui_not_built code", rec.Body.String())
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"index.html":           "text/html; charset=utf-8",
		"assets/main-x9z3.js":  "application/javascript; charset=utf-8",
		"assets/main-x9z3.css": "text/css; charset=utf-8",
		"assets/icon.svg":      "image/svg+xml",
		"assets/font.woff2":    "font/woff2",
		"assets/main.js.map":   "application/json",
		"unknown.bin":          "",
	}
	for name, want := range cases {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
}
