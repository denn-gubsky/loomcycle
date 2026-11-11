package sqlite

// tenant_telemetry.go — per-tenant telemetry destinations. Mirrors postgres
// migration 0133; times are unix nanos, signals a JSON array.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

const tenantTelemetryCols = `tenant_id, endpoint, protocol, headers_credential, signals, sampler_ratio, updated_at, updated_by`

func scanTenantTelemetry(sc deviceAuthScanner) (store.TenantTelemetryPolicyRow, error) {
	var (
		row     store.TenantTelemetryPolicyRow
		signals string
		updated int64
	)
	if err := sc.Scan(&row.TenantID, &row.Endpoint, &row.Protocol, &row.HeadersCredential, &signals, &row.SamplerRatio, &updated, &row.UpdatedBy); err != nil {
		return store.TenantTelemetryPolicyRow{}, err
	}
	if err := json.Unmarshal([]byte(signals), &row.Signals); err != nil {
		return store.TenantTelemetryPolicyRow{}, fmt.Errorf("tenant_telemetry_policy: decode signals: %w", err)
	}
	row.UpdatedAt = time.Unix(0, updated).UTC()
	return row, nil
}

// TenantTelemetryPolicyGet implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyGet(ctx context.Context, tenantID string) (store.TenantTelemetryPolicyRow, error) {
	row, err := scanTenantTelemetry(s.db.QueryRowContext(ctx,
		`SELECT `+tenantTelemetryCols+` FROM tenant_telemetry_policies WHERE tenant_id = ?`, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.TenantTelemetryPolicyRow{}, &store.ErrNotFound{Kind: "tenant_telemetry_policy", ID: tenantID}
	}
	if err != nil {
		return store.TenantTelemetryPolicyRow{}, fmt.Errorf("tenant_telemetry_policy get: %w", err)
	}
	return row, nil
}

// TenantTelemetryPolicyList implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyList(ctx context.Context) ([]store.TenantTelemetryPolicyRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tenantTelemetryCols+` FROM tenant_telemetry_policies ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("tenant_telemetry_policy list: %w", err)
	}
	defer rows.Close()
	out := []store.TenantTelemetryPolicyRow{}
	for rows.Next() {
		row, err := scanTenantTelemetry(rows)
		if err != nil {
			return nil, fmt.Errorf("tenant_telemetry_policy list: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// TenantTelemetryPolicyPut implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyPut(ctx context.Context, row store.TenantTelemetryPolicyRow) error {
	if err := store.CheckTenantTelemetryPolicyRow(row); err != nil {
		return err
	}
	signals, err := store.NormalizeTelemetrySignals(row.Signals)
	if err != nil {
		return err
	}
	sj, err := json.Marshal(signals)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_telemetry_policies (`+tenantTelemetryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET endpoint = excluded.endpoint, protocol = excluded.protocol,
			headers_credential = excluded.headers_credential, signals = excluded.signals,
			sampler_ratio = excluded.sampler_ratio, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		row.TenantID, row.Endpoint, row.Protocol, row.HeadersCredential, string(sj), row.SamplerRatio,
		gateStamp(row.UpdatedAt).UnixNano(), row.UpdatedBy); err != nil {
		return fmt.Errorf("tenant_telemetry_policy put: %w", err)
	}
	return nil
}

// TenantTelemetryPolicyDelete implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyDelete(ctx context.Context, tenantID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tenant_telemetry_policies WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return false, fmt.Errorf("tenant_telemetry_policy delete: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
