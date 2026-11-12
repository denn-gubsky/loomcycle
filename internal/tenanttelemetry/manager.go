package tenanttelemetry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// InvalidateTopic is the backplane channel a policy write is announced on, so
// every replica re-reads that tenant's row.
const InvalidateTopic = "loomcycle.tenant_telemetry_changed"

// RefreshInterval is how often each replica re-reads every policy and the
// header credentials they name. The credential store announces no change, so
// this is how a rotated, expired or deleted credential reaches a running
// pipeline, and how a missed invalidation is made up.
const RefreshInterval = time.Minute

// Store is what the manager reads and writes: the policies, and the
// credential rows a policy names (their change stamps, never their values).
type Store interface {
	store.TenantTelemetryPolicyStore
	CredentialDefGet(ctx context.Context, tenantID, scope, scopeID, name string) (credentialdef.Row, error)
}

// Credentials resolves a header credential for the URL it will be sent to.
// *credential.Engine implements it.
type Credentials interface {
	ResolveFor(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (credential.Resolved, bool, error)
}

// Deps are what a Manager is built from.
type Deps struct {
	Config Config
	Store  Store
	// Credentials may be nil (no secret key configured): a policy then may
	// name no header credential.
	Credentials Credentials
	Router      *lcotel.TenantRouter
	// Dial reaches a tenant's collector; the operator's address guard lives
	// in it (netguard.GuardedDialContext).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RegisterSecret is told every header value resolved, so a log line that
	// ever quotes one is masked. May be nil.
	RegisterSecret func(tenant, value string)
	ServiceVersion string
	Now            func() time.Time
	// TLSConfig is the client TLS configuration for https collectors; nil for
	// the system roots (production). Tests trust their own collector here.
	TLSConfig *tls.Config
}

// Manager keeps one replica's tenant pipelines in step with the stored
// policies, and checks, stores and tests a policy.
type Manager struct {
	cfg            Config
	st             Store
	creds          Credentials
	router         *lcotel.TenantRouter
	dial           func(ctx context.Context, network, addr string) (net.Conn, error)
	registerSecret func(tenant, value string)
	version        string
	now            func() time.Time
	tlsConfig      *tls.Config

	bpMu sync.Mutex
	bp   coord.Backplane

	mu      sync.Mutex
	applied map[string]applied
	// locks serialise each tenant's read-then-apply (reload). One per tenant
	// ever seen here: bounded by the tenants with a policy.
	locks map[string]*sync.Mutex
	// lastTest is when each tenant last ran the test here.
	lastTest map[string]time.Time
	// contentMarked is when this replica last recorded, in the store, that a
	// tenant's content is being captured (ContentCaptureFor).
	contentMarked map[string]time.Time
	// contentMarking names the tenants whose record a recording is moving in
	// the background (ContentCaptureActive): one write per tenant at a time.
	contentMarking map[string]bool
}

// applied is what this replica last made of one tenant's policy.
type applied struct {
	// stamp identifies the row and its credential row as they were read:
	// when neither changed, re-reading them changes nothing.
	stamp string
	// expiresAt is when the header credential stops resolving (zero: never).
	expiresAt time.Time
	// err is why the policy is not being exported, when it is not; reported
	// in the status, scrubbed of any value.
	err string
	// transient: err is a fault (a store or decryption failure), not a
	// refusal, so the next refresh tries again though nothing changed.
	transient bool
	// contentMaxChars is the cut of each captured field while the tenant's
	// spans are exported with content capture on; 0 when they are not.
	contentMaxChars int
}

// New builds a Manager. It sets no destination until Refresh (or Reload).
func New(d Deps) (*Manager, error) {
	if d.Store == nil || d.Router == nil || d.Dial == nil {
		return nil, errors.New("tenant telemetry: a store, a router and a dialer are required")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		cfg: d.Config, st: d.Store, creds: d.Credentials, router: d.Router, dial: d.Dial,
		registerSecret: d.RegisterSecret, version: d.ServiceVersion, now: now, tlsConfig: d.TLSConfig,
		applied: map[string]applied{}, locks: map[string]*sync.Mutex{}, lastTest: map[string]time.Time{},
		contentMarked: map[string]time.Time{}, contentMarking: map[string]bool{},
	}, nil
}

// SetBackplane makes policy writes announced to the other replicas.
func (m *Manager) SetBackplane(bp coord.Backplane) {
	m.bpMu.Lock()
	m.bp = bp
	m.bpMu.Unlock()
}

// Get returns tenant's stored policy (nil when none).
func (m *Manager) Get(ctx context.Context, tenant string) (*store.TenantTelemetryPolicyRow, error) {
	row, err := m.st.TenantTelemetryPolicyGet(ctx, tenant)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// Status is tenant's export on this replica.
type Status struct {
	lcotel.TenantExportStatus
	// ApplyError is why this replica is not exporting the stored policy (a
	// header credential that expired or was removed), "" when it is or when
	// there is no policy.
	ApplyError string
}

// Status reports tenant's export on this replica.
func (m *Manager) Status(tenant string) Status {
	m.mu.Lock()
	a := m.applied[tenant]
	m.mu.Unlock()
	return Status{TenantExportStatus: m.router.Status(tenant), ApplyError: a.err}
}

// Check checks in for tenant against the operator's rules and resolves its
// header credential for the endpoint, and returns the row Store would write.
// A policy that could not export is refused here, before anything is stored.
func (m *Manager) Check(ctx context.Context, tenant string, in PolicyInput) (store.TenantTelemetryPolicyRow, error) {
	row, err := m.normalize(tenant, in, false)
	if err != nil {
		return store.TenantTelemetryPolicyRow{}, err
	}
	if _, err := m.headersFor(ctx, row); err != nil {
		return store.TenantTelemetryPolicyRow{}, err
	}
	return row, nil
}

// Store writes a row Check returned, applies it here and announces it to the
// other replicas.
func (m *Manager) Store(ctx context.Context, row store.TenantTelemetryPolicyRow, by string) (store.TenantTelemetryPolicyRow, error) {
	row.UpdatedAt, row.UpdatedBy = m.now().UTC(), by
	if err := m.st.TenantTelemetryPolicyPut(ctx, row); err != nil {
		return store.TenantTelemetryPolicyRow{}, err
	}
	m.Applied(ctx, row.TenantID)
	return row, nil
}

// Delete removes tenant's policy, stops its export here and announces it.
func (m *Manager) Delete(ctx context.Context, tenant string) (bool, error) {
	had, err := m.st.TenantTelemetryPolicyDelete(ctx, tenant)
	if err != nil {
		return false, err
	}
	m.Applied(ctx, tenant)
	return had, nil
}

// Applied makes this replica's export of tenant match its stored policy and
// tells the other replicas to do the same: after a write here, or after a
// snapshot restore wrote the row.
func (m *Manager) Applied(ctx context.Context, tenant string) {
	m.Reload(ctx, tenant)
	m.publish(ctx, tenant)
}

// Test sends one synthetic span (with traces) and one synthetic metric (with
// metrics) to tenant's stored destination, through the same credential
// resolution, address guard and exporters its exports take, and returns nil
// or why it failed (scrubbed of any header value).
func (m *Manager) Test(ctx context.Context, tenant string) error {
	row, err := m.Get(ctx, tenant)
	if err != nil {
		return err
	}
	if row == nil {
		return invalid("this tenant has no telemetry policy to test")
	}
	if !m.mayTest(tenant) {
		return fmt.Errorf("%w: one test per %s", ErrRateLimited, TestInterval)
	}
	valid, err := m.ValidStored(*row)
	if err != nil {
		return err
	}
	d, err := m.destination(ctx, valid)
	if err != nil {
		return err
	}
	return m.router.Test(ctx, tenant, d, lcotel.DefaultTenantExportTimeout)
}

// ContentMarkInterval is how often, at most, each replica records in the
// store that a tenant's content is being captured. The erasure report's
// window is that record, so its last time lags by up to this much.
const ContentMarkInterval = time.Minute

// contentMarkTimeout bounds the record's write on the run's start.
const contentMarkTimeout = 5 * time.Second

// ContentCaptureFor reports whether a run of tenant starting now captures
// content, and the length each captured field is cut to. It does while the
// tenant's policy is exported here with content capture on (and the operator
// allows it: ValidStored). The run's own checks — its agent's opt-out, a
// no-trace run — are the caller's.
//
// A yes is first recorded in the store (at most every ContentMarkInterval),
// so the erasure report can say when content left for the tenant's backend.
// A record that cannot be written is a no: content is never exported unless
// the export is on record.
func (m *Manager) ContentCaptureFor(ctx context.Context, tenant string) (int, bool) {
	if tenant == "" {
		return 0, false
	}
	m.mu.Lock()
	a := m.applied[tenant]
	last := m.contentMarked[tenant]
	m.mu.Unlock()
	if a.contentMaxChars <= 0 || a.err != "" {
		return 0, false
	}
	now := m.now()
	if !last.IsZero() && now.Sub(last) < ContentMarkInterval {
		return a.contentMaxChars, true
	}
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contentMarkTimeout)
	defer cancel()
	if err := m.st.TenantTelemetryContentExportMark(mctx, tenant, now.UTC()); err != nil {
		slog.Warn("tenant telemetry: could not record a content capture; this run captures no content", "tenant", tenant, "err", err)
		return 0, false
	}
	m.mu.Lock()
	m.contentMarked[tenant] = now
	m.mu.Unlock()
	return a.contentMaxChars, true
}

// ContentCaptureActive reports whether tenant's destination captures content
// now: asked on each recording of a run that started capturing, so a tenant
// that turns capture off (or replaces its destination with one that does not
// capture) stops it mid-run. A yes also keeps the capture record's last time
// current through a long run: at most every ContentMarkInterval, in the
// background, it is moved, and the recording never waits on the store. The
// first time is on record already: the run's start (ContentCaptureFor) wrote
// it before any content was recorded.
func (m *Manager) ContentCaptureActive(tenant string) bool {
	if !m.router.CapturesContent(tenant) {
		return false
	}
	now := m.now()
	m.mu.Lock()
	due := !m.contentMarking[tenant] && now.Sub(m.contentMarked[tenant]) >= ContentMarkInterval
	if due {
		m.contentMarking[tenant] = true
	}
	m.mu.Unlock()
	if due {
		go m.markContent(tenant, now)
	}
	return true
}

// markContent moves tenant's capture record to now, in the background.
func (m *Manager) markContent(tenant string, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), contentMarkTimeout)
	defer cancel()
	err := m.st.TenantTelemetryContentExportMark(ctx, tenant, now.UTC())
	m.mu.Lock()
	delete(m.contentMarking, tenant)
	if err == nil && now.After(m.contentMarked[tenant]) {
		m.contentMarked[tenant] = now
	}
	m.mu.Unlock()
	if err != nil {
		slog.Warn("tenant telemetry: could not move the content capture record", "tenant", tenant, "err", err)
	}
}

// TestInterval is how often each tenant may run the test on one replica: a
// test is an outbound request a tenant triggers at will.
const TestInterval = 10 * time.Second

// ErrRateLimited is a test asked for within TestInterval of the last one.
var ErrRateLimited = errors.New("tenant telemetry: a test was run moments ago")

// mayTest reports whether tenant may run a test now, and if so records it.
func (m *Manager) mayTest(tenant string) bool {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.lastTest[tenant]; ok && now.Sub(last) < TestInterval {
		return false
	}
	m.lastTest[tenant] = now
	return true
}

// ValidStored holds a stored row to the operator's rules as they are NOW —
// the ones a set is held to — and returns it as a set would store it. A row
// restored from another deployment, or written before the operator narrowed
// its rules, can fail them: it is then refused (ErrInvalid, "policy no longer
// valid: <reason>") wherever it would be used — a pipeline build, a test, a
// snapshot restore. The header credential's binding to the endpoint is
// checked where the credential is resolved, each time.
func (m *Manager) ValidStored(row store.TenantTelemetryPolicyRow) (store.TenantTelemetryPolicyRow, error) {
	ratio, maxChars := row.SamplerRatio, row.ContentMaxChars
	valid, err := m.normalize(row.TenantID, PolicyInput{Endpoint: row.Endpoint, Protocol: row.Protocol,
		HeadersCredential: row.HeadersCredential, Signals: row.Signals, SamplerRatio: &ratio,
		ContentCapture: row.ContentCapture, ContentMaxChars: &maxChars}, true)
	if err != nil {
		return store.TenantTelemetryPolicyRow{}, fmt.Errorf("%w: policy no longer valid: %s", ErrInvalid, PublicError(row.TenantID, err))
	}
	valid.UpdatedAt, valid.UpdatedBy = row.UpdatedAt, row.UpdatedBy
	return valid, nil
}

// destination resolves row into what the router exports with.
func (m *Manager) destination(ctx context.Context, row store.TenantTelemetryPolicyRow) (lcotel.TenantDestination, error) {
	headers, err := m.headersFor(ctx, row)
	if err != nil {
		return lcotel.TenantDestination{}, err
	}
	if m.registerSecret != nil {
		for _, v := range headers {
			m.registerSecret(row.TenantID, v)
		}
	}
	return lcotel.TenantDestination{
		Endpoint: row.Endpoint, Protocol: lcotel.Protocol(row.Protocol), Headers: headers, Dial: m.dial,
		TLSConfig: m.tlsConfig, Signals: append([]string(nil), row.Signals...),
		Ratio: row.SamplerRatio, ContentCapture: row.ContentCapture == ContentCaptureOn,
	}, nil
}

// Reload re-reads tenant's policy and makes this replica's pipeline match it:
// set, replaced, or removed.
func (m *Manager) Reload(ctx context.Context, tenant string) {
	m.reload(ctx, tenant, true)
}

// reload is Reload, holding tenant's lock from the read of its row to the
// router change: a refresh that read the policies before a write or a delete
// landed would otherwise apply the row it read after the write's own reload,
// pointing spans back at a collector the tenant replaced or removed. Unless
// force, a row and credential unchanged since the last apply are left alone.
func (m *Manager) reload(ctx context.Context, tenant string, force bool) {
	l := m.tenantLock(tenant)
	l.Lock()
	defer l.Unlock()
	row, err := m.Get(ctx, tenant)
	if err != nil {
		// Keep what is running: a store that cannot be read is no reason to
		// stop, and the next refresh tries again.
		slog.Warn("tenant telemetry: could not read the policy; keeping the current export", "tenant", tenant, "err", err)
		return
	}
	if row == nil {
		m.remove(tenant)
		return
	}
	m.apply(ctx, *row, force)
}

func (m *Manager) tenantLock(tenant string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[tenant]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[tenant] = l
	}
	return l
}

// Refresh re-reads every policy, applies those that changed (or whose header
// credential changed or expired, or which a fault left off), and removes the
// pipelines of policies that are gone. It runs at boot and every
// RefreshInterval. The list only names the tenants: each is re-read under its
// own lock (reload), so a write that lands meanwhile is never undone.
func (m *Manager) Refresh(ctx context.Context) error {
	rows, err := m.st.TenantTelemetryPolicyList(ctx)
	if err != nil {
		return fmt.Errorf("tenant telemetry: list policies: %w", err)
	}
	seen := make(map[string]bool, len(rows))
	tenants := make([]string, 0, len(rows))
	for _, row := range rows {
		seen[row.TenantID] = true
		tenants = append(tenants, row.TenantID)
	}
	m.mu.Lock()
	for t := range m.applied {
		if !seen[t] {
			tenants = append(tenants, t)
			seen[t] = true
		}
	}
	m.mu.Unlock()
	tenants = append(tenants, missing(m.router.Tenants(), seen)...)
	for _, t := range tenants {
		m.reload(ctx, t, false)
	}
	return nil
}

func missing(tenants []string, seen map[string]bool) []string {
	var out []string
	for _, t := range tenants {
		if !seen[t] {
			out = append(out, t)
		}
	}
	return out
}

func (m *Manager) remove(tenant string) {
	m.router.RemoveDestination(tenant)
	m.mu.Lock()
	delete(m.applied, tenant)
	m.mu.Unlock()
}

// apply makes the router's destination for row's tenant match row. Unless
// force, a row and credential unchanged since the last apply are left alone:
// a refresh decrypts only what changed.
func (m *Manager) apply(ctx context.Context, row store.TenantTelemetryPolicyRow, force bool) {
	stamp, expires, err := m.stampOf(ctx, row)
	if err != nil {
		slog.Warn("tenant telemetry: could not read the header credential; keeping the current export", "tenant", row.TenantID, "err", err)
		return
	}
	m.mu.Lock()
	prev, had := m.applied[row.TenantID]
	m.mu.Unlock()
	now := m.now()
	expired := !expires.IsZero() && !now.Before(expires)
	if !force && had && !prev.transient && prev.stamp == stamp && (expires.IsZero() || !expired || prev.err != "") {
		return
	}
	next := applied{stamp: stamp, expiresAt: expires}
	// The operator's rules are asked of every stored row, not only of a
	// write: a row restored from another deployment, or one written before
	// the operator narrowed its host list or ratio cap, exports only if the
	// rules allow it now.
	valid, ruleErr := m.ValidStored(row)
	switch {
	case !containsString(row.Signals, SignalTraces) && !containsString(row.Signals, SignalMetrics):
		m.router.RemoveDestination(row.TenantID)
	case ruleErr != nil:
		m.router.RemoveDestination(row.TenantID)
		next.err = publicError(row.TenantID, ruleErr, genericApplyError)
		slog.Warn("tenant telemetry: the stored policy breaks the operator's rules; not exporting it", "tenant", row.TenantID, "err", next.err)
	default:
		d, err := m.destination(ctx, valid)
		if err != nil {
			m.router.RemoveDestination(row.TenantID)
			next.err = publicError(row.TenantID, err, genericApplyError)
			next.transient = !errors.Is(err, ErrInvalid)
			slog.Warn("tenant telemetry: the policy cannot be exported", "tenant", row.TenantID, "err", next.err)
			break
		}
		m.router.SetDestination(row.TenantID, d)
		// ValidStored turned capture off if the operator no longer allows it.
		if valid.ContentCapture == ContentCaptureOn {
			next.contentMaxChars = valid.ContentMaxChars
		}
	}
	m.mu.Lock()
	m.applied[row.TenantID] = next
	m.mu.Unlock()
}

// stampOf identifies row, and the credential row it names, as they are now.
// Only change stamps are read, never a credential's value.
func (m *Manager) stampOf(ctx context.Context, row store.TenantTelemetryPolicyRow) (string, time.Time, error) {
	h := sha256.New()
	b, _ := json.Marshal(row)
	_, _ = h.Write(b)
	var expires time.Time
	if row.HeadersCredential != "" {
		cr, err := m.st.CredentialDefGet(ctx, row.TenantID, "tenant", "", row.HeadersCredential)
		var nf *store.ErrNotFound
		switch {
		case errors.As(err, &nf):
			_, _ = h.Write([]byte("missing"))
		case err != nil:
			return "", time.Time{}, err
		default:
			_, _ = fmt.Fprintf(h, "%d", cr.UpdatedAt.UnixNano())
			if cr.ExpiresAt != nil {
				expires = *cr.ExpiresAt
				_, _ = fmt.Fprintf(h, "/%d", expires.UnixNano())
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), expires, nil
}

// genericTestError is what a tenant is shown for a failure whose text is not
// known to be its to read (a store fault, a credential that would not open).
const genericTestError = "the telemetry export could not be tested; the operator's logs have details"

// genericApplyError is the same for a stored policy that is not being
// exported.
const genericApplyError = "the telemetry export could not be set up; the operator's logs have details"

// PublicError is err as tenant may read it: a policy refusal in its own
// words, an exporter's failure as the exporter (scrubbed of header values,
// URL credentials and query values) put it, and anything else as one generic
// line, the full error logged for the operator.
func PublicError(tenant string, err error) string {
	return publicError(tenant, err, genericTestError)
}

func publicError(tenant string, err error, generic string) string {
	var exp *lcotel.TenantExportError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalid):
		return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
	case errors.As(err, &exp):
		return exp.Error()
	}
	// Never a header value here: a resolved value is never put in an error.
	slog.Warn("tenant telemetry: failure hidden from the tenant", "tenant", tenant, "err", err)
	return generic
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// publish tells the other replicas tenant's policy changed. Best-effort: a
// missed message is made up by the next refresh.
func (m *Manager) publish(ctx context.Context, tenant string) {
	m.bpMu.Lock()
	bp := m.bp
	m.bpMu.Unlock()
	if bp == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"tenant": tenant})
	if err := bp.Publish(ctx, InvalidateTopic, payload); err != nil {
		slog.Warn("tenant telemetry: invalidation publish failed", "tenant", tenant, "err", err)
	}
}

// Subscribe reloads a tenant's policy when a peer replica reports a write.
// Returns once subscribed; the listener stops with ctx.
func (m *Manager) Subscribe(ctx context.Context, bp coord.Backplane) error {
	ch, err := bp.Subscribe(ctx, InvalidateTopic)
	if err != nil {
		return err
	}
	safego.Go(ctx, "tenanttelemetry.backplane_listener", func() {
		for ev := range ch {
			var msg struct {
				Tenant string `json:"tenant"`
			}
			if json.Unmarshal(ev.Payload, &msg) != nil || msg.Tenant == "" {
				continue
			}
			// Recovered per event: a listener that stopped would leave this
			// replica exporting to a destination a tenant changed or removed
			// until the next refresh.
			safego.Run(ctx, "tenanttelemetry.backplane_listener", func() { m.Reload(ctx, msg.Tenant) })
		}
	})
	return nil
}

// Run refreshes every RefreshInterval until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safego.Run(ctx, "tenanttelemetry.refresh", func() {
				if err := m.Refresh(ctx); err != nil {
					slog.Warn("tenant telemetry: refresh failed", "err", err)
				}
			})
		}
	}
}
