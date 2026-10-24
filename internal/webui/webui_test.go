package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandler_TokenQuerySetsNoCookie: `/ui?token=` was the pre-session way in
// and set the raw bearer as a cookie. It is no longer honoured (RFC FA §5): it
// sends the browser to the login page, so the token leaves the address bar,
// and sets nothing.
func TestHandler_TokenQuerySetsNoCookie(t *testing.T) {
	h := Handler("/ui", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui?token=secret-token-123", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("status = %d Location = %q, want 302 to /ui/login", rec.Code, rec.Header().Get("Location"))
	}
	if c := rec.Result().Cookies(); len(c) != 0 {
		t.Fatalf("?token= set cookies %+v; nothing may carry a bearer into a cookie", c)
	}
}

// TestHandler_SetsNoCookieOnAnyRoute: the SPA handler never sets a cookie — the
// login and logout routes are the API server's, so a UI bundle cannot change
// cookie handling. GET /ui/logout is a deep link the SPA handles by posting to
// the server's logout route; the GET itself changes nothing.
func TestHandler_SetsNoCookieOnAnyRoute(t *testing.T) {
	h := Handler("/ui", nil)
	for _, target := range []string{"/ui", "/ui/logout", "/ui/login", "/ui/login-config.json", "/ui/assets/x.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if c := rec.Result().Cookies(); len(c) != 0 {
			t.Errorf("GET %s set cookies %+v", target, c)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/ui/session", nil))
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("POST /ui/session reached the SPA handler: status %d", rec.Code)
	}
}

func TestHandler_IndexReturns503WhenUINotBuilt(t *testing.T) {
	// dist/ in the source tree contains only .gitkeep at this point
	// (assuming tests run before `make build-ui`). Hitting /ui without
	// an index.html should produce the documented diagnostic.
	h := Handler("/ui", nil)
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

// TestLoginConfig_ServesModeGatesAndOrigins: the login page renders from this
// document — the mode, the gates (name, kind, label) and the handoff origins —
// and an unconfigured list is an empty array, not null.
func TestLoginConfig_ServesModeGatesAndOrigins(t *testing.T) {
	login := func(*http.Request) LoginConfig {
		return LoginConfig{
			Origins: []string{"https://loomcycle.cloud"}, Mode: "token",
			Gates:          []LoginGate{{Name: "token", Kind: "token", Label: "Access token"}},
			SessionRefused: "insecure_transport",
		}
	}
	rec := httptest.NewRecorder()
	Handler("/ui", login).ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login-config.json", nil))
	var body struct {
		Origins        []string    `json:"login_origins"`
		Mode           string      `json:"mode"`
		Gates          []LoginGate `json:"gates"`
		SessionRefused string      `json:"session_refused"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if len(body.Origins) != 1 || body.Mode != "token" || len(body.Gates) != 1 || body.Gates[0].Kind != "token" ||
		body.Gates[0].Label != "Access token" || body.SessionRefused != "insecure_transport" {
		t.Errorf("login-config = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	Handler("/ui", nil).ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login-config.json", nil))
	got := rec.Body.String()
	if !strings.Contains(got, `"login_origins":[]`) || !strings.Contains(got, `"gates":[]`) || strings.Contains(got, "session_refused") {
		t.Errorf("unconfigured login-config = %s, want empty lists and no refusal", got)
	}
}

// TestLoginConfig_ReportsAPILevel: the unauthenticated UI config carries the
// server's UI API level, so a UI can tell which API it is talking to before
// the operator signs in.
func TestLoginConfig_ReportsAPILevel(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler("/ui", nil).ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login-config.json", nil))
	var body struct {
		APILevel *int `json:"api_level"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if body.APILevel == nil || *body.APILevel != APILevel {
		t.Fatalf("api_level = %v, want %d; body=%s", body.APILevel, APILevel, rec.Body.String())
	}
}
