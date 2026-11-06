package credential

// Refreshing and substituting an OAuth credential's access token (RFC GB).
//
// A refresh spends the refresh token: every refresh at the authorization
// server rotates it, so two refreshes of one grant — two goroutines, or two
// replicas — would leave one of them holding a refresh token the server no
// longer knows, and the grant lost. Three things keep it to one:
//
//  1. Within a process, one flight per credential (singleflight). The flight
//     runs detached from its caller (a cancelled caller must not abandon a
//     rotation the server has already made) and bounded on its own.
//  2. Across replicas, a lease in the row, taken with the conditional write:
//     a replica that finds a live lease of another waits for it (using the
//     access token it has while that is still valid) instead of refreshing.
//  3. The rotated refresh token is written before the new access token is
//     handed to anyone. A write that fails, or loses to another writer, puts
//     the credential in needs_reauth rather than keep a refresh token the
//     server has already retired.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// refreshThreshold is how long before its expiry an access token is
// refreshed: a minute, or a tenth of its lifetime when that is longer.
func refreshThreshold(sec oauthSecret) time.Duration {
	t := sec.ExpiresAt.Sub(sec.IssuedAt) / 10
	if t < time.Minute {
		t = time.Minute
	}
	return t
}

// fresh reports whether sec's access token is far enough from its expiry to
// send without refreshing.
func (m *OAuthManager) fresh(sec oauthSecret) bool {
	return sec.AccessToken != "" && m.now().Before(sec.ExpiresAt.Add(-refreshThreshold(sec)))
}

// unexpired reports whether sec's access token may still be sent at all.
func (m *OAuthManager) unexpired(sec oauthSecret) bool {
	return sec.AccessToken != "" && m.now().Before(sec.ExpiresAt)
}

// ErrOAuthUnavailable: the authorization server could not refresh the
// access token just now, and the one held has expired. Transient: the same
// call may succeed later, and the grant is kept.
var ErrOAuthUnavailable = errors.New("credential: the OAuth authorization server is unavailable and the access token has expired; try again later")

// Substitute replaces every $oauth:<name> token in s with a fresh access
// token of the OAuth credential the name resolves to for the run identity
// (scope precedence agent > user > tenant, as $cred:), for a request to
// targetURL. It has refSubstituter's shape: register is called with each
// token handed out (for the redactor); a name no visible scope holds is left
// as written and returned in unresolved (the caller drops the header); any
// other failure aborts and is returned — never a partly substituted string.
// ctx must come from netguard.WithPinnable and be the request's context: an
// OAuth credential is always restricted to URLs (its resource by default),
// so it pins the request.
func (m *OAuthManager) Substitute(ctx context.Context, tenantID, agentName, userID, s, targetURL string, register func(string)) (string, []string, error) {
	if !oauthTokenRe.MatchString(s) {
		return s, nil, nil
	}
	var (
		firstErr   error
		unresolved []string
	)
	out := oauthTokenRe.ReplaceAllStringFunc(s, func(ref string) string {
		if firstErr != nil {
			return ref
		}
		name := oauthTokenRe.FindStringSubmatch(ref)[1]
		attempt := oauthAttemptFrom(ctx)
		var (
			tok   string
			found bool
			err   error
		)
		if rejected, retry := attempt.rejected(name); retry {
			tok, found, err = m.ForceRefresh(ctx, tenantID, agentName, userID, name, targetURL, rejected)
		} else {
			tok, found, err = m.token(ctx, tenantID, agentName, userID, name, targetURL)
		}
		if err != nil {
			firstErr = err
			return ref
		}
		if !found {
			unresolved = append(unresolved, name)
			return ref
		}
		if register != nil {
			register(tok)
		}
		if attempt != nil {
			sent := tok
			attempt.note(name, oauthSent{token: sent, reauth: func(ctx context.Context) error {
				return m.refusedAfterRefresh(ctx, tenantID, agentName, userID, name, targetURL, sent)
			}})
		}
		return tok
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	return out, unresolved, nil
}

// token returns the access token to send for name, refreshing it first when
// it is close to expiry.
func (m *OAuthManager) token(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (string, bool, error) {
	r, found, err := m.engine.findOAuth(ctx, tenantID, agentName, userID, name, targetURL)
	if err != nil || !found {
		return "", found, err
	}
	if err := usableState(r); err != nil {
		return "", true, err
	}
	sec, err := m.engine.openOAuthSecret(r)
	if err != nil {
		return "", true, err
	}
	if m.fresh(sec) {
		return sec.AccessToken, true, nil
	}
	// A refresh failed a moment ago: do not hammer the server; the token
	// held may still be sent.
	if m.backingOff(r.key()) {
		if m.unexpired(sec) {
			return sec.AccessToken, true, nil
		}
		return "", true, fmt.Errorf("%w (%q)", ErrOAuthUnavailable, name)
	}
	tok, err := m.flight(ctx, r, "")
	return tok, true, err
}

// flight joins (or starts) this process's one refresh flight for r and
// waits for its token. rejected is the token a server refused, which the
// flight will not hand back; "" when none was.
func (m *OAuthManager) flight(ctx context.Context, r oauthRow, rejected string) (string, error) {
	id := r.id
	id.Bound = nil
	ch := m.sf.DoChan(r.key(), func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthCallTimeout)
		defer cancel()
		return m.refresh(fctx, id, rejected)
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(string), nil
	}
}

// ForceRefresh returns an access token for name other than rejected, the
// one a resource server just refused, refreshing the credential even though
// that token has not expired. A burst of refusals makes one refresh: they
// share this process's flight and the cross-replica lease, and a caller that
// comes after it finds a newer token stored and sends that. found is false
// when no visible scope holds the name.
func (m *OAuthManager) ForceRefresh(ctx context.Context, tenantID, agentName, userID, name, targetURL, rejected string) (string, bool, error) {
	r, found, err := m.engine.findOAuth(ctx, tenantID, agentName, userID, name, targetURL)
	if err != nil || !found {
		return "", found, err
	}
	if err := usableState(r); err != nil {
		return "", true, err
	}
	sec, err := m.engine.openOAuthSecret(r)
	if err != nil {
		return "", true, err
	}
	if !sameToken(sec.AccessToken, rejected) && m.unexpired(sec) {
		return sec.AccessToken, true, nil
	}
	// A refresh failed a moment ago: the refused token is no use, and the
	// server is not asked again yet.
	if m.backingOff(r.key()) {
		return "", true, fmt.Errorf("%w (%q)", ErrOAuthUnavailable, name)
	}
	// Twice at most: a flight already under way when this caller joined may
	// have been started for a token it found still fresh — the refused one.
	for range 2 {
		tok, err := m.flight(ctx, r, rejected)
		if err != nil {
			return "", true, err
		}
		if !sameToken(tok, rejected) {
			return tok, true, nil
		}
	}
	return "", true, fmt.Errorf("%w (%q)", ErrOAuthUnavailable, name)
}

// refusedAfterRefresh is a resource server refusing sent, a token handed out
// for a retry after a refusal: the grant is no good for that server. The
// credential moves to needs_reauth, unless the token stored is no longer
// sent (someone refreshed since), and the typed error is returned either
// way.
func (m *OAuthManager) refusedAfterRefresh(ctx context.Context, tenantID, agentName, userID, name, targetURL, sent string) error {
	const reason = "the server refused a refreshed access token"
	// The same lookup that handed the token out. It pins its ctx for the
	// request about to carry the token; none is sent from here, so the pin
	// is taken on a ctx of its own.
	r, found, err := m.engine.findOAuth(netguard.WithPinnable(ctx), tenantID, agentName, userID, name, targetURL)
	if err != nil || !found {
		return &OAuthReauthError{Name: name, Scope: "unknown", Reason: reason}
	}
	sec, err := m.engine.openOAuthSecret(r)
	if err != nil || !sameToken(sec.AccessToken, sent) {
		return &OAuthReauthError{Name: r.row.Name, Scope: r.row.Scope, Reason: reason}
	}
	return m.reauth(ctx, r, sec, reason)
}

// usableState refuses a credential whose state holds no grant to send.
func usableState(r oauthRow) error {
	switch r.def.Status.State {
	case OAuthConnected:
		return nil
	case OAuthPendingConnect:
		return fmt.Errorf("%w (%q in %s scope)", ErrOAuthNotConnected, r.row.Name, r.row.Scope)
	default:
		reason := r.def.Status.LastError
		if reason == "" {
			reason = r.def.Status.State
		}
		return &OAuthReauthError{Name: r.row.Name, Scope: r.row.Scope, Reason: reason}
	}
}

// refresh is one flight: it refreshes the credential stored exactly under
// id unless someone else already has, and returns the access token to send.
// A token equal to rejected is never handed back as it is.
func (m *OAuthManager) refresh(ctx context.Context, id Identity, rejected string) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		r, err := m.engine.getOAuth(ctx, id)
		if err != nil {
			return "", err
		}
		if err := usableState(r); err != nil {
			return "", err
		}
		sec, err := m.engine.openOAuthSecret(r)
		if err != nil {
			return "", err
		}
		usable := !sameToken(sec.AccessToken, rejected)
		if usable && m.fresh(sec) {
			return sec.AccessToken, nil
		}
		if l := r.def.Lease; l != nil && m.now().Before(l.Until) && l.Holder != m.holder {
			// Another replica is at it: send what is still valid, else wait
			// for its write or for its lease to lapse.
			if usable && m.unexpired(sec) {
				return sec.AccessToken, nil
			}
			if err := m.wait(ctx, m.leasePoll); err != nil {
				return "", err
			}
			continue
		}
		// A live lease of this replica's own name is stale: this flight is
		// the only one here, so it was left by an earlier run of this
		// replica.
		def := r.def
		def.Lease = &oauthLease{Holder: m.holder, Purpose: leaseRefresh, Until: m.now().Add(m.leaseTTL)}
		leased, ok, err := m.engine.casOAuth(ctx, r, def, nil)
		if err != nil {
			return "", err
		}
		if !ok {
			continue // someone wrote first: look again
		}
		return m.refreshUnderLease(ctx, leased, sec, rejected)
	}
}

// refreshUnderLease spends sec's refresh token (read before the lease was
// taken, from the definition the lease was written over) and stores the
// rotated grant.
func (m *OAuthManager) refreshUnderLease(ctx context.Context, r oauthRow, sec oauthSecret, rejected string) (string, error) {
	tok, err := m.client.tokenRefresh(ctx, r.def.Config, sec)
	switch {
	case err == nil:
	case isTransient(err):
		// Nothing was granted: release the lease, back off, and send the
		// token held while it is valid.
		m.noteFailure(r.key())
		def := r.def
		def.Lease = nil
		def.Status.LastError = reasonOf(err)
		_, _, _ = m.engine.casOAuth(ctx, r, def, nil)
		if !sameToken(sec.AccessToken, rejected) && m.unexpired(sec) {
			return sec.AccessToken, nil
		}
		return "", fmt.Errorf("%w (%q)", ErrOAuthUnavailable, r.row.Name)
	case isOAuthCode(err, "invalid_client"):
		// The registration is gone at the server: drop it with the grant,
		// so the next connect registers again.
		sec.ClientID, sec.ClientSecret, sec.RegistrationIssuer = "", "", ""
		return "", m.reauth(ctx, r, sec, "invalid_client")
	default:
		// invalid_grant (the refresh token was revoked, or spent elsewhere)
		// and anything else: the grant cannot be trusted any more.
		return "", m.reauth(ctx, r, sec, reasonOf(err))
	}
	now := m.now()
	next := sec
	next.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken
	}
	next.IssuedAt, next.ExpiresAt = now, accessExpiry(now, tok)
	def := r.def
	def.Lease = nil
	def.Status.State = OAuthConnected
	def.Status.LastRefreshAt = &now
	def.Status.AccessExpiresAt = &next.ExpiresAt
	def.Status.LastError = ""
	// The rotated refresh token is stored before the access token is handed
	// out: the server has already retired the old one.
	if _, ok, err := m.engine.casOAuth(ctx, r, def, &next); err != nil || !ok {
		return "", m.reauth(ctx, r, sec, "the refreshed grant could not be stored")
	}
	m.clearFailures(r.key())
	return next.AccessToken, nil
}

// reauth moves r to needs_reauth (best effort: it loses to any newer write)
// keeping sec's registration, records it, and returns the typed error.
func (m *OAuthManager) reauth(ctx context.Context, r oauthRow, sec oauthSecret, reason string) error {
	sec.clearGrant()
	def := r.def
	def.Lease = nil
	def.Status = OAuthStatus{State: OAuthNeedsReauth, LastError: reason}
	if _, ok, err := m.engine.casOAuth(context.WithoutCancel(ctx), r, def, &sec); err == nil && ok {
		m.record("oauth_needs_reauth", r, sec.Endpoints.Issuer, "failed", reason)
	}
	return &OAuthReauthError{Name: r.row.Name, Scope: r.row.Scope, Reason: reason}
}

// backingOff reports whether a refresh of key failed recently enough that
// another is not tried yet.
func (m *OAuthManager) backingOff(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.backoff[key]
	return ok && m.now().Before(b.until)
}

// noteFailure backs key off: 2^n seconds after the nth failure in a row, at
// most a minute.
func (m *OAuthManager) noteFailure(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.backoff[key]
	b.failures++
	d := time.Minute
	if b.failures < 6 {
		d = time.Duration(1<<b.failures) * time.Second
	}
	b.until = m.now().Add(d)
	m.backoff[key] = b
}

func (m *OAuthManager) clearFailures(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.backoff, key)
}
