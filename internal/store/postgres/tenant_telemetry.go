package postgres

// tenant_telemetry.go — per-tenant telemetry destinations (migrations 0133,
// 0134).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

const tenantTelemetryCols = `tenant_id, endpoint, protocol, headers_credential, signals, sampler_ratio, content_capture, content_max_chars, updated_at, updated_by`

func scanTenantTelemetry(row pgx.Row) (store.TenantTelemetryPolicyRow, error) {
	var (
		out     store.TenantTelemetryPolicyRow
		signals string
	)
	if err := row.Scan(&out.TenantID, &out.Endpoint, &out.Protocol, &out.HeadersCredential, &signals, &out.SamplerRatio, &out.ContentCapture, &out.ContentMaxChars, &out.UpdatedAt, &out.UpdatedBy); err != nil {
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
		INSERT INTO tenant_telemetry_policies (`+tenantTelemetryCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id) DO UPDATE SET endpoint = EXCLUDED.endpoint, protocol = EXCLUDED.protocol,
			headers_credential = EXCLUDED.headers_credential, signals = EXCLUDED.signals,
			sampler_ratio = EXCLUDED.sampler_ratio, content_capture = EXCLUDED.content_capture,
			content_max_chars = EXCLUDED.content_max_chars, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		row.TenantID, row.Endpoint, row.Protocol, row.HeadersCredential, string(sj), row.SamplerRatio,
		store.StoredContentCapture(row.ContentCapture), row.ContentMaxChars, gateStamp(row.UpdatedAt).UTC(), row.UpdatedBy); err != nil {
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

// TenantTelemetryContentExportMark implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryContentExportMark(ctx context.Context, tenantID string, at time.Time) error {
	if tenantID == "" {
		return fmt.Errorf("tenant_telemetry_content_export: tenant_id required")
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_telemetry_content_exports (tenant_id, first_at, last_at) VALUES ($1, $2, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET
			first_at = LEAST(tenant_telemetry_content_exports.first_at, EXCLUDED.first_at),
			last_at = GREATEST(tenant_telemetry_content_exports.last_at, EXCLUDED.last_at)`,
		tenantID, at.UTC()); err != nil {
		return fmt.Errorf("tenant_telemetry_content_export mark: %w", err)
	}
	return nil
}

// TenantTelemetryContentExportGet implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryContentExportGet(ctx context.Context, tenantID string) (store.TenantTelemetryContentExport, error) {
	out := store.TenantTelemetryContentExport{TenantID: tenantID}
	err := s.pool.QueryRow(ctx,
		`SELECT first_at, last_at FROM tenant_telemetry_content_exports WHERE tenant_id = $1`, tenantID).Scan(&out.FirstAt, &out.LastAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TenantTelemetryContentExport{}, &store.ErrNotFound{Kind: "tenant_telemetry_content_export", ID: tenantID}
	}
	if err != nil {
		return store.TenantTelemetryContentExport{}, fmt.Errorf("tenant_telemetry_content_export get: %w", err)
	}
	out.FirstAt, out.LastAt = out.FirstAt.UTC(), out.LastAt.UTC()
	return out, nil
}
