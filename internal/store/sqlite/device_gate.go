package sqlite

// device_gate.go — RFC FA P5 device authorizations and per-tenant gate
// policies. Mirrors postgres migration 0117; times are unix nanos.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

const deviceAuthCols = `device_code_hash, user_code_hash, client_id, source_addr, created_at, expires_at,
	interval_ms, last_polled_at, status, decided_tenant, decided_subject, decided_gate, decided_at, redeemed_at`

type deviceAuthScanner interface{ Scan(dest ...any) error }

func scanDeviceAuth(sc deviceAuthScanner, extra ...any) (store.DeviceAuthRow, error) {
	var (
		row                     store.DeviceAuthRow
		created, expires        int64
		intervalMS              int64
		polled, decided, redeem sql.NullInt64
	)
	dest := []any{&row.DeviceCodeHash, &row.UserCodeHash, &row.ClientID, &row.SourceAddr,
		&created, &expires, &intervalMS, &polled, &row.Status, &row.DecidedTenant,
		&row.DecidedSubject, &row.DecidedGate, &decided, &redeem}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return store.DeviceAuthRow{}, err
	}
	row.CreatedAt, row.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	row.Interval = time.Duration(intervalMS) * time.Millisecond
	row.LastPolledAt, row.DecidedAt, row.RedeemedAt = gateTime(polled), gateTime(decided), gateTime(redeem)
	return row, nil
}

// DeviceAuthCreate implements store.DeviceAuthStore.
func (s *Store) DeviceAuthCreate(ctx context.Context, row store.DeviceAuthRow) error {
	if row.DeviceCodeHash == "" || row.UserCodeHash == "" || row.ClientID == "" || row.ExpiresAt.IsZero() || row.Interval <= 0 {
		return fmt.Errorf("device_auth: device_code_hash + user_code_hash + client_id + expires_at + interval required")
	}
	status := row.Status
	if status == "" {
		status = store.DeviceAuthPending
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO device_authorizations (`+deviceAuthCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DeviceCodeHash, row.UserCodeHash, row.ClientID, row.SourceAddr,
		gateStamp(row.CreatedAt).UnixNano(), row.ExpiresAt.UnixNano(), row.Interval.Milliseconds(), gateNanos(row.LastPolledAt),
		status, row.DecidedTenant, row.DecidedSubject, row.DecidedGate, gateNanos(row.DecidedAt), gateNanos(row.RedeemedAt))
	if err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "device_auth", ID: "<hash>"}
		}
		return fmt.Errorf("device_auth create: %w", err)
	}
	return nil
}

// DeviceAuthGetByUserCode implements store.DeviceAuthStore.
func (s *Store) DeviceAuthGetByUserCode(ctx context.Context, userCodeHash string) (store.DeviceAuthRow, error) {
	row, err := scanDeviceAuth(s.db.QueryRowContext(ctx,
		`SELECT `+deviceAuthCols+` FROM device_authorizations WHERE user_code_hash = ?`, userCodeHash))
	if errors.Is(err, sql.ErrNoRows) {
		return store.DeviceAuthRow{}, &store.ErrNotFound{Kind: "device_auth", ID: "<hash>"}
	}
	if err != nil {
		return store.DeviceAuthRow{}, fmt.Errorf("device_auth get: %w", err)
	}
	return row, nil
}

// DeviceAuthPoll implements store.DeviceAuthStore. Every SET expression reads
// the row as it was before the update, so "early" is judged once against the
// previous poll and both columns agree on it.
func (s *Store) DeviceAuthPoll(ctx context.Context, deviceCodeHash string, now time.Time, step, max time.Duration) (store.DeviceAuthRow, bool, error) {
	var early int
	row, err := scanDeviceAuth(s.db.QueryRowContext(ctx, `
		UPDATE device_authorizations SET
			last_poll_early = CASE WHEN last_polled_at IS NOT NULL AND ?2 < last_polled_at + interval_ms * 1000000
				THEN 1 ELSE 0 END,
			interval_ms = CASE WHEN last_polled_at IS NOT NULL AND ?2 < last_polled_at + interval_ms * 1000000
				THEN MIN(interval_ms + ?3, ?4) ELSE interval_ms END,
			last_polled_at = ?2
		WHERE device_code_hash = ?1
		RETURNING `+deviceAuthCols+`, last_poll_early`,
		deviceCodeHash, now.UnixNano(), step.Milliseconds(), max.Milliseconds()), &early)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DeviceAuthRow{}, false, &store.ErrNotFound{Kind: "device_auth", ID: "<hash>"}
	}
	if err != nil {
		return store.DeviceAuthRow{}, false, fmt.Errorf("device_auth poll: %w", err)
	}
	return row, early == 1, nil
}

// DeviceAuthDecide implements store.DeviceAuthStore.
func (s *Store) DeviceAuthDecide(ctx context.Context, userCodeHash string, d store.DeviceAuthDecision) (bool, error) {
	status := store.DeviceAuthDenied
	if d.Approve {
		status = store.DeviceAuthApproved
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE device_authorizations SET status = ?2, decided_tenant = ?3, decided_subject = ?4, decided_gate = ?5,
			decided_at = ?6
		WHERE user_code_hash = ?1 AND status = 'pending' AND expires_at > ?6`,
		userCodeHash, status, d.TenantID, d.Subject, d.Gate, d.At.UnixNano())
	if err != nil {
		return false, fmt.Errorf("device_auth decide: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeviceAuthRedeem implements store.DeviceAuthStore.
func (s *Store) DeviceAuthRedeem(ctx context.Context, deviceCodeHash string, at time.Time) (store.DeviceAuthRow, bool, error) {
	row, err := scanDeviceAuth(s.db.QueryRowContext(ctx, `
		UPDATE device_authorizations SET status = 'redeemed', redeemed_at = ?2
		WHERE device_code_hash = ?1 AND status = 'approved' AND expires_at > ?2
		RETURNING `+deviceAuthCols, deviceCodeHash, at.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return store.DeviceAuthRow{}, false, nil
	}
	if err != nil {
		return store.DeviceAuthRow{}, false, fmt.Errorf("device_auth redeem: %w", err)
	}
	return row, true, nil
}

// DeviceAuthDeleteExpired implements store.DeviceAuthStore.
func (s *Store) DeviceAuthDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM device_authorizations WHERE expires_at <= ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("device_auth sweep: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- per-tenant gate policies ----------------------------------------------

func normalizePolicyGates(gates []string) ([]string, error) {
	out := append([]string(nil), gates...)
	sort.Strings(out)
	n := 0
	for i, g := range out {
		if g == "" {
			return nil, fmt.Errorf("tenant_auth_policy: empty gate name")
		}
		if i > 0 && g == out[i-1] {
			continue
		}
		out[n] = g
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("tenant_auth_policy: at least one gate required")
	}
	return out[:n], nil
}

func scanTenantPolicy(sc deviceAuthScanner) (store.TenantAuthPolicyRow, error) {
	var (
		row     store.TenantAuthPolicyRow
		gates   string
		updated int64
	)
	if err := sc.Scan(&row.TenantID, &gates, &updated, &row.UpdatedBy); err != nil {
		return store.TenantAuthPolicyRow{}, err
	}
	if err := json.Unmarshal([]byte(gates), &row.Gates); err != nil {
		return store.TenantAuthPolicyRow{}, fmt.Errorf("tenant_auth_policy: decode gates: %w", err)
	}
	row.UpdatedAt = time.Unix(0, updated).UTC()
	return row, nil
}

// TenantAuthPolicyGet implements store.TenantAuthPolicyStore.
func (s *Store) TenantAuthPolicyGet(ctx context.Context, tenantID string) (store.TenantAuthPolicyRow, error) {
	row, err := scanTenantPolicy(s.db.QueryRowContext(ctx,
		`SELECT tenant_id, gates, updated_at, updated_by FROM tenant_auth_policies WHERE tenant_id = ?`, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.TenantAuthPolicyRow{}, &store.ErrNotFound{Kind: "tenant_auth_policy", ID: tenantID}
	}
	if err != nil {
		return store.TenantAuthPolicyRow{}, fmt.Errorf("tenant_auth_policy get: %w", err)
	}
	return row, nil
}

// TenantAuthPolicyList implements store.TenantAuthPolicyStore.
func (s *Store) TenantAuthPolicyList(ctx context.Context) ([]store.TenantAuthPolicyRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, gates, updated_at, updated_by FROM tenant_auth_policies ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("tenant_auth_policy list: %w", err)
	}
	defer rows.Close()
	out := []store.TenantAuthPolicyRow{}
	for rows.Next() {
		row, err := scanTenantPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("tenant_auth_policy list: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// TenantAuthPolicyPut implements store.TenantAuthPolicyStore.
func (s *Store) TenantAuthPolicyPut(ctx context.Context, row store.TenantAuthPolicyRow) error {
	if row.TenantID == "" {
		return fmt.Errorf("tenant_auth_policy: tenant_id required")
	}
	gates, err := normalizePolicyGates(row.Gates)
	if err != nil {
		return err
	}
	gj, err := json.Marshal(gates)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_auth_policies (tenant_id, gates, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET gates = excluded.gates, updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		row.TenantID, string(gj), gateStamp(row.UpdatedAt).UnixNano(), row.UpdatedBy); err != nil {
		return fmt.Errorf("tenant_auth_policy put: %w", err)
	}
	return nil
}

// TenantAuthPolicyDelete implements store.TenantAuthPolicyStore.
func (s *Store) TenantAuthPolicyDelete(ctx context.Context, tenantID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tenant_auth_policies WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return false, fmt.Errorf("tenant_auth_policy delete: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TenantAuthPolicyRevokeSessions implements store.TenantAuthPolicyStore.
func (s *Store) TenantAuthPolicyRevokeSessions(ctx context.Context, tenantID string, gates []string, at time.Time) (int, error) {
	q := `UPDATE ui_sessions SET revoked_at = ? WHERE tenant_id = ? AND revoked_at IS NULL`
	args := []any{at.UnixNano(), tenantID}
	if len(gates) > 0 {
		q += ` AND gate NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(gates)), ",") + `)`
		for _, g := range gates {
			args = append(args, g)
		}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("tenant_auth_policy revoke sessions: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
