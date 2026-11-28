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
	// MaxIdleTimeout bounds the idle timeout an agent may ask for one page
	// (RFC HI §3); zero is IdleTimeout, so an agent can ask for no more
	// than the operator's own.
	MaxIdleTimeout time.Duration
}

// The bounds of an exposure's own fields.
const (
	maxExposureTitle = 200
	// exposureTouchEvery spaces the writes of an exposure's last use: a page
	// of 30 assets is one write, not thirty.
	exposureTouchEvery = 30 * time.Second
	// minPageIdleTimeout is the shortest idle timeout an agent may ask for:
	// twice the touch spacing, so a page in use never reads as idle.
	minPageIdleTimeout = 2 * exposureTouchEvery
	// closedExposureKeep is how long a closed page's row is kept for a
	// client to ask why it went (RFC HI D6).
	closedExposureKeep = 24 * time.Hour
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
	if l.MaxIdleTimeout <= 0 {
		l.MaxIdleTimeout = l.IdleTimeout
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
// idleTimeoutS is the page's own idle timeout (RFC HI §3); zero is the
// operator's.
func (m *Manager) Expose(ctx context.Context, o Owner, name string, port int, title, kind string, idleTimeoutS int) (store.WorkspaceExposure, error) {
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
	var idle *int
	if idleTimeoutS != 0 {
		lo, hi := int(minPageIdleTimeout/time.Second), int(m.expose.MaxIdleTimeout/time.Second)
		if idleTimeoutS < lo || idleTimeoutS > hi {
			return store.WorkspaceExposure{}, fmt.Errorf("%w: idle_timeout_s %d: the operator's bound is %d to %d seconds (workspace.expose.max_idle_timeout_s)",
				ErrExposeArgument, idleTimeoutS, lo, hi)
		}
		idle = &idleTimeoutS
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
		WorkspaceID: r.ID, Port: port, Title: title, Kind: kind, WorkspaceName: r.Name, RunID: r.OwnerRunID, IdleTimeoutS: idle},
		m.expose.MaxPerWorkspace)
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
		ok, err := m.store.WorkspaceExposureClose(ctx, r.TenantID, e.ID, SurfaceUnexposed, m.now())
		if err != nil {
			return false, err
		}
		if ok {
			m.tellClosed(ctx, r, e, SurfaceUnexposed)
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
// exposure has neither closed nor gone idle. Every other case is
// ErrExposureNotFound, the answer for an unknown id, so an id cannot be
// probed. There is no admin exemption: a page is model-written active
// content shown to one user. A store fault is returned as it is (the route
// fails closed on it).
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
	// A closed row is kept (RFC HI D6) and answers as a missing one.
	if !e.Live() || e.TenantID != tenantID || r.TenantID != tenantID || r.UserID != userID || e.UserID != userID ||
		r.OwnerKind != store.WorkspaceOwnerRun || r.State != store.WorkspaceReady {
		return none()
	}
	now := m.now().UTC()
	if now.After(m.ExposureDeadline(e)) {
		ok, err := m.store.WorkspaceExposureClose(ctx, tenantID, id, SurfaceIdle, now)
		if err != nil {
			m.log.Warn("cannot close an idle exposure", "exposure_id", id, "err", err)
		}
		if ok {
			m.tellClosed(ctx, r, e, SurfaceIdle)
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

// SweepExposures closes the exposures idle past their timeout, every
// tenant's, and returns how many; it then purges the rows closed more than a
// day ago (RFC HI D6).
func (m *Manager) SweepExposures(ctx context.Context) (int, error) {
	now := m.now()
	// A page's own timeout may be shorter than the operator's, down to
	// minPageIdleTimeout: list from the shorter, and decide by each row's.
	idle, err := m.store.WorkspaceExposureListIdle(ctx, now.Add(-min(m.expose.IdleTimeout, minPageIdleTimeout)))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range idle {
		if !now.After(m.ExposureDeadline(e)) {
			continue
		}
		ok, err := m.store.WorkspaceExposureClose(ctx, e.TenantID, e.ID, SurfaceIdle, now)
		if err != nil {
			m.log.Warn("cannot close an idle exposure", "exposure_id", e.ID, "err", err)
			continue
		}
		if !ok {
			continue
		}
		n++
		// The owning run is told; a workspace already gone has no run to
		// tell (its release told it).
		if r, err := m.store.WorkspaceGet(ctx, e.TenantID, e.WorkspaceID); err == nil {
			m.tellClosed(ctx, r, e, SurfaceIdle)
		}
	}
	if _, err := m.store.WorkspaceExposurePurgeClosed(ctx, now.Add(-closedExposureKeep)); err != nil {
		m.log.Warn("cannot purge closed exposures", "err", err)
	}
	return n, nil
}

// ExposureIdleTimeout is page e's idle timeout: its own, else the
// operator's.
func (m *Manager) ExposureIdleTimeout(e store.WorkspaceExposure) time.Duration {
	if e.IdleTimeoutS != nil {
		return time.Duration(*e.IdleTimeoutS) * time.Second
	}
	return m.expose.IdleTimeout
}

// ExposureDeadline is when live page e closes idle: its last use plus its
// idle timeout, or its lease's end when that is later (RFC HI §4).
func (m *Manager) ExposureDeadline(e store.WorkspaceExposure) time.Time {
	at := e.LastUsedAt.Add(m.ExposureIdleTimeout(e))
	if e.LeaseExpiresAt != nil && e.LeaseExpiresAt.After(at) {
		at = *e.LeaseExpiresAt
	}
	return at
}

// ExposureState is the state of page id for its own user (RFC HI §4): the
// row, live or closed. Every case the route refuses as unknown (another
// user's or tenant's, an unknown id, workspace.expose off) is
// ErrExposureNotFound. A live page past its idle deadline is closed now, as
// the route would close it, so the answer never reads live past it. It
// never counts as a use of the page.
func (m *Manager) ExposureState(ctx context.Context, tenantID, userID, id string) (store.WorkspaceExposure, error) {
	if !m.expose.Enabled || userID == "" || !validExposureID(id) {
		return store.WorkspaceExposure{}, ErrExposureNotFound
	}
	e, err := m.store.WorkspaceExposureGet(ctx, tenantID, id)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return store.WorkspaceExposure{}, ErrExposureNotFound
	}
	if err != nil {
		return store.WorkspaceExposure{}, err
	}
	if e.TenantID != tenantID || e.UserID != userID {
		return store.WorkspaceExposure{}, ErrExposureNotFound
	}
	now := m.now().UTC()
	if e.Live() && now.After(m.ExposureDeadline(e)) {
		ok, err := m.store.WorkspaceExposureClose(ctx, tenantID, id, SurfaceIdle, now)
		if err != nil {
			return store.WorkspaceExposure{}, err
		}
		if ok {
			if r, err := m.store.WorkspaceGet(ctx, tenantID, e.WorkspaceID); err == nil {
				m.tellClosed(ctx, r, e, SurfaceIdle)
			}
		}
		if e, err = m.store.WorkspaceExposureGet(ctx, tenantID, id); err != nil {
			return store.WorkspaceExposure{}, err
		}
	}
	return m.withOwner(ctx, e), nil
}

// UserExposures lists the user's live pages, those past their idle deadline
// left out (the sweep closes them).
func (m *Manager) UserExposures(ctx context.Context, tenantID, userID string) ([]store.WorkspaceExposure, error) {
	if !m.expose.Enabled || userID == "" {
		return []store.WorkspaceExposure{}, nil
	}
	list, err := m.store.WorkspaceExposureListByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	now := m.now()
	out := make([]store.WorkspaceExposure, 0, len(list))
	for _, e := range list {
		if e.TenantID != tenantID || e.UserID != userID || now.After(m.ExposureDeadline(e)) {
			continue
		}
		out = append(out, m.withOwner(ctx, e))
	}
	return out, nil
}

// withOwner fills a row from before RFC HI, which names no workspace or run,
// from its workspace's row while that exists.
func (m *Manager) withOwner(ctx context.Context, e store.WorkspaceExposure) store.WorkspaceExposure {
	if e.WorkspaceName != "" {
		return e
	}
	if r, err := m.store.WorkspaceGet(ctx, e.TenantID, e.WorkspaceID); err == nil {
		e.WorkspaceName, e.RunID = r.Name, r.OwnerRunID
	}
	return e
}

// dropExposures closes a workspace's exposures; a failure is logged, since
// the route refuses a workspace that is not ready whatever its rows say.
func (m *Manager) dropExposures(ctx context.Context, r store.WorkspaceRow) {
	m.dropExposuresAs(ctx, r, SurfaceReleased)
}

// closeLostPages closes the pages of a workspace whose worker went away,
// telling its run surface_closed with the reason lost (ruling Q6). The
// route already refuses a workspace that is not ready; this tells the run
// and closes the rows.
func (m *Manager) closeLostPages(ctx context.Context, r store.WorkspaceRow) {
	m.dropExposuresAs(ctx, r, SurfaceLost)
}

// dropExposuresAs closes r's exposures, telling its run why.
func (m *Manager) dropExposuresAs(ctx context.Context, r store.WorkspaceRow, reason string) {
	list, err := m.store.WorkspaceExposureListByWorkspace(ctx, r.TenantID, r.ID)
	if err != nil {
		m.log.Warn("cannot list a released workspace's exposures", "workspace_id", r.ID, "err", err)
		return
	}
	for _, e := range list {
		ok, err := m.store.WorkspaceExposureClose(ctx, r.TenantID, e.ID, reason, m.now())
		if err != nil {
			m.log.Warn("cannot close a released workspace's exposure", "exposure_id", e.ID, "err", err)
			continue
		}
		if ok {
			m.tellClosed(ctx, r, e, reason)
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

// SurfaceClosed is a page that went (RFC GW D24): the owning run's stream is
// told, by SetSurfaceNotifier's function.
type SurfaceClosed struct {
	ExposureID string
	// Reason is unexposed, released or idle.
	Reason string
}

// The reasons a page goes, as its run is told and its row records.
const (
	SurfaceUnexposed = store.WorkspaceExposureClosedUnexposed
	// SurfaceLost: the workspace's worker went away (RFC GW P12 ruling Q6).
	SurfaceLost     = store.WorkspaceExposureClosedLost
	SurfaceReleased = store.WorkspaceExposureClosedReleased
	SurfaceIdle     = store.WorkspaceExposureClosedIdle
)

// SurfaceNotifier tells the run ownerRunID that one of its pages went; ctx
// is the closer's (the run's own tool call, or a sweep's).
type SurfaceNotifier func(ctx context.Context, ownerRunID string, c SurfaceClosed)

// SetSurfaceNotifier sets who is told of a page's close. Boot wiring only.
func (m *Manager) SetSurfaceNotifier(fn SurfaceNotifier) { m.surfaceClosed = fn }

// tellClosed tells the owning run of workspace r that page e went.
func (m *Manager) tellClosed(ctx context.Context, r store.WorkspaceRow, e store.WorkspaceExposure, reason string) {
	if m.surfaceClosed == nil || r.OwnerKind != store.WorkspaceOwnerRun || r.OwnerRunID == "" {
		return
	}
	m.surfaceClosed(ctx, r.OwnerRunID, SurfaceClosed{ExposureID: e.ID, Reason: reason})
}
