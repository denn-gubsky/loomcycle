package sqlite

// tenant_telemetry.go — per-tenant telemetry destinations. Mirrors postgres
// migrations 0133 and 0134; times are unix nanos, signals a JSON array.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

const tenantTelemetryCols = `tenant_id, endpoint, protocol, headers_credential, signals, sampler_ratio, content_capture, content_max_chars, updated_at, updated_by`

func scanTenantTelemetry(sc deviceAuthScanner) (store.TenantTelemetryPolicyRow, error) {
	var (
		row     store.TenantTelemetryPolicyRow
		signals string
		updated int64
	)
	if err := sc.Scan(&row.TenantID, &row.Endpoint, &row.Protocol, &row.HeadersCredential, &signals, &row.SamplerRatio, &row.ContentCapture, &row.ContentMaxChars, &updated, &row.UpdatedBy); err != nil {
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
		INSERT INTO tenant_telemetry_policies (`+tenantTelemetryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET endpoint = excluded.endpoint, protocol = excluded.protocol,
			headers_credential = excluded.headers_credential, signals = excluded.signals,
			sampler_ratio = excluded.sampler_ratio, content_capture = excluded.content_capture,
			content_max_chars = excluded.content_max_chars, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		row.TenantID, row.Endpoint, row.Protocol, row.HeadersCredential, string(sj), row.SamplerRatio,
		store.StoredContentCapture(row.ContentCapture), row.ContentMaxChars, gateStamp(row.UpdatedAt).UnixNano(), row.UpdatedBy); err != nil {
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

// TenantTelemetryContentExportMark implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryContentExportMark(ctx context.Context, tenantID string, at time.Time) error {
	if tenantID == "" {
		return fmt.Errorf("tenant_telemetry_content_export: tenant_id required")
	}
	ns := at.UTC().UnixNano()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_telemetry_content_exports (tenant_id, first_at, last_at) VALUES (?, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET
			first_at = MIN(tenant_telemetry_content_exports.first_at, excluded.first_at),
			last_at = MAX(tenant_telemetry_content_exports.last_at, excluded.last_at)`,
		tenantID, ns, ns); err != nil {
		return fmt.Errorf("tenant_telemetry_content_export mark: %w", err)
	}
	return nil
}

// TenantTelemetryContentExportGet implements store.TenantTelemetryPolicyStore.
func (s *Store) TenantTelemetryContentExportGet(ctx context.Context, tenantID string) (store.TenantTelemetryContentExport, error) {
	var first, last int64
	err := s.db.QueryRowContext(ctx,
		`SELECT first_at, last_at FROM tenant_telemetry_content_exports WHERE tenant_id = ?`, tenantID).Scan(&first, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return store.TenantTelemetryContentExport{}, &store.ErrNotFound{Kind: "tenant_telemetry_content_export", ID: tenantID}
	}
	if err != nil {
		return store.TenantTelemetryContentExport{}, fmt.Errorf("tenant_telemetry_content_export get: %w", err)
	}
	return store.TenantTelemetryContentExport{TenantID: tenantID, FirstAt: time.Unix(0, first).UTC(), LastAt: time.Unix(0, last).UTC()}, nil
}
