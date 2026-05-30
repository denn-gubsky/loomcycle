package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryBackendStore is the subset of store.Store the memory-backend
// resolver uses. Declared here so tests + callers can mock without
// depending on the full store interface. RFC I MR-3a / mirrors
// WebhookStore.
type MemoryBackendStore interface {
	MemoryBackendDefGetActive(ctx context.Context, name string) (store.MemoryBackendDefRow, error)
}

// MemoryBackend resolves a memory-backend NAME to its effective
// config.MemoryBackend by walking the lookup chain in precedence order:
//
//  1. static cfg.MemoryBackends (yaml-defined, pre-validated at boot)
//  2. memory_backend_def_active + memory_backend_defs (substrate path)
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false) — defensive against
// future-field churn or hand-edited rows.
//
// RFC I MR-3a / mirrors lookup.Webhook. Nothing consumes this yet — the
// per-agent routing + factory land in MR-3b.
func MemoryBackend(ctx context.Context, s MemoryBackendStore, cfg *config.Config, name string) (config.MemoryBackend, bool) {
	if cfg != nil {
		if mb, ok := cfg.MemoryBackends[name]; ok {
			return mb, true
		}
	}
	if s == nil {
		return config.MemoryBackend{}, false
	}
	activeRow, err := s.MemoryBackendDefGetActive(ctx, name)
	if err != nil {
		return config.MemoryBackend{}, false
	}
	var md SubstrateMemoryBackendDef
	if uerr := json.Unmarshal(activeRow.Definition, &md); uerr != nil {
		return config.MemoryBackend{}, false
	}
	return md.ToConfigDef(), true
}

// SubstrateMemoryBackendDef mirrors the JSON shape `MemoryBackendDef`
// create/fork persists in `memory_backend_defs.definition` (snake_case
// JSON tags via the `mergedMemoryBackendDef` adapter in
// internal/tools/builtin/memorybackenddef.go). The runtime consumer
// (`config.MemoryBackend`) carries yaml tags for the operator-yaml
// path; this adapter is the substrate-read seam.
//
// Kept in sync with `mergedMemoryBackendDef`; the builtin-package drift
// test TestMergedMemoryBackendDef_DriftDetection_VsLookupSubstrate pins
// merged↔substrate parity. The complementary assertion here
// (memory_backend_test.go TestMemoryBackend_DriftDetection) pins this
// shape against an explicit expected field-set, mirroring the
// WebhookDef resolver. RFC I MR-3a.
type SubstrateMemoryBackendDef struct {
	Name                       string                        `json:"name,omitempty"`
	Kind                       string                        `json:"kind,omitempty"`
	Config                     SubstrateMemoryBackendConfig  `json:"config,omitempty"`
	TenancyStrategy            SubstrateMemoryBackendTenancy `json:"tenancy_strategy,omitempty"`
	FallbackOnError            string                        `json:"fallback_on_error,omitempty"`
	HealthCheckIntervalSeconds int                           `json:"health_check_interval_seconds,omitempty"`
}

// SubstrateMemoryBackendConfig mirrors config.MemoryBackendConfig.
type SubstrateMemoryBackendConfig struct {
	BaseURL    string `json:"base_url,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
}

// SubstrateMemoryBackendTenancy mirrors config.MemoryBackendTenancy.
type SubstrateMemoryBackendTenancy struct {
	Kind          string `json:"kind,omitempty"`
	EnvPattern    string `json:"env_pattern,omitempty"`
	PrefixPattern string `json:"prefix_pattern,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto
// config.MemoryBackend for the runtime to consume. Pure data shuffling.
func (s SubstrateMemoryBackendDef) ToConfigDef() config.MemoryBackend {
	return config.MemoryBackend{
		Name: s.Name,
		Kind: s.Kind,
		Config: config.MemoryBackendConfig{
			BaseURL:    s.Config.BaseURL,
			APIVersion: s.Config.APIVersion,
			APIKeyEnv:  s.Config.APIKeyEnv,
		},
		TenancyStrategy: config.MemoryBackendTenancy{
			Kind:          s.TenancyStrategy.Kind,
			EnvPattern:    s.TenancyStrategy.EnvPattern,
			PrefixPattern: s.TenancyStrategy.PrefixPattern,
		},
		FallbackOnError:            s.FallbackOnError,
		HealthCheckIntervalSeconds: s.HealthCheckIntervalSeconds,
	}
}
