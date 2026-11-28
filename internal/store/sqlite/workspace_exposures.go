package sqlite

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// The RFC GW exposure queries live in internal/store/sqlshared.

func (s *Store) WorkspaceExposureCreate(ctx context.Context, row store.WorkspaceExposure, max int) (store.WorkspaceExposure, error) {
	return sqlshared.WorkspaceExposureCreate(ctx, s.shared(), row, max)
}

func (s *Store) WorkspaceExposureGet(ctx context.Context, tenantID, id string) (store.WorkspaceExposure, error) {
	return sqlshared.WorkspaceExposureGet(ctx, s.shared(), tenantID, id)
}

func (s *Store) WorkspaceExposureListByWorkspace(ctx context.Context, tenantID, workspaceID string) ([]store.WorkspaceExposure, error) {
	return sqlshared.WorkspaceExposureListByWorkspace(ctx, s.shared(), tenantID, workspaceID)
}

func (s *Store) WorkspaceExposureListByUser(ctx context.Context, tenantID, userID string) ([]store.WorkspaceExposure, error) {
	return sqlshared.WorkspaceExposureListByUser(ctx, s.shared(), tenantID, userID)
}

func (s *Store) WorkspaceExposureClose(ctx context.Context, tenantID, id, reason string, at time.Time) (bool, error) {
	return sqlshared.WorkspaceExposureClose(ctx, s.shared(), tenantID, id, reason, at)
}

func (s *Store) WorkspaceExposurePurgeClosed(ctx context.Context, before time.Time) (int64, error) {
	return sqlshared.WorkspaceExposurePurgeClosed(ctx, s.shared(), before)
}

func (s *Store) WorkspaceExposureTouch(ctx context.Context, tenantID, id string, at time.Time) error {
	return sqlshared.WorkspaceExposureTouch(ctx, s.shared(), tenantID, id, at)
}

func (s *Store) WorkspaceExposureListIdle(ctx context.Context, before time.Time) ([]store.WorkspaceExposure, error) {
	return sqlshared.WorkspaceExposureListIdle(ctx, s.shared(), before)
}
