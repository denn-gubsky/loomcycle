package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DocumentSourceStore is the subset of store.Store the document-source
// resolver uses. Declared here so tests + callers can mock without depending
// on the full store interface. RFC CE / RFC N: the substrate lookup carries a
// tenantID.
type DocumentSourceStore interface {
	DocumentSourceDefGetActive(ctx context.Context, tenantID, name string) (store.DocumentSourceDefRow, error)
}

// DocumentSource resolves a document-source NAME to its effective
// config.DocumentSource within the caller's tenant, walking the lookup chain in
// precedence order (mirrors lookup.MemoryBackend):
//
//  1. (tenantID != "") tenant-scoped substrate — document_source_def_active
//     WHERE tenant_id=tenantID.
//  2. static cfg.DocumentSources (yaml-defined, pre-validated at boot) — the
//     shared operator base every tenant inherits.
//  3. shared substrate (tenant_id="") — document_source_def_active.
//
// For the default tenant "" step 1 is skipped, so the order collapses to
// static-cfg → shared-substrate — identical to the pre-substrate behavior.
//
// Returns (zero, false) when no source has the name. Malformed persistence JSON
// also returns (zero, false) — defensive against future-field churn or
// hand-edited rows.
func DocumentSource(ctx context.Context, s DocumentSourceStore, cfg *config.Config, tenantID, name string) (config.DocumentSource, bool) {
	// 1. Tenant-scoped substrate (skipped for the shared "" tenant).
	if tenantID != "" {
		if ds, ok := resolveDocumentSourceSubstrate(ctx, s, tenantID, name); ok {
			return ds, true
		}
	}
	// 2. Static cfg.DocumentSources — the shared operator base.
	if cfg != nil {
		if ds, ok := cfg.DocumentSources[name]; ok {
			return ds, true
		}
	}
	// 3. Shared substrate (tenant_id="").
	return resolveDocumentSourceSubstrate(ctx, s, "", name)
}

// resolveDocumentSourceSubstrate reads the document_source_def_active overlay
// for one tenant pass. Returns (zero, false) when the store is nil, the name has
// no active pointer for that tenant, or the row's JSON is malformed.
func resolveDocumentSourceSubstrate(ctx context.Context, s DocumentSourceStore, tenantID, name string) (config.DocumentSource, bool) {
	if s == nil {
		return config.DocumentSource{}, false
	}
	activeRow, err := s.DocumentSourceDefGetActive(ctx, tenantID, name)
	if err != nil {
		return config.DocumentSource{}, false
	}
	// A retired def that is still the active pointer must NOT resolve — retiring
	// the active source deactivates it (set_remote/sync then report "unknown
	// source" until a live version is promoted), rather than silently dialing a
	// peer the operator retired. (The store keeps the pointer for lineage; the
	// resolver is where "retired means unusable" is enforced.)
	if activeRow.Retired {
		return config.DocumentSource{}, false
	}
	var sd SubstrateDocumentSourceDef
	if uerr := json.Unmarshal(activeRow.Definition, &sd); uerr != nil {
		return config.DocumentSource{}, false
	}
	return sd.ToConfigDef(), true
}

// SubstrateDocumentSourceDef mirrors the JSON shape `DocumentSourceDef`
// create/fork persists in `document_source_defs.definition` (snake_case JSON
// tags via the `mergedDocumentSourceDef` adapter in
// internal/tools/builtin/documentsourcedef.go). The runtime consumer
// (`config.DocumentSource`) carries yaml tags for the operator-yaml path; this
// adapter is the substrate-read seam.
//
// Kept in sync with `mergedDocumentSourceDef`; the builtin-package drift test
// TestMergedDocumentSourceDef_DriftDetection_VsLookupSubstrate pins
// merged↔substrate parity. The complementary assertion here
// (document_source_test.go TestDocumentSource_DriftDetection) pins this shape
// against an explicit expected field-set, mirroring the MemoryBackendDef
// resolver.
type SubstrateDocumentSourceDef struct {
	Name            string                         `json:"name,omitempty"`
	Config          SubstrateDocumentSourceConfig  `json:"config,omitempty"`
	TenancyStrategy SubstrateDocumentSourceTenancy `json:"tenancy_strategy,omitempty"`
}

// SubstrateDocumentSourceConfig mirrors config.DocumentSourceConfig.
type SubstrateDocumentSourceConfig struct {
	BaseURL    string `json:"base_url,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
}

// SubstrateDocumentSourceTenancy mirrors config.DocumentSourceTenancy.
type SubstrateDocumentSourceTenancy struct {
	Kind       string `json:"kind,omitempty"`
	EnvPattern string `json:"env_pattern,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto config.DocumentSource for
// the runtime to consume. Pure data shuffling. (config.DocumentSource has no
// Name field — the name is the map key / the def's Name column.)
func (s SubstrateDocumentSourceDef) ToConfigDef() config.DocumentSource {
	return config.DocumentSource{
		Config: config.DocumentSourceConfig{
			BaseURL:    s.Config.BaseURL,
			APIVersion: s.Config.APIVersion,
			APIKeyEnv:  s.Config.APIKeyEnv,
		},
		TenancyStrategy: config.DocumentSourceTenancy{
			Kind:       s.TenancyStrategy.Kind,
			EnvPattern: s.TenancyStrategy.EnvPattern,
		},
	}
}
