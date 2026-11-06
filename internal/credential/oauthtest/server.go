// Package oauthtest is an invented OAuth authorization server and protected
// resource for tests of OAuth credentials (RFC GB): protected resource
// metadata, authorization server metadata, dynamic registration that refuses
// a device-only client, the device grant, a token endpoint that rotates the
// refresh token on every refresh, revocation and userinfo. It names no real
// service. Tests only.
package oauthtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The grant types the server knows.
const (
	grantDeviceCode   = "urn:ietf:params:oauth:grant-type:device_code"
	grantRefreshToken = "refresh_token"
	grantAuthCode     = "authorization_code"
)

// Server is an invented OAuth authorization server and protected resource
// on one TLS test server: protected resource metadata, authorization server
// metadata, dynamic registration that refuses a device-only client, the
// device grant, a token endpoint that rotates the refresh token on every
// refresh, revocation and userinfo. Knobs make it misbehave; counters say
// what was called.
type Server struct {
	t   *testing.T
	Srv *httptest.Server

	mu sync.Mutex // guards every field below
	// knobs
	ApproveAfter    int  // device polls answered authorization_pending first
	SlowDowns       int  // then this many slow_down
	DenyDevice      bool // access_denied instead of a grant
	AccessTTL       int64
	GrantOpenID     bool
	Fail5xx         int  // token requests answered 500 (body echoes the refresh token)
	InvalidClient   int  // device requests answered invalid_client
	Revoke500       bool // revocation answers 500
	TokenDelay      time.Duration
	RequireResource bool
	MetaOverride    func(map[string]any)
	Interval        int // the device poll interval it names (default 5)
	// Resource serves the protected resource itself (at Resource()); nil
	// answers 404.
	ResourceHandler http.Handler

	// state
	clients       map[string]bool
	devices       map[string]*fakeDevice
	refresh       map[string]bool // live refresh tokens
	access        map[string]bool
	Revoked       []string
	IssuedAccess  []string
	IssuedRefresh []string
	IssuedDevice  []string
	DevicePolls   int
	Registrations []map[string]any
	TokenForms    []map[string][]string

	calls map[string]*atomic.Int64
}

type fakeDevice struct {
	clientID string
	polls    int
}

// New starts a Server, closed when t ends.
func New(t *testing.T) *Server {
	t.Helper()
	f := &Server{t: t, AccessTTL: 300, RequireResource: true, Interval: 5,
		clients: map[string]bool{}, devices: map[string]*fakeDevice{}, refresh: map[string]bool{}, access: map[string]bool{},
		calls: map[string]*atomic.Int64{}}
	for _, k := range []string{"prm", "meta", "register", "device", "token", "revoke", "userinfo"} {
		f.calls[k] = &atomic.Int64{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", f.prm)
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.meta)
	mux.HandleFunc("/register", f.register)
	mux.HandleFunc("/device", f.device)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/revoke", f.revoke)
	mux.HandleFunc("/userinfo", f.userinfo)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h := f.ResourceHandler
		f.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	f.Srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.Srv.Close)
	return f
}

// Resource is the protected resource's URL.
func (f *Server) Resource() string { return f.Srv.URL + "/mcp" }

// Count is how often endpoint k (prm, meta, register, device, token,
// revoke, userinfo) was called.
func (f *Server) Count(k string) int64 { return f.calls[k].Load() }

// Client is an HTTP client that trusts the test server and follows no
// redirect, as production's does.
func (f *Server) Client() *http.Client {
	c := f.Srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func randTok(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// WriteJSON writes v as a JSON answer with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Server) prm(w http.ResponseWriter, r *http.Request) {
	f.calls["prm"].Add(1)
	WriteJSON(w, 200, map[string]any{"resource": f.Resource(), "authorization_servers": []string{f.Srv.URL}})
}

func (f *Server) meta(w http.ResponseWriter, r *http.Request) {
	f.calls["meta"].Add(1)
	m := map[string]any{
		"issuer":                        f.Srv.URL,
		"token_endpoint":                f.Srv.URL + "/token",
		"device_authorization_endpoint": f.Srv.URL + "/device",
		"registration_endpoint":         f.Srv.URL + "/register",
		"revocation_endpoint":           f.Srv.URL + "/revoke",
		"userinfo_endpoint":             f.Srv.URL + "/userinfo",
	}
	f.mu.Lock()
	if f.MetaOverride != nil {
		f.MetaOverride(m)
	}
	f.mu.Unlock()
	WriteJSON(w, 200, m)
}

// register refuses a client that asks for the device grant alone, as some
// authorization servers do: it must also be a code-grant native client with
// a loopback redirect.
func (f *Server) register(w http.ResponseWriter, r *http.Request) {
	f.calls["register"].Add(1)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Registrations = append(f.Registrations, body)
	grants, _ := body["grant_types"].([]any)
	has := func(g string) bool { return slices.Contains(grants, any(g)) }
	redirects, _ := body["redirect_uris"].([]any)
	loopback := len(redirects) == 1 && strings.HasPrefix(redirects[0].(string), "http://127.0.0.1:")
	if !has(grantAuthCode) || !has(grantDeviceCode) || !has(grantRefreshToken) || !loopback {
		WriteJSON(w, 400, map[string]any{"error": "invalid_client_metadata", "error_description": "a native client with a loopback redirect is required"})
		return
	}
	id := randTok("client-")
	f.clients[id] = true
	WriteJSON(w, 201, map[string]any{"client_id": id})
}

func (f *Server) device(w http.ResponseWriter, r *http.Request) {
	f.calls["device"].Add(1)
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InvalidClient > 0 || !f.clients[r.Form.Get("client_id")] {
		if f.InvalidClient > 0 {
			f.InvalidClient--
		}
		WriteJSON(w, 401, map[string]any{"error": "invalid_client"})
		return
	}
	code := randTok("devcode-")
	f.devices[code] = &fakeDevice{clientID: r.Form.Get("client_id")}
	f.IssuedDevice = append(f.IssuedDevice, code)
	WriteJSON(w, 200, map[string]any{"device_code": code, "user_code": "ABCD-EFGH",
		"verification_uri": f.Srv.URL + "/verify", "verification_uri_complete": f.Srv.URL + "/verify?code=ABCD-EFGH",
		"expires_in": 299, "interval": f.Interval})
}

func (f *Server) issue(w http.ResponseWriter) {
	at, rt := randTok("at-"), randTok("rt-")
	f.access[at], f.refresh[rt] = true, true
	f.IssuedAccess = append(f.IssuedAccess, at)
	f.IssuedRefresh = append(f.IssuedRefresh, rt)
	scope := "mcp"
	if f.GrantOpenID {
		scope = "openid mcp"
	}
	WriteJSON(w, 200, map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer",
		"expires_in": f.AccessTTL, "scope": scope})
}

func (f *Server) token(w http.ResponseWriter, r *http.Request) {
	f.calls["token"].Add(1)
	_ = r.ParseForm()
	f.mu.Lock()
	delay := f.TokenDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.TokenForms = append(f.TokenForms, r.Form)
	if !f.clients[r.Form.Get("client_id")] {
		WriteJSON(w, 401, map[string]any{"error": "invalid_client"})
		return
	}
	if f.RequireResource && r.Form.Get("resource") != f.Resource() {
		WriteJSON(w, 400, map[string]any{"error": "invalid_target"})
		return
	}
	switch r.Form.Get("grant_type") {
	case grantDeviceCode:
		d := f.devices[r.Form.Get("device_code")]
		if d == nil {
			WriteJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		f.DevicePolls++
		d.polls++
		switch {
		case d.polls <= f.ApproveAfter:
			WriteJSON(w, 400, map[string]any{"error": "authorization_pending"})
		case d.polls <= f.ApproveAfter+f.SlowDowns:
			WriteJSON(w, 400, map[string]any{"error": "slow_down"})
		case f.DenyDevice:
			WriteJSON(w, 400, map[string]any{"error": "access_denied"})
		default:
			delete(f.devices, r.Form.Get("device_code"))
			f.issue(w)
		}
	case grantRefreshToken:
		rt := r.Form.Get("refresh_token")
		if f.Fail5xx > 0 {
			f.Fail5xx--
			// A body that echoes the token: it must never be quoted.
			w.WriteHeader(500)
			_, _ = w.Write([]byte("internal error while handling " + rt))
			return
		}
		if !f.refresh[rt] {
			WriteJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "refresh token " + rt + " is not valid"})
			return
		}
		delete(f.refresh, rt) // every refresh rotates the refresh token
		f.issue(w)
	default:
		WriteJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (f *Server) revoke(w http.ResponseWriter, r *http.Request) {
	f.calls["revoke"].Add(1)
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Revoke500 {
		w.WriteHeader(500)
		return
	}
	tok := r.Form.Get("token")
	f.Revoked = append(f.Revoked, tok)
	delete(f.refresh, tok)
	w.WriteHeader(200)
}

func (f *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	f.calls["userinfo"].Add(1)
	f.mu.Lock()
	ok := f.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()
	if !ok {
		WriteJSON(w, 401, map[string]any{"error": "invalid_token"})
		return
	}
	WriteJSON(w, 200, map[string]any{"sub": "u-1", "email": "someone@example.test"})
}

// Secrets returns every token, device code and client id the server issued.
func (f *Server) Secrets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string{}, f.IssuedAccess...)
	out = append(out, f.IssuedRefresh...)
	return append(out, f.IssuedDevice...)
}

// LastAccess is the access token issued last.
func (f *Server) LastAccess() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.IssuedAccess) == 0 {
		return ""
	}
	return f.IssuedAccess[len(f.IssuedAccess)-1]
}

// Set changes the server's knobs or reads its state under its lock.
func (f *Server) Set(fn func(f *Server)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}
