// Package oidc is the OpenID Connect gate (RFC FA §3.2, kind: oidc): the
// authorization-code flow with PKCE S256 against one provider, as a
// confidential client. The provider's tokens never leave the server: the gate
// redeems the code, verifies the ID token and returns an assertion, and the
// HTTP server turns that into a session cookie.
//
// Presets fix what a provider needs beyond the standard checks: Google's hd
// (Workspace domain) and email_verified, Entra's tid allowlist and templated
// issuer, Cognito's token_use. Everything else (iss, aud, azp, exp/iat skew,
// nonce, the key by kid) is the shared verifier's.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/codeflow"
	"github.com/denn-gubsky/loomcycle/internal/authgate/jwtverify"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Kind is the gate kind.
const Kind = config.AuthGateKindOIDC

// AMROIDC is recorded when the provider reports no amr.
const AMROIDC = "oidc"

const (
	// metadataMaxAge is how long a discovery document is used.
	metadataMaxAge = time.Hour
	// metadataRetry spaces discovery attempts after a failure.
	metadataRetry = 30 * time.Second
	// idTokenMaxAge: an ID token comes straight from the token endpoint, so
	// it is seconds old; anything older is a replay.
	idTokenMaxAge    = 5 * time.Minute
	maxMetadataBytes = 1 << 20
	maxAMR           = 8
)

// metadata is the part of the provider's discovery document the gate uses.
type metadata struct {
	Issuer                 string   `json:"issuer"`
	AuthorizationEndpoint  string   `json:"authorization_endpoint"`
	TokenEndpoint          string   `json:"token_endpoint"`
	JWKSURI                string   `json:"jwks_uri"`
	EndSessionEndpoint     string   `json:"end_session_endpoint"`
	IDTokenSigningAlgs     []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethods   []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMeths []string `json:"token_endpoint_auth_methods_supported"`
}

// Gate is one configured OIDC gate. It caches the provider's metadata and key
// set, so the server keeps one per gate across requests.
type Gate struct {
	name string
	cfg  config.AuthOIDCGate
	http *http.Client
	now  func() time.Time

	mu      sync.Mutex
	meta    *metadata
	metaAt  time.Time
	triedAt time.Time
	keys    *jwtverify.RemoteJWKS
}

// New builds the gate. client bounds every call to the provider; nil uses a
// 10-second client.
func New(name string, cfg config.AuthOIDCGate, client *http.Client) *Gate {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Gate{name: name, cfg: cfg, http: client, now: time.Now}
}

// SetClock replaces the clock (tests).
func (g *Gate) SetClock(now func() time.Time) { g.now = now }

// Kind implements authgate.Gate.
func (g *Gate) Kind() string { return Kind }

// Verify: an OIDC login is never an API credential.
func (g *Gate) Verify(context.Context, *http.Request) (authgate.Assertion, error) {
	return authgate.Assertion{}, authgate.ErrNoCredential
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: oidc: "+format, append([]any{authgate.ErrInvalidCredential}, args...)...)
}

// endpointOK accepts an https URL, or http on loopback (a local provider).
func endpointOK(v string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	h := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"))
}

// discover returns the provider metadata, fetching it when stale.
func (g *Gate) discover(ctx context.Context) (*metadata, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.meta != nil && now.Sub(g.metaAt) < metadataMaxAge {
		return g.meta, nil
	}
	if !g.triedAt.IsZero() && now.Sub(g.triedAt) < metadataRetry {
		if g.meta != nil {
			return g.meta, nil
		}
		return nil, errors.New("oidc: provider discovery failed recently; retrying shortly")
	}
	g.triedAt = now
	m, err := g.fetchMetadata(ctx)
	if err != nil {
		if g.meta != nil {
			return g.meta, nil
		}
		return nil, err
	}
	if g.keys == nil || g.keys.URL != m.JWKSURI {
		g.keys = &jwtverify.RemoteJWKS{URL: m.JWKSURI, Client: g.http, Now: g.now}
	}
	g.meta, g.metaAt = m, now
	return m, nil
}

func (g *Gate) fetchMetadata(ctx context.Context) (*metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.DiscoveryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: discovery: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err != nil || len(body) > maxMetadataBytes {
		return nil, errors.New("oidc: discovery: unreadable or oversized document")
	}
	var m metadata
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	// The document must be the configured issuer's own (OIDC Discovery §4.3):
	// a document naming another issuer is a misconfiguration or an attack.
	if m.Issuer != g.cfg.Issuer {
		return nil, fmt.Errorf("oidc: discovery: issuer %q is not the configured %q", m.Issuer, g.cfg.Issuer)
	}
	for _, e := range []string{m.AuthorizationEndpoint, m.TokenEndpoint, m.JWKSURI} {
		if !endpointOK(e) {
			return nil, fmt.Errorf("oidc: discovery: endpoint %q is not https", e)
		}
	}
	if m.EndSessionEndpoint != "" && !endpointOK(m.EndSessionEndpoint) {
		m.EndSessionEndpoint = ""
	}
	if len(m.CodeChallengeMethods) > 0 && !slices.Contains(m.CodeChallengeMethods, "S256") {
		return nil, errors.New("oidc: discovery: the provider does not support PKCE S256")
	}
	return &m, nil
}

func (g *Gate) client(m *metadata) *codeflow.Client {
	c := &codeflow.Client{ClientID: g.cfg.ClientID, ClientSecret: g.cfg.ClientSecret, AuthURL: m.AuthorizationEndpoint,
		TokenURL: m.TokenEndpoint, Scopes: g.cfg.Scopes, HTTP: g.http}
	switch g.cfg.TokenAuth {
	case "basic":
		c.AuthStyle = oauth2.AuthStyleInHeader
	case "post":
		c.AuthStyle = oauth2.AuthStyleInParams
	}
	return c
}

// Begin redirects the browser to the provider.
func (g *Gate) Begin(ctx context.Context, w http.ResponseWriter, r *http.Request, ls authgate.LoginState) error {
	u, err := g.AuthURL(ctx, ls)
	if err != nil {
		return err
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
	return nil
}

// AuthURL is the provider authorization URL for the login.
func (g *Gate) AuthURL(ctx context.Context, ls authgate.LoginState) (string, error) {
	m, err := g.discover(ctx)
	if err != nil {
		return "", err
	}
	extra := map[string]string{}
	if g.cfg.Preset == config.OIDCPresetGoogle && len(g.cfg.AllowedDomains) == 1 {
		// A hint to Google's account chooser only; the hd claim is what is
		// checked.
		extra["hd"] = g.cfg.AllowedDomains[0]
	}
	return g.client(m).AuthCodeURL(ls, true, extra)
}

// Complete redeems the callback's code and verifies the ID token.
func (g *Gate) Complete(ctx context.Context, r *http.Request, ls authgate.LoginState) (authgate.Assertion, error) {
	code, err := codeflow.CheckCallback(r, ls)
	if err != nil {
		return authgate.Assertion{}, fmt.Errorf("%w: %w", authgate.ErrInvalidCredential, err)
	}
	m, err := g.discover(ctx)
	if err != nil {
		return authgate.Assertion{}, err
	}
	tok, err := g.client(m).Exchange(ctx, code, ls)
	if err != nil {
		// The provider's error body is not echoed (it may quote the request).
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return authgate.Assertion{}, invalid("token endpoint refused the code (%s)", re.ErrorCode)
		}
		return authgate.Assertion{}, fmt.Errorf("oidc: token endpoint: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return authgate.Assertion{}, invalid("no id_token in the token response")
	}
	return g.verifyIDToken(ctx, m, raw, ls)
}

func (g *Gate) algorithms(m *metadata) []jose.SignatureAlgorithm {
	var out []jose.SignatureAlgorithm
	for _, a := range m.IDTokenSigningAlgs {
		if alg := jose.SignatureAlgorithm(a); jwtverify.AllowedAlgorithm(alg) {
			out = append(out, alg)
		}
	}
	if len(out) == 0 {
		out = []jose.SignatureAlgorithm{jose.RS256}
	}
	return out
}

func (g *Gate) issuerOK(iss string, c jwtverify.Claims) bool {
	switch {
	case g.cfg.Preset == config.OIDCPresetGoogle:
		return iss == config.GoogleIssuer || iss == "accounts.google.com"
	case g.cfg.IssuerTemplate:
		tid, _ := c.String("tid")
		return tid != "" && iss == strings.Replace(g.cfg.Issuer, "{tenantid}", tid, 1)
	}
	return jwtverify.ConstantEqual(iss, g.cfg.Issuer)
}

func (g *Gate) verifyIDToken(ctx context.Context, m *metadata, raw string, ls authgate.LoginState) (authgate.Assertion, error) {
	g.mu.Lock()
	keys := g.keys
	g.mu.Unlock()
	v := &jwtverify.Verifier{Keys: keys, Algorithms: g.algorithms(m), IssuerOK: g.issuerOK, Audience: g.cfg.ClientID,
		MaxAge: idTokenMaxAge, RequireIAT: true, Now: g.now}
	c, err := v.Verify(ctx, raw)
	if err != nil {
		return authgate.Assertion{}, fmt.Errorf("%w: oidc: id_token: %w", authgate.ErrInvalidCredential, err)
	}
	nonce, _ := c.String("nonce")
	if ls.Nonce == "" || !jwtverify.ConstantEqual(nonce, ls.Nonce) {
		return authgate.Assertion{}, invalid("nonce mismatch")
	}
	auds := c.Strings("aud")
	azp, hasAZP := c.String("azp")
	if (len(auds) > 1 || hasAZP) && azp != g.cfg.ClientID {
		return authgate.Assertion{}, invalid("authorized party is not this client")
	}
	sub, _ := c.String("sub")
	if sub == "" {
		return authgate.Assertion{}, invalid("no sub")
	}
	if err := g.presetChecks(c); err != nil {
		return authgate.Assertion{}, err
	}
	var authTime time.Time
	if t, ok, err := c.Time("auth_time"); err != nil {
		return authgate.Assertion{}, invalid("auth_time malformed")
	} else if ok {
		if t.After(g.now().Add(jwtverify.DefaultSkew)) {
			return authgate.Assertion{}, invalid("auth_time in the future")
		}
		authTime = t
	}
	if ls.Reauth {
		// max_age=0 was asked for, so the provider must say when the person
		// authenticated, and it must be during this login.
		if authTime.IsZero() || authTime.Before(ls.StartedAt.Add(-jwtverify.DefaultSkew)) {
			return authgate.Assertion{}, invalid("the provider did not re-authenticate the person")
		}
	}
	iss, _ := c.String("iss")
	email, _ := c.String("email")
	name, _ := c.String("name")
	if name == "" {
		name, _ = c.String("preferred_username")
	}
	exp, _, _ := c.Time("exp")
	return authgate.Assertion{
		Subject: sub, Issuer: iss, Email: email, EmailVerified: c.Bool("email_verified"), DisplayName: name,
		AMR: amrOf(c), AuthTime: authTime, Claims: map[string]any(c), ExpiresAt: exp,
	}, nil
}

// presetChecks are the provider-specific claims a preset requires.
func (g *Gate) presetChecks(c jwtverify.Claims) error {
	switch g.cfg.Preset {
	case config.OIDCPresetGoogle:
		// Workspace membership is the hd claim, never the e-mail's domain
		// (a personal account can carry any address), and the address must
		// be verified. allow_any_account admits every account, so no hd is
		// asked for; the address must still be verified.
		hd, _ := c.String("hd")
		if !g.cfg.AnyAccount && !slices.Contains(g.cfg.AllowedDomains, strings.ToLower(hd)) {
			return invalid("hd %q is not an allowed domain", hd)
		}
		if !c.Bool("email_verified") {
			return invalid("email not verified")
		}
	case config.OIDCPresetEntra:
		tid, _ := c.String("tid")
		if !slices.Contains(g.cfg.AllowedTIDs, strings.ToLower(tid)) {
			return invalid("tid %q is not an allowed tenant", tid)
		}
	case config.OIDCPresetCognito:
		if use, _ := c.String("token_use"); use != "id" {
			return invalid("token_use %q is not id", use)
		}
	}
	return nil
}

// amrOf is the provider's amr claim, bounded and charset-clean, or ["oidc"].
func amrOf(c jwtverify.Claims) []string {
	var out []string
	for _, v := range c.Strings("amr") {
		if len(v) == 0 || len(v) > 32 || strings.IndexFunc(v, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) >= 0 {
			continue
		}
		out = append(out, v)
		if len(out) == maxAMR {
			break
		}
	}
	if len(out) == 0 {
		return []string{AMROIDC}
	}
	return out
}

// EndSessionURL is where to send the browser at logout (RP-initiated logout),
// "" when the gate does not do it or the provider has no endpoint.
func (g *Gate) EndSessionURL(ctx context.Context, postLogout string) string {
	if !g.cfg.EndSession {
		return ""
	}
	m, err := g.discover(ctx)
	if err != nil || m.EndSessionEndpoint == "" {
		return ""
	}
	u, err := url.Parse(m.EndSessionEndpoint)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_id", g.cfg.ClientID)
	if postLogout != "" {
		q.Set("post_logout_redirect_uri", postLogout)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
