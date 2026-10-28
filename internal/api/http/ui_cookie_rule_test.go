package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Every sign-in cookie follows the session cookie's transport rule: Secure
// with the __Host- name over TLS, plain only to a plain-HTTP request the
// session rule admits, nothing at all where strict mode refuses plain HTTP.
func TestUISession_SignInCookiesFollowOneTransportRule(t *testing.T) {
	f := newPasskeyFixture(t)
	begin := func(target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", target, strings.NewReader(`{}`))
		r.Header.Set(uiRequestHeader, "1")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		f.srv.Mux().ServeHTTP(w, r)
		return w
	}
	cookie := func(w *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range w.Result().Cookies() {
			if strings.Contains(c.Name, "lc_webauthn") {
				return c
			}
		}
		return nil
	}
	// Over TLS.
	w := begin("https://lc.example/v1/_auth/passkey/keys/begin")
	if c := cookie(w); w.Code != http.StatusOK || c == nil || c.Name != webauthnCookieSecure || !c.Secure || !c.HttpOnly {
		t.Fatalf("over TLS: %d %+v", w.Code, c)
	}
	// Plain HTTP in token mode: the documented exception.
	f.srv.cfg().Env.PublicURL = "http://lc.example"
	w = begin("http://lc.example/v1/_auth/passkey/keys/begin")
	if c := cookie(w); w.Code != http.StatusOK || c == nil || c.Name != webauthnCookiePlain || c.Secure {
		t.Fatalf("plain HTTP, token mode: %d %+v", w.Code, c)
	}
	// Plain HTTP in strict mode: refused, no cookie.
	f.srv.cfg().ResolvedAuth.Mode = config.AuthModeStrict
	w = begin("http://lc.example/v1/_auth/passkey/keys/begin")
	if c := cookie(w); w.Code != http.StatusForbidden || c != nil {
		t.Fatalf("plain HTTP, strict mode: %d %+v", w.Code, c)
	}
}
