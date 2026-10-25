package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Web UI login sessions (RFC FA §5); see migration 0110. revoked_at NULL means
// the session has not been ended.

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
	var authTime *time.Time
	if !row.AuthTime.IsZero() {
		t := row.AuthTime.UTC()
		authTime = &t
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ui_sessions (session_hash, tenant_id, subject, gate, amr, cred_kind, cred_ref, cred_mac,
			ua_hash, ip_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at, revoked_at, auth_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NULL, $15)`,
		row.SessionHash, row.TenantID, row.Subject, row.Gate, string(amr), row.CredKind, row.CredRef, row.CredMAC,
		row.UAHash, row.IPHash, row.CreatedAt.UTC(), row.LastSeenAt.UTC(),
		row.IdleExpiresAt.UTC(), row.AbsoluteExpiresAt.UTC(), authTime,
	); err != nil {
		return fmt.Errorf("ui_session insert: %w", err)
	}
	return nil
}

func (s *Store) UISessionGet(ctx context.Context, sessionHash string) (store.UISessionRow, error) {
	var (
		row      store.UISessionRow
		amr      string
		revoked  *time.Time
		authTime *time.Time
	)
	err := s.pool.QueryRow(ctx, uiSessionSelect+` WHERE session_hash = $1`, sessionHash).Scan(
		&row.SessionHash, &row.TenantID, &row.Subject, &row.Gate, &amr, &row.CredKind, &row.CredRef,
		&row.CredMAC, &row.UAHash, &row.IPHash, &row.CreatedAt, &row.LastSeenAt, &row.IdleExpiresAt,
		&row.AbsoluteExpiresAt, &revoked, &authTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.UISessionRow{}, &store.ErrNotFound{Kind: "ui_session", ID: "<hash>"}
	}
	if err != nil {
		return store.UISessionRow{}, err
	}
	if err := json.Unmarshal([]byte(amr), &row.AMR); err != nil {
		return store.UISessionRow{}, fmt.Errorf("ui_session: decode amr: %w", err)
	}
	if revoked != nil {
		row.RevokedAt = *revoked
	}
	if authTime != nil {
		row.AuthTime = *authTime
	}
	return row, nil
}

func (s *Store) UISessionTouch(ctx context.Context, sessionHash string, lastSeen, idleExpires time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE ui_sessions SET last_seen_at = $1, idle_expires_at = $2 WHERE session_hash = $3 AND revoked_at IS NULL`,
		lastSeen.UTC(), idleExpires.UTC(), sessionHash)
	return err
}

func (s *Store) UISessionRevoke(ctx context.Context, sessionHash string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE ui_sessions SET revoked_at = $1 WHERE session_hash = $2 AND revoked_at IS NULL`,
		at.UTC(), sessionHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) UISessionDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM ui_sessions WHERE revoked_at IS NOT NULL OR idle_expires_at <= $1 OR absolute_expires_at <= $1`,
		before.UTC())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
