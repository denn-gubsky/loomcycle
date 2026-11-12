package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// TenantTelemetryPolicyRow is where one tenant's own telemetry is exported: an
// OTLP endpoint of the tenant's, reached with headers the tenant keeps in one
// of its credentials. No row = nothing of the tenant's leaves for a
// destination of its own (the operator's export is separate and unaffected).
//
// The store keeps what it is given; the rules a write must pass (https, the
// operator's host list, a header credential bound to the endpoint, the
// signals and ratio the runtime supports) are checked before Put.
type TenantTelemetryPolicyRow struct {
	TenantID string
	// Endpoint is the OTLP base URL (https://collector.example:4318).
	Endpoint string
	// Protocol is "http/protobuf" or "grpc".
	Protocol string
	// HeadersCredential names a tenant-scope credential whose value is the
	// header block sent with every export; "" for none. The value itself is
	// never stored here.
	HeadersCredential string
	// Signals are what is exported ("traces"), sorted and de-duplicated. Empty
	// keeps the row but exports nothing.
	Signals []string
	// SamplerRatio is the share of the tenant's traces sent, 0..1.
	SamplerRatio float64
	// ContentCapture is "off" or "on": whether the tenant's runs' prompts,
	// answers and tool calls are recorded on the spans it receives. "" is
	// stored as "off".
	ContentCapture string
	// ContentMaxChars is the length each captured field is cut to; 0 for the
	// runtime's default.
	ContentMaxChars int
	UpdatedAt       time.Time
	UpdatedBy       string
}

// TenantTelemetryPolicyStore persists the per-tenant telemetry destinations.
type TenantTelemetryPolicyStore interface {
	// TenantTelemetryPolicyGet returns the tenant's policy.
	// *ErrNotFound{Kind:"tenant_telemetry_policy"} when it has none.
	TenantTelemetryPolicyGet(ctx context.Context, tenantID string) (TenantTelemetryPolicyRow, error)
	// TenantTelemetryPolicyList returns every policy, ordered by tenant. Empty
	// slice when there are none.
	TenantTelemetryPolicyList(ctx context.Context) ([]TenantTelemetryPolicyRow, error)
	// TenantTelemetryPolicyPut inserts or replaces the tenant's policy.
	TenantTelemetryPolicyPut(ctx context.Context, row TenantTelemetryPolicyRow) error
	// TenantTelemetryPolicyDelete removes the tenant's policy and reports
	// whether there was one.
	TenantTelemetryPolicyDelete(ctx context.Context, tenantID string) (bool, error)

	// TenantTelemetryContentExportMark records that the tenant's captured
	// content was exported at at: the first mark sets FirstAt, every mark
	// moves LastAt forward (never back). A policy delete leaves the record:
	// what was exported stays in the tenant's backend.
	TenantTelemetryContentExportMark(ctx context.Context, tenantID string, at time.Time) error
	// TenantTelemetryContentExportGet returns the tenant's record.
	// *ErrNotFound{Kind:"tenant_telemetry_content_export"} when its content
	// was never exported.
	TenantTelemetryContentExportGet(ctx context.Context, tenantID string) (TenantTelemetryContentExport, error)
}

// TenantTelemetryContentExport is when a tenant's captured content was
// exported to its own telemetry destination, to within the runtime's mark
// interval: the record an erasure report cites, since loomcycle cannot erase
// what a tenant's own backend holds.
type TenantTelemetryContentExport struct {
	TenantID string
	FirstAt  time.Time
	LastAt   time.Time
}

// NormalizeTelemetrySignals returns signals sorted and de-duplicated, never
// nil, refusing an empty name. Both backends store what it returns, so a row
// reads back the same from either.
func NormalizeTelemetrySignals(signals []string) ([]string, error) {
	out := append([]string{}, signals...)
	sort.Strings(out)
	n := 0
	for i, s := range out {
		if s == "" {
			return nil, fmt.Errorf("tenant_telemetry_policy: empty signal name")
		}
		if i > 0 && s == out[i-1] {
			continue
		}
		out[n] = s
		n++
	}
	return out[:n], nil
}

// CheckTenantTelemetryPolicyRow is the shape both backends require before a
// write: a tenant, an endpoint, a protocol and a ratio in range.
func CheckTenantTelemetryPolicyRow(row TenantTelemetryPolicyRow) error {
	switch {
	case row.TenantID == "":
		return fmt.Errorf("tenant_telemetry_policy: tenant_id required")
	case row.Endpoint == "":
		return fmt.Errorf("tenant_telemetry_policy: endpoint required")
	case row.Protocol == "":
		return fmt.Errorf("tenant_telemetry_policy: protocol required")
	case !(row.SamplerRatio >= 0 && row.SamplerRatio <= 1):
		return fmt.Errorf("tenant_telemetry_policy: sampler_ratio must be between 0 and 1")
	case row.ContentCapture != "" && row.ContentCapture != "off" && row.ContentCapture != "on":
		return fmt.Errorf("tenant_telemetry_policy: content_capture must be off or on")
	case row.ContentMaxChars < 0:
		return fmt.Errorf("tenant_telemetry_policy: content_max_chars must not be negative")
	}
	return nil
}

// StoredContentCapture is the content_capture value both backends store for
// v: "" is "off".
func StoredContentCapture(v string) string {
	if v == "" {
		return "off"
	}
	return v
}
