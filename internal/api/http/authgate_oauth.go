package http

// authgate_oauth.go — RFC FA P5: the OAuth gates (GitHub, GitLab, Gitea,
// generic userinfo) behind the redirect-login routes P1 built for OIDC
// (authgate_oidc.go). Everything after the provider says who the person is —
// the login-state row and cookie, provisioning, group sync, the identity
// session — is that shared machinery; this file only builds the gate.

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/denn-gubsky/loomcycle/internal/authgate/oauth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// oauthGateCache keeps one oauth.Gate per configured gate; a config reload
// that changes a gate's settings gets a fresh one.
type oauthGateCache struct {
	mu sync.Mutex
	m  map[string]oauthCached
}

type oauthCached struct {
	cfg  *config.AuthOAuthGate
	gate *oauth.Gate
}

// oauthGateNamed returns the configured OAuth gate of that name.
func (s *Server) oauthGateNamed(name string) (*oauth.Gate, config.AuthGate, bool) {
	g, ok := s.configuredGate(name)
	if !ok || g.Kind != config.AuthGateKindOAuth || g.OAuth == nil || g.Identity == nil {
		return nil, config.AuthGate{}, false
	}
	c := &s.oauthGates
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]oauthCached{}
	}
	if e, ok := c.m[name]; ok && e.cfg == g.OAuth {
		return e.gate, g, true
	}
	o := g.OAuth
	hc := s.oidcHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	style := oauth2.AuthStyleAutoDetect
	switch o.TokenAuth {
	case "basic":
		style = oauth2.AuthStyleInHeader
	case "post":
		style = oauth2.AuthStyleInParams
	}
	gate, err := oauth.New(oauth.Config{
		Name: name, Preset: o.Preset, ClientID: o.ClientID, ClientSecret: o.ClientSecret, BaseURL: o.BaseURL,
		AuthURL: o.AuthURL, TokenURL: o.TokenURL, UserinfoURL: o.UserinfoURL,
		Claims: oauth.ClaimMap{Subject: o.Claims.Subject, Email: o.Claims.Email, EmailVerified: o.Claims.EmailVerified,
			Name: o.Claims.Name, Login: o.Claims.Login, Groups: o.Claims.Groups},
		Scopes: o.Scopes, AllowedOrgs: o.AllowedOrgs, RequireOrg: o.RequireOrg, TrustEmail: o.TrustEmail,
		AuthStyle: style, HTTP: hc,
	})
	if err != nil {
		return nil, config.AuthGate{}, false
	}
	c.m[name] = oauthCached{cfg: g.OAuth, gate: gate}
	return gate, g, true
}
