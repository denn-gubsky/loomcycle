package postgres

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

func (s *Store) WorkspaceExposureDelete(ctx context.Context, tenantID, id string) (bool, error) {
	return sqlshared.WorkspaceExposureDelete(ctx, s.shared(), tenantID, id)
}

func (s *Store) WorkspaceExposureTouch(ctx context.Context, tenantID, id string, at time.Time) error {
	return sqlshared.WorkspaceExposureTouch(ctx, s.shared(), tenantID, id, at)
}

func (s *Store) WorkspaceExposureListIdle(ctx context.Context, before time.Time) ([]store.WorkspaceExposure, error) {
	return sqlshared.WorkspaceExposureListIdle(ctx, s.shared(), before)
}
