package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
)

// errLastPasskey ends the transaction of a refused removal.
var errLastPasskey = errors.New("the last usable passkey")

// UserPasskeyDeleteKeeping implements store.PasskeyStore, under BEGIN
// IMMEDIATE so the count and the delete are one write.
func (s *Store) UserPasskeyDeleteKeeping(ctx context.Context, tenantID, subject string, credentialID []byte) (bool, bool, error) {
	want := base64.RawURLEncoding.EncodeToString(credentialID)
	err := s.immediateTx(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT credential_id, flagged_at IS NULL FROM user_passkeys
			WHERE tenant_id = ? AND subject = ?`, tenantID, subject)
		if err != nil {
			return fmt.Errorf("passkey delete: read: %w", err)
		}
		found, targetUsable, otherUsable := false, false, 0
		for rows.Next() {
			var id string
			var usable bool
			if err := rows.Scan(&id, &usable); err != nil {
				_ = rows.Close()
				return err
			}
			switch {
			case id == want:
				found, targetUsable = true, usable
			case usable:
				otherUsable++
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !found {
			return errNoChange
		}
		if targetUsable && otherUsable == 0 {
			return errLastPasskey
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM user_passkeys WHERE tenant_id = ? AND subject = ? AND credential_id = ?`,
			tenantID, subject, want); err != nil {
			return fmt.Errorf("passkey delete: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoChange):
		return false, false, nil
	case errors.Is(err, errLastPasskey):
		return false, true, nil
	case err != nil:
		return false, false, err
	}
	return true, false, nil
}
