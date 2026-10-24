// Package oidctest is an in-process OpenID Connect provider for tests: real
// RSA/EC keys, a discovery document, an authorization endpoint that redirects
// straight back with a code, a token endpoint that enforces client
// authentication, single-use codes, PKCE S256 and the exact redirect URI, and
// a JWKS endpoint. No network leaves the process.
package oidctest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Key is one signing key of the provider.
type Key struct {
	ID   string
	Alg  jose.SignatureAlgorithm
	Priv any
}

// NewRSAKey makes an RS256 key.
func NewRSAKey(id string) Key {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return Key{ID: id, Alg: jose.RS256, Priv: k}
}

// NewECKey makes an ES256 key.
func NewECKey(id string) Key {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return Key{ID: id, Alg: jose.ES256, Priv: k}
}

func (k Key) public() jose.JSONWebKey {
	switch p := k.Priv.(type) {
	case *rsa.PrivateKey:
		return jose.JSONWebKey{Key: &p.PublicKey, KeyID: k.ID, Algorithm: string(k.Alg), Use: "sig"}
	case *ecdsa.PrivateKey:
		return jose.JSONWebKey{Key: &p.PublicKey, KeyID: k.ID, Algorithm: string(k.Alg), Use: "sig"}
	}
	panic("key type")
}

type grant struct {
	clientID, redirectURI, challenge, nonce string
	claims                                  map[string]any
	used                                    bool
	at                                      time.Time
}

// Provider is the fake IdP.
type Provider struct {
	Server       *httptest.Server
	ClientID     string
	ClientSecret string

	mu sync.Mutex
	// Issuer defaults to the server URL; a test may change it.
	Issuer string
	// Keys publishes in the JWKS; Signing signs ID tokens.
	Keys    []Key
	Signing Key
	// Claims are the next authorization's identity claims (sub, email …).
	Claims map[string]any
	// Mutate edits an ID token's claims just before signing (a test's
	// forgery).
	Mutate func(map[string]any)
	// IDToken, when set, replaces the signed token outright.
	IDToken string
	// Now is the provider's clock.
	Now        func() time.Time
	grants     map[string]*grant
	JWKSHits   int
	TokenCalls int
	// LastAuthorize is the last authorization request's query.
	LastAuthorize url.Values
}

// New starts a provider for one client.
func New(clientID, clientSecret string) *Provider {
	p := &Provider{ClientID: clientID, ClientSecret: clientSecret, grants: map[string]*grant{}, Now: time.Now}
	k := NewRSAKey("k1")
	p.Keys, p.Signing = []Key{k}, k
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/jwks", p.jwks)
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	p.Server = httptest.NewServer(mux)
	p.Issuer = p.Server.URL
	return p
}

// Close stops the server.
func (p *Provider) Close() { p.Server.Close() }

// Set changes provider state under its lock.
func (p *Provider) Set(f func(p *Provider)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	iss := p.Issuer
	p.mu.Unlock()
	base := p.Server.URL
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer": iss, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
		"jwks_uri": base + "/jwks", "end_session_endpoint": base + "/logout",
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.JWKSHits++
	set := jose.JSONWebKeySet{}
	for _, k := range p.Keys {
		set.Keys = append(set.Keys, k.public())
	}
	_ = json.NewEncoder(w).Encode(set)
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authorize "authenticates" the person instantly and redirects back with a
// code bound to the client, redirect URI, PKCE challenge and nonce.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.mu.Lock()
	p.LastAuthorize = q
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" {
		p.mu.Unlock()
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	claims := map[string]any{}
	for k, v := range p.Claims {
		claims[k] = v
	}
	code := randomHex()
	p.grants[code] = &grant{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"),
		nonce: q.Get("nonce"), claims: claims, at: p.Now()}
	p.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	back := u.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	u.RawQuery = back.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.TokenCalls++
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(p.ClientSecret)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}
	g, ok := p.grants[r.PostForm.Get("code")]
	if !ok || g.used || g.clientID != id {
		tokenError(w, "invalid_grant")
		return
	}
	// A code is redeemable once (RFC 6749 §4.1.2).
	g.used = true
	if r.PostForm.Get("redirect_uri") != g.redirectURI {
		tokenError(w, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError(w, "invalid_grant")
		return
	}
	now := p.Now()
	claims := map[string]any{"iss": p.Issuer, "aud": p.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"auth_time": g.at.Unix()}
	if g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	if p.Mutate != nil {
		p.Mutate(claims)
	}
	tok := p.IDToken
	if tok == "" {
		tok = Sign(p.Signing, claims)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": randomHex(), "token_type": "Bearer", "expires_in": 300, "id_token": tok})
}

// Sign signs claims with k as a compact JWT.
func Sign(k Key, claims map[string]any) string {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: k.Alg, Key: jose.JSONWebKey{Key: k.Priv, KeyID: k.ID}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	body, _ := json.Marshal(claims)
	obj, err := s.Sign(body)
	if err != nil {
		panic(err)
	}
	out, _ := obj.CompactSerialize()
	return out
}
