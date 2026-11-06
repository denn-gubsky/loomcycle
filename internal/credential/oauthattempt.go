package credential

// Recovering from an access token the resource server refused (RFC GB
// §4.5). A token is refreshed before it expires, but a server may still
// refuse it — clock skew, or the access token revoked while the grant
// holds. The sender of a request then refreshes the credential once, even
// though its token has not expired, and sends the request once more; a
// second refusal means the grant is no good for that server, and its owner
// must connect it again.
//
// The sender (the MCP HTTP client) only sees header strings, so it does
// not name the credentials itself: it carries an OAuthAttempt on the
// request's ctx, Substitute notes on it each token it hands out, and the
// sender reports a refusal to it.

import (
	"context"
	"crypto/subtle"
	"sync"
)

// OAuthAttempt follows the $oauth: tokens one outbound request sends
// across its at most two tries. Safe for concurrent use; not reused across
// requests.
type OAuthAttempt struct {
	mu         sync.Mutex
	rejections int
	sent       map[string]oauthSent // by credential name, the latest send
	// refused are the tokens of the first refusal, by credential name. Every
	// substitution of the retry — a re-initialize, then the request — is
	// measured against them, not against what the retry itself sent.
	refused map[string]string
}

type oauthSent struct {
	token string
	// reauth moves the credential to needs_reauth when token is still the
	// one stored, and returns the typed error either way.
	reauth func(ctx context.Context) error
}

// NewOAuthAttempt starts following one request.
func NewOAuthAttempt() *OAuthAttempt {
	return &OAuthAttempt{sent: map[string]oauthSent{}, refused: map[string]string{}}
}

type oauthAttemptKey struct{}

// WithOAuthAttempt returns ctx carrying a, for Substitute to note the
// tokens it hands out on.
func WithOAuthAttempt(ctx context.Context, a *OAuthAttempt) context.Context {
	return context.WithValue(ctx, oauthAttemptKey{}, a)
}

func oauthAttemptFrom(ctx context.Context) *OAuthAttempt {
	a, _ := ctx.Value(oauthAttemptKey{}).(*OAuthAttempt)
	return a
}

// Sent reports whether the request carried an $oauth: token at all.
func (a *OAuthAttempt) Sent() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent) > 0
}

// Rejected records that the server refused the request's token. The first
// refusal returns nil: the next substitution under a force-refreshes each
// credential it sent. A later one moves each credential the retry sent to
// needs_reauth (unless someone refreshed it since) and returns an
// *OAuthReauthError, never naming a token.
func (a *OAuthAttempt) Rejected(ctx context.Context) error {
	a.mu.Lock()
	a.rejections++
	n := a.rejections
	sent := make([]oauthSent, 0, len(a.sent))
	for name, s := range a.sent {
		sent = append(sent, s)
		if n == 1 {
			a.refused[name] = s.token
		}
	}
	a.mu.Unlock()
	if n == 1 {
		return nil
	}
	var first error
	for _, s := range sent {
		if err := s.reauth(ctx); err != nil && first == nil {
			first = err
		}
	}
	if first == nil {
		first = ErrOAuthReauthRequired
	}
	return first
}

// rejected returns the token name was sent with, when the server refused it
// and this is the retry. Nil-safe: a request followed by no attempt never
// retries.
func (a *OAuthAttempt) rejected(name string) (string, bool) {
	if a == nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tok, ok := a.refused[name]
	return tok, ok && a.rejections == 1
}

func (a *OAuthAttempt) note(name string, s oauthSent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent[name] = s
}

// sameToken reports whether stored is the token a server refused, in
// constant time. An empty rejected token matches nothing.
func sameToken(stored, rejected string) bool {
	return rejected != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(rejected)) == 1
}
