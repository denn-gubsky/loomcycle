package anthropic_oauth_dev

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PKCEPair is one PKCE verifier + S256 challenge pair, generated fresh
// per authorization request. The verifier MUST be kept on the operator's
// machine (never sent to Anthropic) until the final token exchange.
type PKCEPair struct {
	Verifier  string // base64url(random(32 bytes)); ~43 chars
	Challenge string // base64url(sha256(verifier)); ~43 chars
}

// NewPKCEPair generates a fresh verifier + challenge per RFC 7636 S256.
// The verifier is 32 cryptographically-random bytes, base64url-encoded
// without padding (~43 chars). The challenge is sha256(verifier),
// base64url-encoded without padding.
func NewPKCEPair() (PKCEPair, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return PKCEPair{}, fmt.Errorf("pkce: read entropy: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCEPair{Verifier: verifier, Challenge: challenge}, nil
}

// CallbackResult is the data the localhost callback server receives
// after the operator authorises in their browser. Either Code is
// non-empty (success) or Err is non-nil (operator denied, or the
// authorize URL surfaced an error).
type CallbackResult struct {
	Code  string
	State string
	Err   error
}

// CallbackServer is a one-shot HTTP server that listens on
// 127.0.0.1:<port>/callback and resolves a single CallbackResult. Used
// by `loomcycle anthropic login`: start the server, open the browser,
// wait for one callback, shut down.
//
// The server binds to loopback only — never exposed beyond the
// operator's machine. After delivering one result (success or error),
// it stops listening; subsequent requests get 404.
type CallbackServer struct {
	listener net.Listener
	srv      *http.Server
	result   chan CallbackResult
	port     int
}

// StartCallbackServer binds the loopback listener and starts serving.
// Call WaitFor() to block on the operator's browser callback. Caller
// MUST defer cs.Close() — even on success — so a stalled `wait_for`
// doesn't leak a listener.
//
// port=0 means "let the OS pick a free port"; the chosen port is
// surfaced via cs.Port() so the caller can embed it in the authorize
// URL's redirect_uri.
func StartCallbackServer(port int) (*CallbackServer, error) {
	addr := net.JoinHostPort(CallbackHost, strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind callback %s: %w", addr, err)
	}
	cs := &CallbackServer{
		listener: listener,
		result:   make(chan CallbackResult, 1),
		port:     listener.Addr().(*net.TCPAddr).Port,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, cs.handleCallback)
	cs.srv = &http.Server{Handler: mux, ReadTimeout: 30 * time.Second}
	go func() {
		_ = cs.srv.Serve(listener) // returns ErrServerClosed on Close()
	}()
	return cs, nil
}

// Port returns the actual TCP port the listener bound to. Useful when
// the caller passed port=0 and needs to embed the OS-chosen port in
// the authorize URL.
func (cs *CallbackServer) Port() int { return cs.port }

// WaitFor blocks until the operator's browser hits the callback path
// OR the context's deadline fires OR Close() is called. Returns the
// CallbackResult populated by handleCallback, or ctx.Err() on timeout.
func (cs *CallbackServer) WaitFor(ctx context.Context) (CallbackResult, error) {
	select {
	case r := <-cs.result:
		return r, nil
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	}
}

// Close shuts down the listener + HTTP server. Idempotent. Always
// safe to defer immediately after StartCallbackServer.
func (cs *CallbackServer) Close() error {
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cs.srv.Shutdown(shutCtx)
	return cs.listener.Close()
}

func (cs *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res := CallbackResult{
		Code:  q.Get("code"),
		State: q.Get("state"),
	}
	if errParam := q.Get("error"); errParam != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = errParam
		}
		res.Err = fmt.Errorf("oauth callback error: %s", desc)
	} else if res.Code == "" {
		res.Err = fmt.Errorf("oauth callback: missing `code` query param")
	}
	// Reply with a self-contained HTML page so the browser shows
	// something readable. The page closes itself if the browser allows
	// it; otherwise the operator just closes the tab.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if res.Err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, callbackHTML, "Authorization failed", res.Err.Error())
	} else {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, callbackHTML, "Authorization successful", "You can close this tab and return to the terminal.")
	}
	// Deliver exactly one result. Subsequent callbacks (duplicate
	// browser tabs, refresh) get the same HTML but don't re-queue.
	select {
	case cs.result <- res:
	default:
	}
}

const callbackHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>
body { font-family: -apple-system, system-ui, sans-serif; max-width: 32em; margin: 4em auto; padding: 2em; line-height: 1.6; }
h1 { color: #333; }
.msg { color: #666; }
</style></head>
<body>
<h1>%[1]s</h1>
<p class="msg">%s</p>
<script>setTimeout(function(){window.close();}, 2000);</script>
</body></html>
`

// BuildAuthorizeURL constructs the URL the operator opens in their
// browser. The redirect_uri MUST match what's registered server-side
// for the Claude Code client_id (loopback callback with the matching
// port). State carries the PKCE verifier so the callback handler can
// thread it back to the token exchange — Pi's pattern.
func BuildAuthorizeURL(pkce PKCEPair, callbackPort int) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClaudeCodeClientID)
	q.Set("redirect_uri", fmt.Sprintf("http://%s:%d%s", CallbackHost, callbackPort, CallbackPath))
	q.Set("scope", strings.Join(Scopes, " "))
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", pkce.Verifier)
	return AuthorizeURL + "?" + q.Encode()
}

// tokenExchangeResponse mirrors Anthropic's OAuth token endpoint
// response. The `Token` field captures the underlying API shape;
// callers receive a fully-populated `Token` struct via the public
// helpers below.
type tokenExchangeResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type,omitempty"`
}

// ExchangeOptions controls the HTTP transport for the OAuth token
// endpoint. HTTPClient = nil uses http.DefaultClient. Endpoint = ""
// uses the production TokenURL — override only in tests against a
// httptest.Server.
type ExchangeOptions struct {
	HTTPClient *http.Client
	Endpoint   string
}

// ExchangeCodeForToken POSTs the authorization_code grant to the OAuth
// token endpoint. Returns a Token with the 5-min slack applied to its
// ExpiresAt.
func ExchangeCodeForToken(ctx context.Context, code, codeVerifier string, callbackPort int, opts ExchangeOptions) (Token, error) {
	body := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     ClaudeCodeClientID,
		"code":          code,
		"redirect_uri":  fmt.Sprintf("http://%s:%d%s", CallbackHost, callbackPort, CallbackPath),
		"code_verifier": codeVerifier,
	}
	return postTokenRequest(ctx, body, opts)
}

// RefreshAccessToken POSTs the refresh_token grant. The new token
// inherits ObtainedAt = now and ExpiresAt = now + expires_in - 5min
// slack. Anthropic may rotate the refresh_token; the caller persists
// whatever comes back.
func RefreshAccessToken(ctx context.Context, refreshToken string, opts ExchangeOptions) (Token, error) {
	body := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClaudeCodeClientID,
		"refresh_token": refreshToken,
	}
	return postTokenRequest(ctx, body, opts)
}

func postTokenRequest(ctx context.Context, body map[string]string, opts ExchangeOptions) (Token, error) {
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = TokenURL
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Token{}, fmt.Errorf("marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return Token{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return Token{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	var parsed tokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Token{}, fmt.Errorf("parse token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return Token{}, fmt.Errorf("token endpoint returned empty access_token")
	}
	return NewToken(parsed.AccessToken, parsed.RefreshToken, parsed.Scope, parsed.ExpiresIn), nil
}
