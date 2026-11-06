package credential

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
)

func fakeConfig(f *oauthtest.Server) OAuthConfig {
	return OAuthConfig{Resource: f.Resource(), Client: OAuthClientConfig{Registration: OAuthRegistrationDynamic, TokenEndpointAuth: OAuthAuthNone}}
}

func TestOAuthDiscover_FollowsPRMThenASMetadata(t *testing.T) {
	f := oauthtest.New(t)
	c := &oauthClient{http: f.Client()}
	ep, err := c.discover(context.Background(), fakeConfig(f))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Issuer != f.Srv.URL || ep.Token != f.Srv.URL+"/token" || ep.DeviceAuthorization != f.Srv.URL+"/device" ||
		ep.Registration != f.Srv.URL+"/register" || ep.Revocation != f.Srv.URL+"/revoke" || ep.Userinfo != f.Srv.URL+"/userinfo" {
		t.Errorf("endpoints = %+v", ep)
	}
	if f.Count("prm") != 1 || f.Count("meta") != 1 {
		t.Errorf("prm=%d meta=%d, want one each", f.Count("prm"), f.Count("meta"))
	}
}

func TestOAuthDiscover_RefusesHTTPEndpoint(t *testing.T) {
	f := oauthtest.New(t)
	f.Set(func(f *oauthtest.Server) {
		f.MetaOverride = func(m map[string]any) {
			m["token_endpoint"] = strings.Replace(f.Srv.URL, "https:", "http:", 1) + "/token"
		}
	})
	if _, err := (&oauthClient{http: f.Client()}).discover(context.Background(), fakeConfig(f)); err == nil {
		t.Error("an http token endpoint was accepted")
	}
}

func TestOAuthDiscover_RefusesEndpointOffIssuerAndResourceHost(t *testing.T) {
	f := oauthtest.New(t)
	for _, field := range []string{"token_endpoint", "device_authorization_endpoint", "registration_endpoint", "revocation_endpoint", "userinfo_endpoint"} {
		f.Set(func(f *oauthtest.Server) {
			f.MetaOverride = func(m map[string]any) { m[field] = "https://collector.example.test/x" }
		})
		if _, err := (&oauthClient{http: f.Client()}).discover(context.Background(), fakeConfig(f)); err == nil {
			t.Errorf("%s on a third host was accepted", field)
		}
	}
}

func TestOAuthDiscover_RefusesIssuerMismatch(t *testing.T) {
	f := oauthtest.New(t)
	f.Set(func(f *oauthtest.Server) {
		f.MetaOverride = func(m map[string]any) { m["issuer"] = "https://other-as.example.test" }
	})
	if _, err := (&oauthClient{http: f.Client()}).discover(context.Background(), fakeConfig(f)); err == nil {
		t.Error("metadata naming another issuer was accepted")
	}
	// A configured issuer the resource does not list is refused too.
	cfg := fakeConfig(f)
	cfg.Issuer = "https://other-as.example.test"
	f.Set(func(f *oauthtest.Server) { f.MetaOverride = nil })
	if _, err := (&oauthClient{http: f.Client()}).discover(context.Background(), cfg); err == nil {
		t.Error("an issuer the resource does not list was accepted")
	}
}

func TestOAuthDiscover_RefusesResourceMismatch(t *testing.T) {
	f := oauthtest.New(t)
	cfg := fakeConfig(f)
	cfg.Resource = f.Srv.URL + "/mcp-other"
	// The metadata at the well-known path for /mcp-other does not exist; the
	// root fallback is not served either, so it fails — and a server whose
	// metadata names another resource must fail as well.
	if _, err := (&oauthClient{http: f.Client()}).discover(context.Background(), cfg); err == nil {
		t.Error("discovery succeeded for a resource the server does not name")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		oauthtest.WriteJSON(w, 200, map[string]any{"resource": "https://elsewhere.example.test/mcp", "authorization_servers": []string{f.Srv.URL}})
	})
	other := httptest.NewTLSServer(mux)
	defer other.Close()
	cfg.Resource = other.URL + "/mcp"
	if _, err := (&oauthClient{http: other.Client()}).discover(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "different resource") {
		t.Errorf("err = %v, want a resource mismatch", err)
	}
}

func TestOAuthDiscover_DoesNotFollowRedirects(t *testing.T) {
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a redirect was followed")
	}))
	defer sink.Close()
	hop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer hop.Close()
	cl := hop.Client()
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg := OAuthConfig{Resource: hop.URL + "/mcp", Client: OAuthClientConfig{Registration: OAuthRegistrationDynamic, TokenEndpointAuth: OAuthAuthNone}}
	if _, err := (&oauthClient{http: cl}).discover(context.Background(), cfg); err == nil {
		t.Error("discovery through a redirect succeeded")
	}
	// The production client is built the same way.
	if NewOAuthHTTPClient(nil).CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("the production client follows redirects")
	}
}

func TestOAuthDiscover_GuardedClientRefusesPrivateAddress(t *testing.T) {
	f := oauthtest.New(t)
	c := NewOAuthHTTPClient(nil)
	// Trust the test certificate, keep the guard's dialer.
	c.Transport.(*http.Transport).TLSClientConfig = f.Srv.Client().Transport.(*http.Transport).TLSClientConfig
	if _, err := (&oauthClient{http: c}).discover(context.Background(), fakeConfig(f)); err == nil {
		t.Error("the guarded client reached a loopback address")
	}
	if f.Count("prm") != 0 {
		t.Error("the loopback server was contacted")
	}
}

func TestOAuthRegister_SendsDeviceRefreshAndCodeGrantsWithLoopbackRedirect(t *testing.T) {
	f := oauthtest.New(t)
	c := &oauthClient{http: f.Client()}
	ep, err := c.discover(context.Background(), fakeConfig(f))
	if err != nil {
		t.Fatal(err)
	}
	id, secret, err := c.register(context.Background(), ep, fakeConfig(f))
	if err != nil {
		t.Fatalf("register (the fake refuses a device-only client): %v", err)
	}
	if id == "" || secret != "" {
		t.Errorf("client_id %q secret %q", id, secret)
	}
	var body map[string]any
	f.Set(func(f *oauthtest.Server) { body = f.Registrations[0] })
	if body["token_endpoint_auth_method"] != "none" || body["client_name"] != oauthClientName {
		t.Errorf("registration body %v", body)
	}
	if rt, _ := body["response_types"].([]any); len(rt) != 1 || rt[0] != "code" {
		t.Errorf("response_types %v", body["response_types"])
	}
}

// An error says what the server's error code was, never what its body held.
func TestOAuthClient_ErrorsNeverQuoteResponseBody(t *testing.T) {
	const secret = "rt-the-refresh-token-value"
	for name, h := range map[string]http.HandlerFunc{
		"500 echo": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("failed for " + secret))
		},
		"400 non-oauth": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte("<html>" + secret + "</html>"))
		},
		"oauth description echo": func(w http.ResponseWriter, r *http.Request) {
			oauthtest.WriteJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "token " + secret + " revoked"})
		},
		"bogus code": func(w http.ResponseWriter, r *http.Request) {
			oauthtest.WriteJSON(w, 400, map[string]any{"error": secret + " <script>"})
		},
		"200 garbage": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{" + secret))
		},
	} {
		srv := httptest.NewTLSServer(h)
		c := &oauthClient{http: srv.Client()}
		sec := oauthSecret{ClientID: "c", RefreshToken: secret, Endpoints: oauthEndpoints{Token: srv.URL + "/token"}}
		_, err := c.tokenRefresh(context.Background(), OAuthConfig{Resource: "https://r.example.test"}, sec)
		srv.Close()
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error quotes the token: %v", name, err)
		}
	}
}

func TestOAuthClient_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	cl := srv.Client()
	cl.Timeout = 20 * time.Millisecond
	sec := oauthSecret{ClientID: "c", RefreshToken: "x", Endpoints: oauthEndpoints{Token: srv.URL + "/token"}}
	_, err := (&oauthClient{http: cl}).tokenRefresh(context.Background(), OAuthConfig{}, sec)
	if !isTransient(err) {
		t.Errorf("err = %v, want transient", err)
	}
}
