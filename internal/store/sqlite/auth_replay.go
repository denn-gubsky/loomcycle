package sqlite

import (
	"context"
	"fmt"
	"time"
)

// auth_replay.go — RFC FA P3. Sibling of internal/store/postgres/auth_replay.go
// (migration 0118 explains the table); contract on store.AuthReplayStore,
// asserted by internal/store/storetest/auth_replay.go.

// AuthReplayAdmit implements store.AuthReplayStore: one statement, so the
// write lock it takes serialises concurrent admits of one pair.
func (s *Store) AuthReplayAdmit(ctx context.Context, scope, key string, expires, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_replay (scope, key_hash, expires_at) VALUES (?1, ?2, ?3)
		ON CONFLICT (scope, key_hash) DO UPDATE SET expires_at = excluded.expires_at
		WHERE auth_replay.expires_at <= ?4`, scope, key, expires.UnixNano(), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("auth replay admit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("auth replay admit: %w", err)
	}
	return n == 1, nil
}

// AuthReplayDeleteExpired implements store.AuthReplayStore.
func (s *Store) AuthReplayDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_replay WHERE expires_at < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("auth replay delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
