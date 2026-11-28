package lifecycle

// expose_lease.go — a client's lease on a workspace page (RFC HI §2, D4).
//
// While a lease is live the page does not close idle (ExposureDeadline counts
// it) and the parked interactive run that owns the page's workspace does not
// end at its idle deadline (RunLeaseEnd, asked by the run at that deadline).
// The lease is stored on the exposure row, so every replica sees it, and is
// the latest end any client asked for.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

var (
	// ErrLeasesOff is a lease asked of a runtime whose operator turned
	// leases off (workspace.expose.max_lease_s: 0).
	ErrLeasesOff = errors.New("this runtime takes no leases on workspace pages (workspace.expose.max_lease_s is 0)")
	// ErrLeaseTTL is a lease length outside 1 second to max_lease_s.
	ErrLeaseTTL = errors.New("invalid lease length")
)

// ExposeMaxLease is the longest lease a client may take on a page
// (workspace.expose.max_lease_s); 0 when leases are off, as they are while
// workspace.expose is off.
func (m *Manager) ExposeMaxLease() time.Duration {
	if !m.expose.Enabled {
		return 0
	}
	return m.expose.MaxLease
}

// LeaseExposure leases page id to its own user until ttl from now, unless a
// lease already runs later, and returns the lease's end. The ttl is checked
// before the page is looked up, so its refusal says nothing of the page.
// Every case the route answers as an unknown id (another user's or tenant's,
// closed, idle) is ErrExposureNotFound. Taking a lease is not a use of the
// page: its idle deadline moves only by the lease.
func (m *Manager) LeaseExposure(ctx context.Context, tenantID, userID, id string, ttl time.Duration) (time.Time, error) {
	if err := m.leasesOn(); err != nil {
		return time.Time{}, err
	}
	if max := m.expose.MaxLease; ttl < time.Second || ttl > max {
		return time.Time{}, fmt.Errorf("%w: ttl_s must be from 1 to %d (workspace.expose.max_lease_s)", ErrLeaseTTL, int(max/time.Second))
	}
	if _, _, err := m.liveExposure(ctx, tenantID, userID, id); err != nil {
		return time.Time{}, err
	}
	end, err := m.store.WorkspaceExposureExtendLease(ctx, tenantID, id, m.now().UTC().Add(ttl))
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		// Closed between the decision and the write.
		return time.Time{}, ErrExposureNotFound
	}
	return end, err
}

// ReleaseExposureLease ends the lease on page id, every client's, for its own
// user. The page then closes at its next idle deadline. Every case the route
// answers as an unknown id is ErrExposureNotFound.
func (m *Manager) ReleaseExposureLease(ctx context.Context, tenantID, userID, id string) error {
	if err := m.leasesOn(); err != nil {
		return err
	}
	if _, _, err := m.liveExposure(ctx, tenantID, userID, id); err != nil {
		return err
	}
	ok, err := m.store.WorkspaceExposureClearLease(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrExposureNotFound
	}
	return nil
}

// leasesOn refuses a lease while workspace.expose is off (as an unknown page,
// as every exposure route answers then) or leases are off.
func (m *Manager) leasesOn() error {
	if !m.expose.Enabled {
		return ErrExposureNotFound
	}
	if m.expose.MaxLease <= 0 {
		return ErrLeasesOff
	}
	return nil
}

// RunLeaseEnd is the latest end of a live lease on any page the route would
// serve of the workspaces run runID owns, or the zero time when none is live
// now (RFC HI D4). It reads the store, so a lease another replica took
// counts. A parked run asks it only at its idle deadline, never on a timer
// per page. A lease is counted no further than max_lease_s from now, so an
// operator who lowered the bound is not held to an older, longer lease.
func (m *Manager) RunLeaseEnd(ctx context.Context, runID string) (time.Time, error) {
	if m.ExposeMaxLease() <= 0 || runID == "" {
		return time.Time{}, nil
	}
	run, err := m.store.GetRun(ctx, runID)
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, err
	}
	rows, err := m.store.WorkspaceListByOwnerRun(ctx, run.TenantID, runID)
	if err != nil {
		return time.Time{}, err
	}
	now := m.now().UTC()
	var end time.Time
	for _, r := range rows {
		if r.OwnerKind != store.WorkspaceOwnerRun || r.State != store.WorkspaceReady {
			continue
		}
		pages, err := m.store.WorkspaceExposureListByWorkspace(ctx, r.TenantID, r.ID)
		if err != nil {
			return time.Time{}, err
		}
		for _, e := range pages {
			if e.LeaseExpiresAt != nil && e.LeaseExpiresAt.After(now) && e.LeaseExpiresAt.After(end) {
				end = *e.LeaseExpiresAt
			}
		}
	}
	if limit := now.Add(m.expose.MaxLease); end.After(limit) {
		end = limit
	}
	return end, nil
}
