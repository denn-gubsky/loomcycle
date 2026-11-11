package postgres

// tenant_telemetry.go — per-tenant telemetry destinations (migration 0133).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

const tenantTelemetryCols = `tenant_id, endpoint, protocol, headers_credential, signals, sampler_ratio, updated_at, updated_by`

func scanTenantTelemetry(row pgx.Row) (store.TenantTelemetryPolicyRow, error) {
	var (
		out     store.TenantTelemetryPolicyRow
		signals string
	)
	if err := row.Scan(&out.TenantID, &out.Endpoint, &out.Protocol, &out.HeadersCredential, &signals, &out.SamplerRatio, &out.UpdatedAt, &out.UpdatedBy); err != nil {
		return store.TenantTelemetryPolicyRow{}, err
	}
	if err := json.Unmarshal([]byte(signals), &out.Signals); err != nil {
		return store.TenantTelemetryPolicyRow{}, fmt.Errorf("tenant_telemetry_policy: decode signals: %w", err)
	}
	out.UpdatedAt = out.UpdatedAt.UTC()
	return out, nil
}

// TenantTelemetryPolicyGet implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyGet(ctx context.Context, tenantID string) (store.TenantTelemetryPolicyRow, error) {
	row, err := scanTenantTelemetry(s.pool.QueryRow(ctx,
		`SELECT `+tenantTelemetryCols+` FROM tenant_telemetry_policies WHERE tenant_id = $1`, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TenantTelemetryPolicyRow{}, &store.ErrNotFound{Kind: "tenant_telemetry_policy", ID: tenantID}
	}
	if err != nil {
		return store.TenantTelemetryPolicyRow{}, fmt.Errorf("tenant_telemetry_policy get: %w", err)
	}
	return row, nil
}

// TenantTelemetryPolicyList implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyList(ctx context.Context) ([]store.TenantTelemetryPolicyRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tenantTelemetryCols+` FROM tenant_telemetry_policies ORDER BY tenant_id`)
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
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_telemetry_policies (`+tenantTelemetryCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id) DO UPDATE SET endpoint = EXCLUDED.endpoint, protocol = EXCLUDED.protocol,
			headers_credential = EXCLUDED.headers_credential, signals = EXCLUDED.signals,
			sampler_ratio = EXCLUDED.sampler_ratio, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		row.TenantID, row.Endpoint, row.Protocol, row.HeadersCredential, string(sj), row.SamplerRatio,
		gateStamp(row.UpdatedAt).UTC(), row.UpdatedBy); err != nil {
		return fmt.Errorf("tenant_telemetry_policy put: %w", err)
	}
	return nil
}

// TenantTelemetryPolicyDelete implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryPolicyDelete(ctx context.Context, tenantID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tenant_telemetry_policies WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return false, fmt.Errorf("tenant_telemetry_policy delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
