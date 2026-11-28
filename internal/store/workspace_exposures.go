package store

import (
	"context"
	"fmt"
	"time"
)

// WorkspaceExposure is one exposed port of a workspace (RFC GW §5.3, P9 plan
// §3.3): what /v1/workspaces/exposures/{id}/… serves. The row names its
// workspace; who may open it is decided by the workspace's row (its tenant
// and user), read on every request, so an exposure never reaches further
// than its workspace's pin.
type WorkspaceExposure struct {
	// ID is random (24 hex) and names the route. The caller mints it.
	ID          string
	TenantID    string
	UserID      string
	WorkspaceID string
	Port        int
	Title       string
	// Kind is page or panel (RFC GW D24).
	Kind       string
	CreatedAt  time.Time
	LastUsedAt time.Time
	// WorkspaceName and RunID are the workspace's name and its owning run
	// when the page was exposed (RFC HI §4). Kept on the row because a
	// closed row outlives its workspace's (a release deletes that), and a
	// durable workspace changes owner. Empty on a row from before RFC HI.
	WorkspaceName string
	RunID         string
	// IdleTimeoutS is the page's own idle timeout, as its agent asked (RFC
	// HI §3); nil is the operator's workspace.expose.idle_timeout_s.
	IdleTimeoutS *int
	// LeaseExpiresAt is the end of the client's lease (RFC HI §2); nil is
	// no lease.
	LeaseExpiresAt *time.Time
	// ClosedAt is when the page closed, and ClosedReason why (one of the
	// WorkspaceExposureClosed* reasons); nil and "" while it is live. A
	// closed row is kept for a day so a client can learn why its page went
	// (RFC HI D6), and every reader that serves a page treats it as absent.
	ClosedAt     *time.Time
	ClosedReason string
}

// Live reports whether the page has not closed.
func (e WorkspaceExposure) Live() bool { return e.ClosedAt == nil }

// The kinds of an exposure (RFC GW D24).
const (
	WorkspaceExposurePage  = "page"
	WorkspaceExposurePanel = "panel"
)

// Why a page closed (RFC HI §4): what WorkspaceExposureClose records.
const (
	WorkspaceExposureClosedUnexposed = "unexposed"
	WorkspaceExposureClosedReleased  = "released"
	WorkspaceExposureClosedIdle      = "idle"
	WorkspaceExposureClosedLost      = "lost"
)

// WorkspaceExposureStore persists exposures. Every read takes the tenant and
// treats another tenant's row as absent, but the sweep's. A closed row is
// kept (RFC HI D6): Get returns it, with ClosedAt set; every list returns
// live rows only.
type WorkspaceExposureStore interface {
	// WorkspaceExposureCreate writes a new live exposure, stamping both
	// times, unless its workspace already has max live exposures
	// (*ErrExposureLimit) or a live one on the same port (*ErrConflict).
	WorkspaceExposureCreate(ctx context.Context, row WorkspaceExposure, max int) (WorkspaceExposure, error)
	// WorkspaceExposureGet returns the tenant's exposure of this id, live
	// or closed; *ErrNotFound when there is none, or it is another tenant's.
	WorkspaceExposureGet(ctx context.Context, tenantID, id string) (WorkspaceExposure, error)
	// WorkspaceExposureListByWorkspace returns the workspace's live
	// exposures, oldest first. Empty, not an error, when it has none.
	WorkspaceExposureListByWorkspace(ctx context.Context, tenantID, workspaceID string) ([]WorkspaceExposure, error)
	// WorkspaceExposureListByUser returns the user's live exposures in the
	// tenant, oldest first.
	WorkspaceExposureListByUser(ctx context.Context, tenantID, userID string) ([]WorkspaceExposure, error)
	// WorkspaceExposureClose marks the tenant's live exposure closed at at,
	// for reason, and reports whether it was live. A closed row is never
	// reopened.
	WorkspaceExposureClose(ctx context.Context, tenantID, id, reason string, at time.Time) (bool, error)
	// WorkspaceExposureTouch sets the live exposure's last_used_at to at.
	WorkspaceExposureTouch(ctx context.Context, tenantID, id string, at time.Time) error
	// WorkspaceExposureListIdle returns every tenant's live exposures last
	// used before the given time, oldest first: the idle sweep's candidates
	// (each is idle by its own timeout, which the caller compares).
	WorkspaceExposureListIdle(ctx context.Context, before time.Time) ([]WorkspaceExposure, error)
	// WorkspaceExposurePurgeClosed deletes every tenant's exposures closed
	// before the given time, and returns how many.
	WorkspaceExposurePurgeClosed(ctx context.Context, before time.Time) (int64, error)
}

// ErrExposureLimit is an exposure refused because its workspace holds the
// most it may.
type ErrExposureLimit struct {
	Max int
}

func (e *ErrExposureLimit) Error() string {
	return fmt.Sprintf("the workspace exposes %d ports already, the most it may", e.Max)
}
