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
}

// The kinds of an exposure (RFC GW D24).
const (
	WorkspaceExposurePage  = "page"
	WorkspaceExposurePanel = "panel"
)

// WorkspaceExposureStore persists exposures. Every read takes the tenant and
// treats another tenant's row as absent, but the idle sweep's.
type WorkspaceExposureStore interface {
	// WorkspaceExposureCreate writes a new exposure, stamping both times,
	// unless its workspace already has max exposures (*ErrExposureLimit) or one on
	// the same port (*ErrConflict).
	WorkspaceExposureCreate(ctx context.Context, row WorkspaceExposure, max int) (WorkspaceExposure, error)
	// WorkspaceExposureGet returns the tenant's exposure of this id;
	// *ErrNotFound when there is none, or it is another tenant's.
	WorkspaceExposureGet(ctx context.Context, tenantID, id string) (WorkspaceExposure, error)
	// WorkspaceExposureListByWorkspace returns the workspace's exposures,
	// oldest first. Empty, not an error, when it has none.
	WorkspaceExposureListByWorkspace(ctx context.Context, tenantID, workspaceID string) ([]WorkspaceExposure, error)
	// WorkspaceExposureDelete removes the tenant's exposure and reports
	// whether it was there.
	WorkspaceExposureDelete(ctx context.Context, tenantID, id string) (bool, error)
	// WorkspaceExposureTouch sets the exposure's last_used_at to at.
	WorkspaceExposureTouch(ctx context.Context, tenantID, id string, at time.Time) error
	// WorkspaceExposureListIdle returns every tenant's exposures last used
	// before the given time, oldest first: the idle timeout's work list.
	WorkspaceExposureListIdle(ctx context.Context, before time.Time) ([]WorkspaceExposure, error)
}

// ErrExposureLimit is an exposure refused because its workspace holds the
// most it may.
type ErrExposureLimit struct {
	Max int
}

func (e *ErrExposureLimit) Error() string {
	return fmt.Sprintf("the workspace exposes %d ports already, the most it may", e.Max)
}
