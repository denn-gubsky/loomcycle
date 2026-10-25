package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Web UI login sessions (RFC FA §5). Every time is unix nanos; revoked_at NULL
// means the session has not been ended.

const uiSessionSelect = `SELECT session_hash, tenant_id, subject, gate, amr, cred_kind, cred_ref,
	cred_mac, ua_hash, ip_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at,
	revoked_at, auth_time FROM ui_sessions`

func (s *Store) UISessionCreate(ctx context.Context, row store.UISessionRow) error {
	if row.SessionHash == "" || row.TenantID == "" || row.Subject == "" || row.Gate == "" || row.CredKind == "" ||
		row.CredMAC == "" || row.IdleExpiresAt.IsZero() || row.AbsoluteExpiresAt.IsZero() {
		return fmt.Errorf("ui_session: session_hash + tenant_id + subject + gate + cred_kind + cred_mac + expiry required")
	}
	if row.AMR == nil {
		row.AMR = []string{}
	}
	amr, err := json.Marshal(row.AMR)
	if err != nil {
		return fmt.Errorf("ui_session: encode amr: %w", err)
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	if row.LastSeenAt.IsZero() {
		row.LastSeenAt = row.CreatedAt
	}
	var authTime any
	if !row.AuthTime.IsZero() {
		authTime = row.AuthTime.UnixNano()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ui_sessions (session_hash, tenant_id, subject, gate, amr, cred_kind, cred_ref, cred_mac,
			ua_hash, ip_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at, revoked_at, auth_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		row.SessionHash, row.TenantID, row.Subject, row.Gate, string(amr), row.CredKind, row.CredRef, row.CredMAC,
		row.UAHash, row.IPHash, row.CreatedAt.UnixNano(), row.LastSeenAt.UnixNano(),
		row.IdleExpiresAt.UnixNano(), row.AbsoluteExpiresAt.UnixNano(), authTime,
	); err != nil {
		return fmt.Errorf("ui_session insert: %w", err)
	}
	return nil
}

func (s *Store) UISessionGet(ctx context.Context, sessionHash string) (store.UISessionRow, error) {
	var (
		row                           store.UISessionRow
		amr                           string
		created, seen, idle, absolute int64
		revoked, authTime             sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, uiSessionSelect+` WHERE session_hash = ?`, sessionHash).Scan(
		&row.SessionHash, &row.TenantID, &row.Subject, &row.Gate, &amr, &row.CredKind, &row.CredRef,
		&row.CredMAC, &row.UAHash, &row.IPHash, &created, &seen, &idle, &absolute, &revoked, &authTime)
	if errors.Is(err, sql.ErrNoRows) {
		return store.UISessionRow{}, &store.ErrNotFound{Kind: "ui_session", ID: "<hash>"}
	}
	if err != nil {
		return store.UISessionRow{}, err
	}
	if err := json.Unmarshal([]byte(amr), &row.AMR); err != nil {
		return store.UISessionRow{}, fmt.Errorf("ui_session: decode amr: %w", err)
	}
	row.CreatedAt = time.Unix(0, created)
	row.LastSeenAt = time.Unix(0, seen)
	row.IdleExpiresAt = time.Unix(0, idle)
	row.AbsoluteExpiresAt = time.Unix(0, absolute)
	if revoked.Valid {
		row.RevokedAt = time.Unix(0, revoked.Int64)
	}
	if authTime.Valid {
		row.AuthTime = time.Unix(0, authTime.Int64)
	}
	return row, nil
}

func (s *Store) UISessionTouch(ctx context.Context, sessionHash string, lastSeen, idleExpires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ui_sessions SET last_seen_at = ?, idle_expires_at = ? WHERE session_hash = ? AND revoked_at IS NULL`,
		lastSeen.UnixNano(), idleExpires.UnixNano(), sessionHash)
	return err
}

func (s *Store) UISessionRevoke(ctx context.Context, sessionHash string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = ? WHERE session_hash = ? AND revoked_at IS NULL`,
		at.UnixNano(), sessionHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) UISessionDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	t := before.UnixNano()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ui_sessions WHERE revoked_at IS NOT NULL OR idle_expires_at <= ? OR absolute_expires_at <= ?`, t, t)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
