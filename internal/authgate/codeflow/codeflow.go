// Package codeflow is the OAuth 2.0 authorization-code machinery the redirect
// gates share (RFC FA): OIDC (P1) and plain OAuth (P5). Code + PKCE (S256)
// only; the state, the PKCE verifier and the OIDC nonce of one login all
// derive from a secret held only in the browser's login cookie, so:
//
//   - a callback from a browser that did not start the login carries no
//     cookie secret and cannot finish it (login CSRF, code injection);
//   - the server's login-state row stores hashes only, so a database read
//     cannot complete anyone's login;
//   - nothing the provider returns reaches the browser: the gate is a
//     confidential client (a BFF, RFC 10017) and the session cookie is all
//     the browser ever holds.
package codeflow

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

// secretBytes and stateBytes are the entropy of the cookie secret and the
// state (128 bits each would do; 256 costs nothing).
const (
	secretBytes = 32
	stateBytes  = 32
)

// TTL is how long a started login may take (RFC FA §3.5: 10 minutes).
const TTL = 10 * time.Minute

// ErrStateMismatch: the callback's state is not the login's.
var ErrStateMismatch = errors.New("codeflow: state mismatch")

// Login is a freshly started login: Secret goes in the browser's login cookie,
// State in the provider redirect, and the hashes in the server's row.
type Login struct {
	Secret []byte
	State  string
}

// NewLogin draws a login's secret and state from the CSPRNG.
func NewLogin() (Login, error) {
	sec := make([]byte, secretBytes)
	st := make([]byte, stateBytes)
	if _, err := rand.Read(sec); err != nil {
		return Login{}, err
	}
	if _, err := rand.Read(st); err != nil {
		return Login{}, err
	}
	return Login{Secret: sec, State: base64.RawURLEncoding.EncodeToString(st)}, nil
}

// derive is an HMAC keyed by the cookie secret, domain-separated by label.
func derive(secret []byte, label, state string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("loomcycle-codeflow\x00" + label + "\x00" + state))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verifier is the login's PKCE code verifier: 43 characters of the unreserved
// set (RFC 7636 §4.1).
func Verifier(secret []byte, state string) string { return derive(secret, "pkce", state) }

// Nonce is the login's OIDC nonce.
func Nonce(secret []byte, state string) string { return derive(secret, "nonce", state) }

// StateHash is the row key for a state.
func StateHash(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// BindingHash is what the row stores to recognise the browser's secret.
func BindingHash(secret []byte) string {
	sum := sha256.Sum256(append([]byte("loomcycle-codeflow-binding\x00"), secret...))
	return hex.EncodeToString(sum[:])
}

// EncodeSecret / DecodeSecret are the cookie value form.
func EncodeSecret(secret []byte) string { return base64.RawURLEncoding.EncodeToString(secret) }

// DecodeSecret reads a cookie value; false for anything that is not a secret.
func DecodeSecret(v string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(b) != secretBytes {
		return nil, false
	}
	return b, true
}

// State builds the gate-facing LoginState from the cookie secret and state.
func State(secret []byte, state, redirectURI string, reauth bool, started time.Time) authgate.LoginState {
	return authgate.LoginState{State: state, Nonce: Nonce(secret, state), CodeVerifier: Verifier(secret, state),
		RedirectURI: redirectURI, Reauth: reauth, StartedAt: started}
}

// Client is a confidential OAuth client of one provider.
type Client struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	Scopes       []string
	// AuthStyle is how the client authenticates at the token endpoint; zero
	// detects it.
	AuthStyle oauth2.AuthStyle
	// HTTP is the client for the token endpoint (bounded timeout).
	HTTP *http.Client
}

func (c *Client) config(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: redirectURI, Scopes: c.Scopes,
		Endpoint: oauth2.Endpoint{AuthURL: c.AuthURL, TokenURL: c.TokenURL, AuthStyle: c.AuthStyle},
	}
}

// AuthCodeURL is the provider URL a login redirects the browser to. extra
// adds provider parameters (hd, prompt …); it cannot override the ones that
// make the flow safe.
func (c *Client) AuthCodeURL(ls authgate.LoginState, oidc bool, extra map[string]string) (string, error) {
	if ls.State == "" || ls.CodeVerifier == "" || ls.RedirectURI == "" || (oidc && ls.Nonce == "") {
		return "", errors.New("codeflow: login state incomplete")
	}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(ls.CodeVerifier)}
	if oidc {
		opts = append(opts, oauth2.SetAuthURLParam("nonce", ls.Nonce))
	}
	if ls.Reauth {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	for k, v := range extra {
		switch strings.ToLower(k) {
		case "state", "nonce", "code_challenge", "code_challenge_method", "redirect_uri", "client_id", "response_type", "scope":
			return "", fmt.Errorf("codeflow: parameter %q may not be overridden", k)
		case "prompt", "max_age":
			if ls.Reauth {
				continue
			}
		}
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	return c.config(ls.RedirectURI).AuthCodeURL(ls.State, opts...), nil
}

// Exchange redeems the callback's code with the login's verifier.
func (c *Client) Exchange(ctx context.Context, code string, ls authgate.LoginState) (*oauth2.Token, error) {
	if code == "" || len(code) > 4096 {
		return nil, errors.New("codeflow: missing or oversized code")
	}
	if c.HTTP != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, c.HTTP)
	}
	return c.config(ls.RedirectURI).Exchange(ctx, code, oauth2.VerifierOption(ls.CodeVerifier))
}

// CheckCallback verifies the callback's state against the login's in constant
// time, and surfaces a provider error.
func CheckCallback(r *http.Request, ls authgate.LoginState) (code string, err error) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// The provider's own error code is safe to log; its description is
		// provider text and is not echoed.
		return "", fmt.Errorf("codeflow: provider refused the login: %.64s", e)
	}
	st := q.Get("state")
	if st == "" || !hmac.Equal([]byte(st), []byte(ls.State)) {
		return "", ErrStateMismatch
	}
	return q.Get("code"), nil
}
