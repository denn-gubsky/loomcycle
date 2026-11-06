package credential

// The OAuth manager (RFC GB): connects OAuth credentials through the device
// authorization grant (RFC 8628), keeps their access tokens fresh, and
// disconnects them. Every write goes through the engine's conditional write,
// so two replicas — or a poller and a status check on one — never both act
// on the same read.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// Lease timings. Constants, not configuration: they bound how long a replica
// that died mid-refresh holds the others up, and how often a waiter looks.
const (
	oauthLeaseTTL  = 30 * time.Second
	oauthLeasePoll = 250 * time.Millisecond
	// oauthCallTimeout bounds one flight (a refresh, a revoke): a cancelled
	// caller does not end it, this does.
	oauthCallTimeout = 30 * time.Second
	// oauthDefaultInterval is the device poll interval when the server
	// names none (RFC 8628 §3.2).
	oauthDefaultInterval = 5
	// oauthSlowDownStep is what a slow_down adds to the interval (RFC 8628
	// §3.5).
	oauthSlowDownStep = 5
)

// OAuthDeviceStart is what the owner is shown to connect: where to go and
// the code to enter. Never the device code.
type OAuthDeviceStart struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int64
	Interval                int
}

// OAuthManager connects, refreshes and disconnects OAuth credentials. Safe
// for concurrent use.
type OAuthManager struct {
	engine *Engine
	client *oauthClient
	holder string
	now    func() time.Time
	// wait sleeps d or until ctx ends; replaced in tests.
	wait func(ctx context.Context, d time.Duration) error
	// leaseTTL and leasePoll are the constants above; tests shorten them.
	leaseTTL, leasePoll time.Duration

	sf singleflight.Group

	mu      sync.Mutex
	pollers map[string]*oauthPoller
	backoff map[string]oauthBackoff
	audit   audit.Sink
	bg      context.Context
}

type oauthPoller struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type oauthBackoff struct {
	failures int
	until    time.Time
}

// NewOAuthHTTPClient is the client production uses to reach authorization
// servers: guarded against private and metadata addresses (unless the
// operator allowlisted the host), and following no redirect at all, so a
// token request is never re-sent to another host.
func NewOAuthHTTPClient(privateHostAllowlist []string) *http.Client {
	c := netguard.NewGuardedClient(oauthCallTimeout, privateHostAllowlist)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// NewOAuthManager builds a manager over engine. httpClient must follow no
// redirect (NewOAuthHTTPClient). holder names this replica in a lease.
// auditSink may be nil.
func NewOAuthManager(engine *Engine, httpClient *http.Client, holder string, auditSink audit.Sink) *OAuthManager {
	return &OAuthManager{
		engine: engine, client: &oauthClient{http: httpClient}, holder: holder,
		now: time.Now, wait: sleepCtx, leaseTTL: oauthLeaseTTL, leasePoll: oauthLeasePoll,
		pollers: map[string]*oauthPoller{}, backoff: map[string]oauthBackoff{},
		audit: auditSink, bg: context.Background(),
	}
}

// SetAudit sets where connect and refresh outcomes are recorded.
func (m *OAuthManager) SetAudit(s audit.Sink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = s
}

// SetBackground sets the context device pollers run under: ended at
// shutdown.
func (m *OAuthManager) SetBackground(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bg = ctx
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// record writes an audit event for r, never anything secret.
func (m *OAuthManager) record(action string, r oauthRow, issuer, outcome, reason string) {
	m.mu.Lock()
	sink := m.audit
	m.mu.Unlock()
	if sink == nil {
		return
	}
	ev := audit.Event{Action: action, TargetTenant: r.row.TenantID, TargetName: r.row.Name,
		OAuthScope: r.row.Scope, OAuthIssuer: issuer, OAuthOutcome: outcome, OAuthReason: reason}
	if r.row.Scope == "user" {
		ev.TargetSubject = r.row.ScopeID
	}
	if err := sink.Record(ev); err != nil {
		slog.Warn("oauth: audit record failed", "action", action, "err", err)
	}
}

// reasonOf is the audit- and status-safe reason for err: an authorization
// server's error code, or a fixed phrase. Never a description, which the
// server wrote.
func reasonOf(err error) string {
	var oe *oauthError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &oe):
		return oe.Code
	case isTransient(err):
		return "authorization server unavailable"
	default:
		return "connect failed"
	}
}

// ConnectStart starts connecting the OAuth credential stored exactly under
// id: discovers the authorization server, registers the runtime as a client
// if it is not yet, and starts a device authorization. A poller on this
// replica then waits for the owner to approve it. A second start
// supersedes the first. Returns what the owner is shown — never the device
// code.
func (m *OAuthManager) ConnectStart(ctx context.Context, id Identity) (OAuthDeviceStart, error) {
	if !m.engine.sealer.Enabled() {
		return OAuthDeviceStart{}, ErrNoKey
	}
	r, err := m.engine.getOAuth(ctx, id)
	if err != nil {
		return OAuthDeviceStart{}, err
	}
	sec, err := m.engine.openOAuthSecret(r)
	if err != nil {
		return OAuthDeviceStart{}, err
	}
	// Mark the connect under way first, so a refresh on another replica
	// stops using the old grant and a status check does not poll.
	def := r.def
	def.Lease = &oauthLease{Holder: m.holder, Purpose: leaseConnect, Until: m.now().Add(m.leaseTTL)}
	r, ok, err := m.engine.casOAuth(ctx, r, def, nil)
	if err != nil {
		return OAuthDeviceStart{}, err
	}
	if !ok {
		return OAuthDeviceStart{}, errors.New("credential: the OAuth credential changed while the connect started; try again")
	}
	dev, sec, err := m.authorizeDevice(ctx, r, sec)
	if err != nil {
		m.failConnect(ctx, r, err)
		return OAuthDeviceStart{}, fmt.Errorf("connect: %w", err)
	}
	now := m.now()
	sec.clearGrant()
	sec.DeviceCode = dev.DeviceCode
	sec.DeviceExpiresAt = deviceDeadline(now, dev)
	sec.Interval = dev.Interval
	if sec.Interval <= 0 {
		sec.Interval = oauthDefaultInterval
	}
	def = r.def
	def.Status = OAuthStatus{State: OAuthPendingConnect}
	def.Lease = &oauthLease{Holder: m.holder, Purpose: leaseConnect, Until: now.Add(m.leaseTTL)}
	r, ok, err = m.engine.casOAuth(ctx, r, def, &sec)
	if err != nil {
		return OAuthDeviceStart{}, err
	}
	if !ok {
		return OAuthDeviceStart{}, errors.New("credential: the OAuth credential changed while the connect started; try again")
	}
	m.startPoller(r, sec)
	return OAuthDeviceStart{UserCode: dev.UserCode, VerificationURI: dev.VerificationURI,
		VerificationURIComplete: dev.VerificationURIComplete, ExpiresIn: dev.ExpiresIn, Interval: sec.Interval}, nil
}

// authorizeDevice discovers, registers when needed, and starts a device
// authorization. A registration the server no longer knows (invalid_client)
// is dropped and made again, once.
func (m *OAuthManager) authorizeDevice(ctx context.Context, r oauthRow, sec oauthSecret) (deviceResponse, oauthSecret, error) {
	cfg := r.def.Config
	ep, err := m.client.discover(ctx, cfg)
	if err != nil {
		return deviceResponse{}, sec, err
	}
	sec.Endpoints = ep
	for attempt := 0; ; attempt++ {
		if !sec.registered(ep.Issuer) {
			id, secret, err := m.client.register(ctx, ep, cfg)
			if err != nil {
				return deviceResponse{}, sec, err
			}
			sec.ClientID, sec.ClientSecret, sec.RegistrationIssuer = id, secret, ep.Issuer
			if attempt > 0 {
				m.record("oauth_reregistered", r, ep.Issuer, "ok", "invalid_client")
			}
		}
		dev, err := m.client.deviceAuthorize(ctx, cfg, sec)
		if attempt == 0 && isOAuthCode(err, "invalid_client") {
			sec.ClientID, sec.ClientSecret, sec.RegistrationIssuer = "", "", ""
			continue
		}
		return dev, sec, err
	}
}

// failConnect records a connect that could not start, and puts the reason
// on the credential's status (keeping its state), clearing the lease.
func (m *OAuthManager) failConnect(ctx context.Context, r oauthRow, cause error) {
	reason := reasonOf(cause)
	m.record("oauth_connect_failed", r, "", "failed", reason)
	def := r.def
	def.Lease = nil
	def.Status.LastError = reason
	if _, _, err := m.engine.casOAuth(context.WithoutCancel(ctx), r, def, nil); err != nil {
		slog.Warn("oauth: could not record a failed connect", "credential", r.row.Name, "scope", r.row.Scope, "err", err)
	}
}

// startPoller starts polling r's device grant on this replica, cancelling
// any poller already running for the credential.
func (m *OAuthManager) startPoller(r oauthRow, sec oauthSecret) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.pollers[r.key()]; p != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(m.bg)
	p := &oauthPoller{cancel: cancel, done: make(chan struct{})}
	m.pollers[r.key()] = p
	go func() {
		defer close(p.done)
		defer cancel()
		m.poll(ctx, r, sec)
		m.mu.Lock()
		if m.pollers[r.key()] == p {
			delete(m.pollers, r.key())
		}
		m.mu.Unlock()
	}()
}

// stopPoller cancels and waits for the poller of key, if any.
func (m *OAuthManager) stopPoller(key string) {
	m.mu.Lock()
	p := m.pollers[key]
	delete(m.pollers, key)
	m.mu.Unlock()
	if p != nil {
		p.cancel()
		<-p.done
	}
}

// hasPoller reports whether this replica polls key.
func (m *OAuthManager) hasPoller(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pollers[key] != nil
}

// poll waits for the owner to approve the device authorization: one token
// request per interval until it is granted, refused, or the device code
// expires. Each write is conditional on the last one this poller made;
// losing one means someone else changed the credential (a new start, a
// disconnect), and the poller stops quietly.
func (m *OAuthManager) poll(ctx context.Context, r oauthRow, sec oauthSecret) {
	interval := sec.Interval
	for {
		if err := m.wait(ctx, time.Duration(interval)*time.Second); err != nil {
			return
		}
		next, done := m.pollStep(ctx, r, sec, &interval)
		if done {
			return
		}
		// Keep the connect lease live while this poller runs, so a status
		// check elsewhere does not poll beside it.
		if next.def.Lease != nil && next.def.Lease.Until.Sub(m.now()) < m.leaseTTL/2 {
			def := next.def
			def.Lease = &oauthLease{Holder: m.holder, Purpose: leaseConnect, Until: m.now().Add(m.leaseTTL)}
			nr, ok, err := m.engine.casOAuth(ctx, next, def, nil)
			if err != nil || !ok {
				return
			}
			next = nr
		}
		r = next
	}
}

// pollStep makes one token request for r's device code and acts on the
// answer. done is true when polling is over; *interval grows on slow_down.
func (m *OAuthManager) pollStep(ctx context.Context, r oauthRow, sec oauthSecret, interval *int) (oauthRow, bool) {
	if !m.now().Before(sec.DeviceExpiresAt) {
		m.loseGrant(ctx, r, "device code expired; start again", "oauth_connect_failed")
		return r, true
	}
	tok, err := m.client.tokenDevice(ctx, r.def.Config, sec)
	switch {
	case err == nil:
		m.connected(ctx, r, sec, tok)
		return r, true
	case isOAuthCode(err, "authorization_pending"):
		return r, false
	case isOAuthCode(err, "slow_down"):
		*interval += oauthSlowDownStep
		return r, false
	case isTransient(err) || ctx.Err() != nil:
		// Keep polling until the device code expires.
		return r, false
	default:
		m.loseGrant(ctx, r, reasonOf(err), "oauth_connect_failed")
		return r, true
	}
}

// connected stores a granted token as r's grant.
func (m *OAuthManager) connected(ctx context.Context, r oauthRow, sec oauthSecret, tok tokenResponse) {
	now := m.now()
	sec.clearGrant()
	sec.AccessToken, sec.RefreshToken = tok.AccessToken, tok.RefreshToken
	sec.IssuedAt, sec.ExpiresAt = now, accessExpiry(now, tok)
	connectedAs := m.client.userinfo(ctx, sec, tok.Scope)
	def := r.def
	def.Lease = nil
	def.Status = OAuthStatus{State: OAuthConnected, ConnectedAs: connectedAs, ConnectedAt: &now, AccessExpiresAt: &sec.ExpiresAt}
	if _, ok, err := m.engine.casOAuth(context.WithoutCancel(ctx), r, def, &sec); err != nil || !ok {
		// The grant is lost with this write (a new start or a disconnect
		// came first); nothing else holds it.
		slog.Info("oauth: a granted connect was superseded before it was stored", "credential", r.row.Name, "scope", r.row.Scope)
		return
	}
	m.record("oauth_connected", r, sec.Endpoints.Issuer, "ok", "")
}

// accessExpiry is when an access token issued at now expires. A server that
// names no lifetime is taken at a cautious five minutes.
func accessExpiry(now time.Time, tok tokenResponse) time.Time {
	life := time.Duration(tok.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	return now.Add(life)
}

// loseGrant moves r to needs_reauth with reason, dropping its tokens and
// device code but keeping its registration, and records action.
func (m *OAuthManager) loseGrant(ctx context.Context, r oauthRow, reason, action string) bool {
	sec, err := m.engine.openOAuthSecret(r)
	if err != nil {
		sec = oauthSecret{}
	}
	sec.clearGrant()
	def := r.def
	def.Lease = nil
	def.Status = OAuthStatus{State: OAuthNeedsReauth, LastError: reason}
	_, ok, err := m.engine.casOAuth(context.WithoutCancel(ctx), r, def, &sec)
	if err != nil || !ok {
		return false
	}
	m.record(action, r, sec.Endpoints.Issuer, "failed", reason)
	return true
}

// ConnectStatus returns the metadata of the OAuth credential stored exactly
// under id. A connect started on a replica that is gone (no poller holds
// its lease) is advanced here by one poll step, under a lease of its own,
// so a restart does not strand it; an expired device code moves the
// credential to needs_reauth.
func (m *OAuthManager) ConnectStatus(ctx context.Context, id Identity) (Meta, error) {
	r, err := m.engine.getOAuth(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	if r.def.Status.State == OAuthPendingConnect && !m.connectLeaseLive(r) {
		m.statusStep(ctx, r)
	}
	return m.engine.Get(ctx, id)
}

// connectLeaseLive reports whether a poller is (or may be) at work on r: a
// lease that has not lapsed, unless it names this replica, which has no
// poller for it (this replica restarted under the same name).
func (m *OAuthManager) connectLeaseLive(r oauthRow) bool {
	l := r.def.Lease
	if l == nil || !m.now().Before(l.Until) {
		return false
	}
	if l.Holder == m.holder {
		return m.hasPoller(r.key())
	}
	return true
}

// statusStep makes one poll step for a stranded connect.
func (m *OAuthManager) statusStep(ctx context.Context, r oauthRow) {
	sec, err := m.engine.openOAuthSecret(r)
	if err != nil {
		return
	}
	if sec.DeviceCode == "" {
		return
	}
	if !m.now().Before(sec.DeviceExpiresAt) {
		m.loseGrant(ctx, r, "device code expired; start again", "oauth_connect_failed")
		return
	}
	interval := sec.Interval
	if interval <= 0 {
		interval = oauthDefaultInterval
	}
	// The lease spaces status checks by the interval, and keeps a second
	// one from exchanging the same device code beside this one.
	def := r.def
	def.Lease = &oauthLease{Holder: m.holder, Purpose: leaseConnect, Until: m.now().Add(time.Duration(interval) * time.Second)}
	r, ok, err := m.engine.casOAuth(ctx, r, def, nil)
	if err != nil || !ok {
		return
	}
	m.pollStep(ctx, r, sec, &interval)
}

// Disconnect revokes the OAuth credential stored exactly under id (best
// effort: the server may be unreachable) and clears its grant, whatever the
// revocation answered. The registration is kept for a later connect.
func (m *OAuthManager) Disconnect(ctx context.Context, id Identity) (Meta, error) {
	r, err := m.engine.getOAuth(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	m.stopPoller(r.key())
	for attempt := 0; attempt < 5; attempt++ {
		sec, oerr := m.engine.openOAuthSecret(r)
		if oerr == nil {
			m.revoke(ctx, r, sec)
		} else {
			sec = oauthSecret{} // unopenable: nothing to keep, nothing to revoke
		}
		sec.clearGrant()
		def := r.def
		def.Lease = nil
		def.Status = OAuthStatus{State: OAuthRevoked}
		_, ok, err := m.engine.casOAuth(context.WithoutCancel(ctx), r, def, &sec)
		if err != nil {
			return Meta{}, err
		}
		if ok {
			return m.engine.Get(ctx, id)
		}
		// Someone wrote between the read and this write (a refresh); read
		// again and revoke what it holds now.
		if r, err = m.engine.getOAuth(ctx, id); err != nil {
			return Meta{}, err
		}
	}
	return Meta{}, errors.New("credential: the OAuth credential kept changing; try the disconnect again")
}

// RevokeBeforeDelete revokes the grant of the OAuth credential stored
// exactly under id, best effort, before it is deleted; any other credential
// is left alone.
func (m *OAuthManager) RevokeBeforeDelete(ctx context.Context, id Identity) {
	r, err := m.engine.getOAuth(ctx, id)
	if err != nil {
		return
	}
	m.stopPoller(r.key())
	if sec, err := m.engine.openOAuthSecret(r); err == nil {
		m.revoke(ctx, r, sec)
	}
}

// revoke revokes sec's refresh token, bounded, logging (never returning) a
// failure.
func (m *OAuthManager) revoke(ctx context.Context, r oauthRow, sec oauthSecret) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthCallTimeout)
	defer cancel()
	if err := m.client.revoke(rctx, r.def.Config, sec); err != nil {
		slog.Warn("oauth: revocation failed; the grant is cleared here anyway", "credential", r.row.Name, "scope", r.row.Scope, "reason", reasonOf(err))
	}
}
