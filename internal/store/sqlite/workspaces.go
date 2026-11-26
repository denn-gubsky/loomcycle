package sqlite

import (
	"context"
	"time"

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

func (s *Store) WorkspaceSetGit(ctx context.Context, tenantID, id string, git store.WorkspaceGit) (bool, error) {
	return sqlshared.WorkspaceSetGit(ctx, s.shared(), tenantID, id, git)
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

func (s *Store) WorkspaceTouchByWorker(ctx context.Context, workerID string) (int, error) {
	return sqlshared.WorkspaceTouchByWorker(ctx, s.shared(), workerID)
}

func (s *Store) WorkspaceListStale(ctx context.Context, before time.Time) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceListStale(ctx, s.shared(), before)
}

func (s *Store) WorkspaceSetShares(ctx context.Context, tenantID, id string, from, to []store.WorkspaceShare) (bool, error) {
	return sqlshared.WorkspaceSetShares(ctx, s.shared(), tenantID, id, from, to)
}

func (s *Store) WorkspaceListByOwnerAgent(ctx context.Context, tenantID, ownerAgentID, name string) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceListByOwnerAgent(ctx, s.shared(), tenantID, ownerAgentID, name)
}

func (s *Store) WorkspaceListSharedWith(ctx context.Context, tenantID, runID string) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceListSharedWith(ctx, s.shared(), tenantID, runID)
}

func (s *Store) WorkspaceListPrincipalOwned(ctx context.Context) ([]store.WorkspaceRow, error) {
	return sqlshared.WorkspaceListPrincipalOwned(ctx, s.shared())
}

func (s *Store) WorkspaceTouchUsed(ctx context.Context, tenantID, id string, at time.Time) error {
	return sqlshared.WorkspaceTouchUsed(ctx, s.shared(), tenantID, id, at)
}

func (s *Store) WorkspaceGetDurable(ctx context.Context, tenantID, userID, name string) (store.WorkspaceRow, error) {
	return sqlshared.WorkspaceGetDurable(ctx, s.shared(), tenantID, userID, name)
}

func (s *Store) WorkspacePin(ctx context.Context, tenantID, id, fromOwnerRunID string, to store.WorkspaceOwner) (bool, error) {
	return sqlshared.WorkspacePin(ctx, s.shared(), tenantID, id, fromOwnerRunID, to)
}

func (s *Store) WorkspaceUnpin(ctx context.Context, tenantID, id, fromRunID string) (bool, error) {
	return sqlshared.WorkspaceUnpin(ctx, s.shared(), tenantID, id, fromRunID)
}

func (s *Store) WorkspaceCountDurableByUser(ctx context.Context, tenantID, userID string) (int, error) {
	return sqlshared.WorkspaceCountDurableByUser(ctx, s.shared(), tenantID, userID)
}

func (s *Store) SnapshotReadWorkspaces(ctx context.Context) ([]store.WorkspaceRow, error) {
	return sqlshared.SnapshotReadWorkspaces(ctx, s.shared())
}
