package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// The RFC AL Path-tree (dirents) queries live in internal/store/sqlshared,
// written once for both stores.

func (s *Store) DirentCreate(ctx context.Context, row store.DirentRow) (store.DirentRow, error) {
	return sqlshared.DirentCreate(ctx, s.shared(), row)
}

func (s *Store) DirentGet(ctx context.Context, tenantID, scope, scopeID, parentPath, name string) (store.DirentRow, error) {
	return sqlshared.DirentGet(ctx, s.shared(), tenantID, scope, scopeID, parentPath, name)
}

func (s *Store) DirentList(ctx context.Context, tenantID, scope, scopeID, parentPath string) ([]store.DirentRow, error) {
	return sqlshared.DirentList(ctx, s.shared(), tenantID, scope, scopeID, parentPath)
}

func (s *Store) DirentListUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) ([]store.DirentRow, error) {
	return sqlshared.DirentListUnder(ctx, s.shared(), tenantID, scope, scopeID, prefix)
}

func (s *Store) DirentDelete(ctx context.Context, tenantID, scope, scopeID, parentPath, name string) (bool, error) {
	return sqlshared.DirentDelete(ctx, s.shared(), tenantID, scope, scopeID, parentPath, name)
}

func (s *Store) DirentDeleteUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) (int, error) {
	return sqlshared.DirentDeleteUnder(ctx, s.shared(), tenantID, scope, scopeID, prefix)
}

func (s *Store) DirentMove(ctx context.Context, tenantID, scope, scopeID, fromParent, fromName, toParent, toName string) (bool, error) {
	return sqlshared.DirentMove(ctx, s.shared(), tenantID, scope, scopeID, fromParent, fromName, toParent, toName)
}

func (s *Store) SnapshotReadDirents(ctx context.Context) ([]store.DirentRow, error) {
	return sqlshared.SnapshotReadDirents(ctx, s.shared())
}

func (s *Store) SnapshotRestoreDirent(ctx context.Context, row store.DirentRow) (bool, error) {
	return sqlshared.SnapshotRestoreDirent(ctx, s.shared(), row)
}
