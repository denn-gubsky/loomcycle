package config

import (
	"strings"
	"testing"
	"time"
)

func TestAuthClients_YAMLLoadsTheRFCExample(t *testing.T) {
	c, err := loadYAMLString(t, `
auth:
  clients:
    loomboard:
      kind: public
      label: loomboard
      redirect_uris:
        - https://board.example.com/auth/callback
        - https://abcdefghijklmnop.chromiumapp.org/
        - http://127.0.0.1/callback
        - loomboard://auth/callback
      allowed_origins: [https://board.example.com]
      access_token_ttl: 1h
      refresh: { idle: 7d, absolute: 30d }
      restrictions: { retention: none }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cl, ok := c.ResolvedAuth.Client("loomboard")
	if !ok {
		t.Fatal("client not resolved")
	}
	if len(cl.RedirectURIs) != 4 || cl.AccessTTL != time.Hour || cl.RefreshIdle != 7*24*time.Hour ||
		cl.RefreshAbs != 30*24*time.Hour || cl.RestrictionsJSON != `{"retention":"none"}` || !cl.FromConfig() ||
		cl.TenantID != "" || !cl.OriginAllowed("https://board.example.com") || cl.OriginAllowed("https://evil.example") {
		t.Fatalf("client = %+v", cl)
	}
}

func TestAuthClients_DefaultsAreTheRFCLifetimes(t *testing.T) {
	cl, err := ResolveAppClient("x", "app", AuthClientConfig{Kind: "public", RedirectURIs: []string{"https://a.example/cb"}})
	if err != nil {
		t.Fatal(err)
	}
	if cl.AccessTTL != time.Hour || cl.RefreshIdle != 7*24*time.Hour || cl.RefreshAbs != 30*24*time.Hour || cl.Label != "app" {
		t.Fatalf("defaults = %+v", cl)
	}
}

func TestValidateAppRedirectURI_AcceptsOnlyTheRFCForms(t *testing.T) {
	for _, ok := range []string{
		"https://board.example.com/auth/callback",
		"https://board.example.com:8443/cb?x=1",
		"http://127.0.0.1/callback",
		"http://127.0.0.1:53000/callback",
		"http://[::1]/callback",
		"loomboard://auth/callback",
		"com.example.board:/oauth2redirect",
	} {
		if err := ValidateAppRedirectURI(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"http://board.example.com/cb",       // http to a non-loopback host
		"http://localhost/cb",               // a name, not a loopback literal
		"http://127.0.0.2/cb",               // not the loopback literal
		"https://board.example.com/cb#frag", // fragment
		"https://*.example.com/cb",          // wildcard
		"https://board.example.com/*",       // wildcard
		"https://user:pw@board.example.com/cb",
		"https:///cb", // no host
		"javascript:alert(1)",
		"data:text/html,x",
		"file:///etc/passwd",
		"loomboard:",
		"/relative/path",
		"HTTPS://board.example.com/cb",
		"https://board.example.com/c b",
	} {
		if err := ValidateAppRedirectURI(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAppClient_MatchRedirectURIIsExactExceptLoopbackPort(t *testing.T) {
	cl, err := ResolveAppClient("x", "app", AuthClientConfig{Kind: "public", RedirectURIs: []string{
		"https://board.example.com/auth/callback", "http://127.0.0.1/callback", "loomboard://auth/callback",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{
		"https://board.example.com/auth/callback",
		"http://127.0.0.1/callback",
		"http://127.0.0.1:61234/callback",
		"loomboard://auth/callback",
	} {
		if !cl.MatchRedirectURI(ok) {
			t.Errorf("%s did not match", ok)
		}
	}
	for _, bad := range []string{
		"https://board.example.com/auth/callback/",
		"https://board.example.com/auth/callback?x=1",
		"https://board.example.com:443/auth/callback",
		"https://BOARD.example.com/auth/callback",
		"https://board.example.com.evil.example/auth/callback",
		"http://127.0.0.1:61234/callback/other",
		"http://127.0.0.1:61234/callback#x",
		"http://[::1]:61234/callback",
		"http://localhost:61234/callback",
		"loomboard://auth/callback2",
	} {
		if cl.MatchRedirectURI(bad) {
			t.Errorf("%s matched", bad)
		}
	}
}

func TestAuthClients_RefusesWhatTheRFCRefuses(t *testing.T) {
	base := func() AuthClientConfig {
		return AuthClientConfig{Kind: "public", RedirectURIs: []string{"https://a.example/cb"}}
	}
	for name, tc := range map[string]struct {
		id   string
		mut  func(*AuthClientConfig)
		want string
	}{
		"bad id":         {"Bad Id", func(*AuthClientConfig) {}, "invalid client id"},
		"no kind":        {"a", func(c *AuthClientConfig) { c.Kind = "" }, "kind is required"},
		"confidential":   {"a", func(c *AuthClientConfig) { c.Kind = "confidential" }, "not supported"},
		"no redirects":   {"a", func(c *AuthClientConfig) { c.RedirectURIs = nil }, "at least one"},
		"bad redirect":   {"a", func(c *AuthClientConfig) { c.RedirectURIs = []string{"http://evil.example/cb"} }, "loopback"},
		"origin path":    {"a", func(c *AuthClientConfig) { c.AllowedOrigins = []string{"https://a.example/x"} }, "nothing after"},
		"origin star":    {"a", func(c *AuthClientConfig) { c.AllowedOrigins = []string{"*"} }, "wildcard"},
		"origin http":    {"a", func(c *AuthClientConfig) { c.AllowedOrigins = []string{"http://a.example"} }, "loopback"},
		"ttl too long":   {"a", func(c *AuthClientConfig) { c.AccessTokenTTL = "48h" }, "between"},
		"idle > abs":     {"a", func(c *AuthClientConfig) { c.Refresh = AuthClientRefresh{Idle: "40d", Absolute: "30d"} }, "longer than absolute"},
		"abs too long":   {"a", func(c *AuthClientConfig) { c.Refresh = AuthClientRefresh{Absolute: "400d"} }, "at most"},
		"bad restrict":   {"a", func(c *AuthClientConfig) { c.Restrictions = map[string]any{"nope": 1} }, "restrictions"},
		"bad retention":  {"a", func(c *AuthClientConfig) { c.Restrictions = map[string]any{"retention": "maybe"} }, "restrictions"},
		"label controls": {"a", func(c *AuthClientConfig) { c.Label = "a\nb" }, "control"},
	} {
		cc := base()
		tc.mut(&cc)
		_, err := ResolveAppClient("auth.clients."+tc.id, tc.id, cc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestAuthClients_ALoadRefusesAnInvalidClient(t *testing.T) {
	_, err := loadYAMLString(t, `
auth:
  clients:
    board:
      kind: public
      redirect_uris: [http://board.example.com/cb]
`)
	if err == nil || !strings.Contains(err.Error(), "auth.clients.board.redirect_uris") {
		t.Fatalf("err = %v, want the redirect refusal naming the key", err)
	}
}
