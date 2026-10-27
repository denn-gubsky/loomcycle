package postgres

import (
	"context"
	"fmt"
	"time"
)

// auth_replay.go — RFC FA P3 (migration 0118). Sibling of
// internal/store/sqlite/auth_replay.go; contract on store.AuthReplayStore,
// asserted by internal/store/storetest/auth_replay.go.

// AuthReplayAdmit implements store.AuthReplayStore. An existing row is
// overwritten only when it has expired, in the same statement as the insert,
// so the row count says whether this call admitted the pair.
func (s *Store) AuthReplayAdmit(ctx context.Context, scope, key string, expires, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO auth_replay (scope, key_hash, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (scope, key_hash) DO UPDATE SET expires_at = EXCLUDED.expires_at
		WHERE auth_replay.expires_at <= $4`, scope, key, expires.UTC(), now.UTC())
	if err != nil {
		return false, fmt.Errorf("auth replay admit: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// AuthReplayDeleteExpired implements store.AuthReplayStore.
func (s *Store) AuthReplayDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_replay WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("auth replay delete expired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
