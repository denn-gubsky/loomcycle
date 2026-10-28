package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
)

// UserPasskeyDeleteKeeping implements store.PasskeyStore. Every passkey row
// of the subject is locked first: a concurrent removal waits, then counts
// what this one left.
func (s *Store) UserPasskeyDeleteKeeping(ctx context.Context, tenantID, subject string, credentialID []byte) (bool, bool, error) {
	want := base64.RawURLEncoding.EncodeToString(credentialID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT credential_id, flagged_at IS NULL FROM user_passkeys
		WHERE tenant_id = $1 AND subject = $2 FOR UPDATE`, tenantID, subject)
	if err != nil {
		return false, false, fmt.Errorf("passkey delete: lock: %w", err)
	}
	found, targetUsable, otherUsable := false, false, 0
	for rows.Next() {
		var id string
		var usable bool
		if err := rows.Scan(&id, &usable); err != nil {
			rows.Close()
			return false, false, err
		}
		switch {
		case id == want:
			found, targetUsable = true, usable
		case usable:
			otherUsable++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	if !found {
		return false, false, nil
	}
	if targetUsable && otherUsable == 0 {
		return false, true, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_passkeys WHERE tenant_id = $1 AND subject = $2 AND credential_id = $3`,
		tenantID, subject, want); err != nil {
		return false, false, fmt.Errorf("passkey delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return true, false, nil
}
