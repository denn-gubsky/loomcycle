package snapshot

// The tenant_telemetry section: where each tenant's own traces are exported.
// The policy names its header credential, never the header values; the
// credential itself (sealed under the source's key) never travels, so a
// restored policy exports only once the tenant stores the credential again on
// the target. Whether it exports at all is still the target's decision: the
// runtime applies the target operator's rules (https, allowed hosts, ratio
// cap) to every stored policy before exporting.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TenantTelemetrySection carries every tenant telemetry policy.
type TenantTelemetrySection struct {
	Version string                 `json:"version"`
	Entries []TenantTelemetryEntry `json:"entries"`
}

// TenantTelemetryEntry mirrors a tenant_telemetry_policies row.
type TenantTelemetryEntry struct {
	TenantID          string    `json:"tenant_id"`
	Endpoint          string    `json:"endpoint"`
	Protocol          string    `json:"protocol"`
	HeadersCredential string    `json:"headers_credential,omitempty"`
	Signals           []string  `json:"signals"`
	SamplerRatio      float64   `json:"sampler_ratio"`
	UpdatedAt         time.Time `json:"updated_at"`
	UpdatedBy         string    `json:"updated_by,omitempty"`

	// ContentCapture is "on" when the tenant captured content; absent for
	// off, so a policy without capture is carried as it was before the
	// field existed. The target's operator gate still decides whether a
	// restored "on" captures anything there.
	ContentCapture  string `json:"content_capture,omitempty"`
	ContentMaxChars int    `json:"content_max_chars,omitempty"`
}

// captureTenantTelemetry reads every policy. out stays nil when there are
// none, so a capture without any is byte-identical to one taken before the
// section existed.
func captureTenantTelemetry(ctx context.Context, s store.Store, out **TenantTelemetrySection) error {
	rows, err := s.TenantTelemetryPolicyList(ctx)
	if err != nil {
		return fmt.Errorf("snapshot tenant_telemetry: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TenantID < rows[j].TenantID })
	sec := &TenantTelemetrySection{Version: SectionVersion, Entries: make([]TenantTelemetryEntry, 0, len(rows))}
	for _, r := range rows {
		sec.Entries = append(sec.Entries, TenantTelemetryEntry{
			TenantID: r.TenantID, Endpoint: r.Endpoint, Protocol: r.Protocol, HeadersCredential: r.HeadersCredential,
			Signals: append([]string{}, r.Signals...), SamplerRatio: r.SamplerRatio,
			ContentMaxChars: r.ContentMaxChars, UpdatedAt: r.UpdatedAt.UTC(), UpdatedBy: r.UpdatedBy,
		})
		if r.ContentCapture == "on" {
			sec.Entries[len(sec.Entries)-1].ContentCapture = r.ContentCapture
		}
	}
	*out = sec
	return nil
}

// restoreTenantTelemetry writes each policy the target does not hold and its
// operator's rules allow (validate: the rules a set is held to on this host).
// A policy already there is the target's newer word and stands; a re-restore
// writes nothing. Without a validator nothing is restored: a policy sends a
// tenant's traces to a host, which only the target's rules may admit. The
// restored tenants are named in the refresh, so the runtime applies their
// policies at once rather than at its next re-read.
func restoreTenantTelemetry(ctx context.Context, s store.Store, sec *TenantTelemetrySection, validate func(json.RawMessage) error, result *RestoreResult) {
	if validate == nil {
		if len(sec.Entries) > 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"tenant_telemetry: %d polic(ies) skipped: nothing on this host can check them against its rules", len(sec.Entries)))
		}
		return
	}
	for _, e := range sec.Entries {
		where := "tenant telemetry policy " + e.TenantID
		body, err := json.Marshal(e)
		if err == nil {
			err = validate(body)
		}
		if err != nil {
			slog.Warn("snapshot: tenant telemetry policy skipped: this host's rules refuse it", "tenant", e.TenantID, "err", err)
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped: %v", where, err))
			continue
		}
		if _, err := s.TenantTelemetryPolicyGet(ctx, e.TenantID); err == nil {
			continue
		} else if nf := (*store.ErrNotFound)(nil); !errors.As(err, &nf) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped: %v", where, err))
			continue
		}
		if err := s.TenantTelemetryPolicyPut(ctx, store.TenantTelemetryPolicyRow{
			TenantID: e.TenantID, Endpoint: e.Endpoint, Protocol: e.Protocol, HeadersCredential: e.HeadersCredential,
			Signals: e.Signals, SamplerRatio: e.SamplerRatio, ContentCapture: e.ContentCapture,
			ContentMaxChars: e.ContentMaxChars, UpdatedAt: e.UpdatedAt, UpdatedBy: e.UpdatedBy,
		}); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped: %v", where, err))
			continue
		}
		result.TenantTelemetryRestored++
		result.Refresh.TelemetryTenants = append(result.Refresh.TelemetryTenants, e.TenantID)
		if e.HeadersCredential != "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: restored; it exports once the tenant credential %q is stored on this host (credentials never travel)", where, e.HeadersCredential))
		}
	}
}

// restoreTenantTelemetrySection restores the tenant_telemetry section (its
// place: restoreSectionOrder).
func restoreTenantTelemetrySection(ctx context.Context, rs *restoreRun) error {
	raw, ok := rs.sections[migrations.SectionTenantTelemetry]
	if !ok {
		return nil
	}
	var sec TenantTelemetrySection
	if err := decodeWithMigration(migrations.SectionTenantTelemetry, raw, &sec); err != nil {
		return err
	}
	restoreTenantTelemetry(ctx, rs.s, &sec, rs.opts.Validators[migrations.SectionTenantTelemetry], rs.result)
	return nil
}
