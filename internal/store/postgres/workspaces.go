package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// The RFC GW workspace queries live in internal/store/sqlshared, written once
// for both stores.

func (s *Store) WorkspaceCreate(ctx context.Context, row store.WorkspaceRow) (store.WorkspaceRow, error) {
	return sqlshared.WorkspaceCreate(ctx, s.shared(), row)
}

func (s *Store) WorkspaceGet(ctx context.Context, tenantID, id string) (store.WorkspaceRow, error) {
	return sqlshared.WorkspaceGet(ctx, s.shared(), tenantID, id)
}

func (s *Store) WorkspaceGetByName(ctx context.Context, tenantID, ownerRunID, name string) (store.WorkspaceRow, error) {
	return sqlshared.WorkspaceGetByName(ctx, s.shared(), tenantID, ownerRunID, name)
}

func (s *Store) WorkspaceListByOwnerRun(ctx context.Context, tenantID, ownerRunID string) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceListByOwnerRun(ctx, s.shared(), tenantID, ownerRunID)
}

func (s *Store) WorkspaceListByWorker(ctx context.Context, workerID string, states ...store.WorkspaceState) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceListByWorker(ctx, s.shared(), workerID, states...)
}

func (s *Store) WorkspaceSetState(ctx context.Context, tenantID, id string, from, to store.WorkspaceState) (bool, error) {
	return sqlshared.WorkspaceSetState(ctx, s.shared(), tenantID, id, from, to)
}

func (s *Store) WorkspaceDelete(ctx context.Context, tenantID, id string) (bool, error) {
	return sqlshared.WorkspaceDelete(ctx, s.shared(), tenantID, id)
}

func (s *Store) WorkspaceCountActiveByWorker(ctx context.Context, workerID string) (int, error) {
	return sqlshared.WorkspaceCountActiveByWorker(ctx, s.shared(), workerID)
}

func (s *Store) WorkspaceCountActiveByTenant(ctx context.Context, tenantID string) (int, error) {
	return sqlshared.WorkspaceCountActiveByTenant(ctx, s.shared(), tenantID)
}

func (s *Store) WorkspaceSweepCandidates(ctx context.Context) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceSweepCandidates(ctx, s.shared())
}
