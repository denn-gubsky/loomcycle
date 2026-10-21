package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// The RFC AH volume substrate (volume_defs, ephemeral_volume_defs) queries
// live in internal/store/sqlshared, written once for both stores.

func (s *Store) VolumeDefCreate(ctx context.Context, row store.VolumeDefRow) (store.VolumeDefRow, error) {
	return sqlshared.VolumeDefCreate(ctx, s.shared(), row)
}

func (s *Store) VolumeDefGetByName(ctx context.Context, tenantID, name string) (store.VolumeDefRow, error) {
	return sqlshared.VolumeDefGetByName(ctx, s.shared(), tenantID, name)
}

func (s *Store) VolumeDefList(ctx context.Context, tenantID string) ([]store.VolumeDefRow, error) {
	return sqlshared.VolumeDefList(ctx, s.shared(), tenantID)
}

func (s *Store) VolumeDefDelete(ctx context.Context, tenantID, name string) (bool, error) {
	return sqlshared.VolumeDefDelete(ctx, s.shared(), tenantID, name)
}

func (s *Store) SnapshotReadVolumeDefs(ctx context.Context) ([]store.VolumeDefRow, error) {
	return sqlshared.SnapshotReadVolumeDefs(ctx, s.shared())
}

func (s *Store) SnapshotRestoreVolumeDef(ctx context.Context, row store.VolumeDefRow) (bool, error) {
	return sqlshared.SnapshotRestoreVolumeDef(ctx, s.shared(), row)
}

func (s *Store) EphemeralVolumeCreate(ctx context.Context, row store.EphemeralVolumeDefRow) (store.EphemeralVolumeDefRow, error) {
	return sqlshared.EphemeralVolumeCreate(ctx, s.shared(), row)
}

func (s *Store) EphemeralVolumeListByRun(ctx context.Context, rootRunID string) ([]store.EphemeralVolumeDefRow, error) {
	return sqlshared.EphemeralVolumeListByRun(ctx, s.shared(), rootRunID)
}

func (s *Store) EphemeralVolumeSetReadOnly(ctx context.Context, rootRunID, name string) (bool, error) {
	return sqlshared.EphemeralVolumeSetReadOnly(ctx, s.shared(), rootRunID, name)
}

func (s *Store) EphemeralVolumeListByTenant(ctx context.Context, tenantID string) ([]store.EphemeralVolumeDefRow, error) {
	return sqlshared.EphemeralVolumeListByTenant(ctx, s.shared(), tenantID)
}

func (s *Store) EphemeralVolumeDeleteByRun(ctx context.Context, rootRunID string) (int, error) {
	return sqlshared.EphemeralVolumeDeleteByRun(ctx, s.shared(), rootRunID)
}

func (s *Store) EphemeralVolumeSweepCandidates(ctx context.Context) ([]store.EphemeralVolumeSweepRow, error) {
	return sqlshared.EphemeralVolumeSweepCandidates(ctx, s.shared())
}
