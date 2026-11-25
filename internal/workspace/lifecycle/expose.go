package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ExposeLimits are workspace.expose, resolved (RFC GW §5.3, P9 plan §3.3).
type ExposeLimits struct {
	// Enabled lets a run publish a port. Turning it off closes every
	// exposure at its next request: OpenExposure refuses them all.
	Enabled bool
	// MaxPerWorkspace bounds one workspace's exposures; zero is 2.
	MaxPerWorkspace int
	// IdleTimeout closes an exposure no request used for this long; zero is
	// 15 minutes.
	IdleTimeout time.Duration
}

// The bounds of an exposure's own fields.
const (
	maxExposureTitle = 200
	// exposureTouchEvery spaces the writes of an exposure's last use: a page
	// of 30 assets is one write, not thirty.
	exposureTouchEvery = 30 * time.Second
)

var (
	// ErrExposeOff is an expose while workspace.expose is off.
	ErrExposeOff = errors.New("this runtime serves no workspace pages (workspace.expose is off)")
	// ErrExposureNotFound is every reason the route answers "no such
	// exposure": unknown, another user's or tenant's, closed, idle, or its
	// workspace not ready. One error, so none of them can be told apart.
	ErrExposureNotFound = errors.New("no such exposure")
	// ErrExposureLimit is a workspace exposing the most ports it may.
	ErrExposureLimit = errors.New("the workspace exposes the most ports it may; unexpose one first")
	// ErrPortExposed is a port the workspace exposes already.
	ErrPortExposed = errors.New("the workspace exposes this port already")
	// ErrExposeArgument is an expose whose port, title or kind is refused.
	ErrExposeArgument = errors.New("invalid expose argument")
)

// SetExpose sets workspace.expose. Boot wiring only.
func (m *Manager) SetExpose(l ExposeLimits) {
	if l.MaxPerWorkspace <= 0 {
		l.MaxPerWorkspace = 2
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = 15 * time.Minute
	}
	m.expose = l
}

// ExposeEnabled reports whether workspace.expose is on.
func (m *Manager) ExposeEnabled() bool { return m.expose.Enabled }

// ExposurePrefix is the API route an exposure is served under, with its
// trailing slash: what expose returns as base_path.
func ExposurePrefix(id string) string { return "/v1/workspaces/exposures/" + id + "/" }

// Expose publishes port of the run's workspace name to the run's user. The
// caller has checked the grant. Only a run owns an exposure: a workspace
// opened over the MCP server has no user conversation to show a page in.
func (m *Manager) Expose(ctx context.Context, o Owner, name string, port int, title, kind string) (store.WorkspaceExposure, error) {
	if !m.expose.Enabled {
		return store.WorkspaceExposure{}, ErrExposeOff
	}
	if o.IsPrincipal() || o.UserID == "" {
		// The route lets in the workspace's user only; a run with none
		// could never open it.
		return store.WorkspaceExposure{}, ErrNotFound
	}
	if port < 1 || port > 65535 {
		return store.WorkspaceExposure{}, fmt.Errorf("%w: port must be from 1 to 65535", ErrExposeArgument)
	}
	if kind == "" {
		kind = store.WorkspaceExposurePage
	}
	if kind != store.WorkspaceExposurePage && kind != store.WorkspaceExposurePanel {
		return store.WorkspaceExposure{}, fmt.Errorf("%w: kind %q: want page or panel", ErrExposeArgument, kind)
	}
	title = strings.TrimSpace(title)
	if len(title) > maxExposureTitle || !utf8.ValidString(title) || strings.ContainsAny(title, "\r\n\x00") {
		return store.WorkspaceExposure{}, fmt.Errorf("%w: title: at most %d bytes of one line of text", ErrExposeArgument, maxExposureTitle)
	}
	r, err := m.Get(ctx, o, name)
	if err != nil {
		return store.WorkspaceExposure{}, err
	}
	if r.State != store.WorkspaceReady {
		return store.WorkspaceExposure{}, fmt.Errorf("%w: %q is %s", ErrNotReady, r.Name, r.State)
	}
	id, err := newExposureID()
	if err != nil {
		return store.WorkspaceExposure{}, err
	}
	e, err := m.store.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: id, TenantID: r.TenantID, UserID: r.UserID,
		WorkspaceID: r.ID, Port: port, Title: title, Kind: kind}, m.expose.MaxPerWorkspace)
	var lim *store.ErrExposureLimit
	var conflict *store.ErrConflict
	switch {
	case errors.As(err, &lim):
		return store.WorkspaceExposure{}, ErrExposureLimit
	case errors.As(err, &conflict):
		return store.WorkspaceExposure{}, ErrPortExposed
	}
	return e, err
}

// Unexpose closes the run's workspace's exposure of port, and reports
// whether there was one.
func (m *Manager) Unexpose(ctx context.Context, o Owner, name string, port int) (bool, error) {
	r, err := m.Get(ctx, o, name)
	if err != nil {
		return false, err
	}
	list, err := m.store.WorkspaceExposureListByWorkspace(ctx, r.TenantID, r.ID)
	if err != nil {
		return false, err
	}
	found := false
	for _, e := range list {
		if e.Port != port {
			continue
		}
		ok, err := m.store.WorkspaceExposureDelete(ctx, r.TenantID, e.ID)
		if err != nil {
			return false, err
		}
		found = found || ok
	}
	return found, nil
}

// Exposures lists the run's workspace's exposures.
func (m *Manager) Exposures(ctx context.Context, o Owner, name string) ([]store.WorkspaceExposure, error) {
	r, err := m.Get(ctx, o, name)
	if err != nil {
		return nil, err
	}
	return m.store.WorkspaceExposureListByWorkspace(ctx, r.TenantID, r.ID)
}

// OpenExposure is the route's whole decision (RFC GW D22, P9 plan D-P9-8):
// the exposure id, read in the caller's tenant, of a run-owned ready
// workspace whose user is the caller, while workspace.expose is on and the
// exposure has not gone idle. Every other case is ErrExposureNotFound, the
// answer for an unknown id, so an id cannot be probed. There is no admin
// exemption: a page is model-written active content shown to one user. A
// store fault is returned as it is (the route fails closed on it).
func (m *Manager) OpenExposure(ctx context.Context, tenantID, userID, id string) (store.WorkspaceExposure, store.WorkspaceRow, error) {
	none := func() (store.WorkspaceExposure, store.WorkspaceRow, error) {
		return store.WorkspaceExposure{}, store.WorkspaceRow{}, ErrExposureNotFound
	}
	if !m.expose.Enabled || userID == "" || !validExposureID(id) {
		return none()
	}
	e, err := m.store.WorkspaceExposureGet(ctx, tenantID, id)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return none()
	}
	if err != nil {
		return store.WorkspaceExposure{}, store.WorkspaceRow{}, err
	}
	r, err := m.store.WorkspaceGet(ctx, tenantID, e.WorkspaceID)
	if errors.As(err, &nf) {
		return none()
	}
	if err != nil {
		return store.WorkspaceExposure{}, store.WorkspaceRow{}, err
	}
	// The queries ask for the tenant already; this keeps a store that ever
	// got it wrong from widening who may open a page.
	if e.TenantID != tenantID || r.TenantID != tenantID || r.UserID != userID || e.UserID != userID ||
		r.OwnerKind != store.WorkspaceOwnerRun || r.State != store.WorkspaceReady {
		return none()
	}
	now := m.now().UTC()
	if now.Sub(e.LastUsedAt) > m.expose.IdleTimeout {
		if _, err := m.store.WorkspaceExposureDelete(ctx, tenantID, id); err != nil {
			m.log.Warn("cannot close an idle exposure", "exposure_id", id, "err", err)
		}
		return none()
	}
	if now.Sub(e.LastUsedAt) > exposureTouchEvery {
		if err := m.store.WorkspaceExposureTouch(ctx, tenantID, id, now); err != nil {
			m.log.Warn("cannot record an exposure's use", "exposure_id", id, "err", err)
		}
	}
	return e, r, nil
}

// SweepExposures closes the exposures idle past the timeout, every tenant's,
// and returns how many.
func (m *Manager) SweepExposures(ctx context.Context) (int, error) {
	idle, err := m.store.WorkspaceExposureListIdle(ctx, m.now().Add(-m.expose.IdleTimeout))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range idle {
		ok, err := m.store.WorkspaceExposureDelete(ctx, e.TenantID, e.ID)
		if err != nil {
			m.log.Warn("cannot close an idle exposure", "exposure_id", e.ID, "err", err)
			continue
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// dropExposures closes a workspace's exposures; a failure is logged, since
// the route refuses a workspace that is not ready whatever its rows say.
func (m *Manager) dropExposures(ctx context.Context, r store.WorkspaceRow) {
	list, err := m.store.WorkspaceExposureListByWorkspace(ctx, r.TenantID, r.ID)
	if err != nil {
		m.log.Warn("cannot list a released workspace's exposures", "workspace_id", r.ID, "err", err)
		return
	}
	for _, e := range list {
		if _, err := m.store.WorkspaceExposureDelete(ctx, r.TenantID, e.ID); err != nil {
			m.log.Warn("cannot close a released workspace's exposure", "exposure_id", e.ID, "err", err)
		}
	}
}

// newExposureID mints an exposure id: 24 hex digits (P9 plan §3.3).
func newExposureID() (string, error) {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// validExposureID: 24 lower-case hex digits, so nothing else reaches a query.
func validExposureID(id string) bool {
	if len(id) != 24 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maxExposureRequest bounds what one request through an exposure may send
// (its headers and body): streamed, never held, but not without end.
const maxExposureRequest = 1 << 30

// DialExposure opens a byte stream to the exposure's port in its workspace,
// for one HTTP request of the route. The workspace row is the one
// OpenExposure returned. The worker reaches the port only when a running
// server of the workspace declared it and, on the host engine, its own
// processes listen on it (#2917): everything else is port_not_served.
func (m *Manager) DialExposure(ctx context.Context, r store.WorkspaceRow, port int) (io.ReadWriteCloser, error) {
	t, err := m.Tunnel(ctx, r, port, maxExposureRequest, 0)
	if err != nil {
		return nil, err
	}
	return t, nil
}
