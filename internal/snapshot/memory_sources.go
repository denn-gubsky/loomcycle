package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The memory-backend and document-source sections (RFC DP §4.6):
// memory_backend_defs and document_source_defs, each with its active
// pointers.
//
// Neither body holds a secret: config.api_key_env is an env-var NAME and
// config.base_url an endpoint. Together, though, they are an exfiltration
// pair — the host resolves the named env var and sends its value to the
// URL — so a crafted snapshot could route a tenant's memory, with an
// infrastructure secret attached, to a host of its choosing. The tools refuse
// that pair at authoring, and restore bypasses the tools, so every body goes
// through the authoring validator the target injects before it is written.
// A row whose validator is not wired is skipped, never written unvalidated,
// and a pointer at a skipped def is refused with it. The env names travel as
// written and are never resolved.
//
// Both lookups (lookup.MemoryBackend, lookup.DocumentSource) read the active
// def from the store on every call, so a restored def is live at once and no
// cache needs a refresh. The live row stands, as in every def section.

// captureMemorySources reads every tenant's memory-backend and
// document-source defs and their active pointers, verbatim.
func captureMemorySources(ctx context.Context, s store.Store, sec *Sections) error {
	sec.MemoryBackendDefs.Version = SectionVersion
	sec.MemoryBackendDefActive.Version = SectionVersion
	sec.DocSourceDefs.Version = SectionVersion
	sec.DocSourceDefActive.Version = SectionVersion

	backends, err := s.SnapshotReadMemoryBackendDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot memory_backend_defs: %w", err)
	}
	sec.MemoryBackendDefs.Entries = make([]MemoryBackendDefEntry, 0, len(backends))
	for _, r := range backends {
		sec.MemoryBackendDefs.Entries = append(sec.MemoryBackendDefs.Entries, MemoryBackendDefEntry{
			DefID: r.DefID, TenantID: r.TenantID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID,
			Definition: r.Definition, Description: r.Description, CreatedAt: r.CreatedAt.UTC(),
			CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
			Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic,
		})
	}
	backendPtrs, err := s.SnapshotReadMemoryBackendDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot memory_backend_def_active: %w", err)
	}
	sec.MemoryBackendDefActive.Entries = make([]MemoryBackendDefActiveEntry, 0, len(backendPtrs))
	for _, p := range backendPtrs {
		sec.MemoryBackendDefActive.Entries = append(sec.MemoryBackendDefActive.Entries, MemoryBackendDefActiveEntry{
			Name: p.Name, TenantID: p.TenantID, DefID: p.DefID, PromotedAt: p.PromotedAt.UTC(), PromotedByAgentID: p.PromotedByAgentID,
		})
	}

	sources, err := s.SnapshotReadDocumentSourceDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot document_source_defs: %w", err)
	}
	sec.DocSourceDefs.Entries = make([]DocSourceDefEntry, 0, len(sources))
	for _, r := range sources {
		sec.DocSourceDefs.Entries = append(sec.DocSourceDefs.Entries, DocSourceDefEntry{
			DefID: r.DefID, TenantID: r.TenantID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID,
			Definition: r.Definition, Description: r.Description, CreatedAt: r.CreatedAt.UTC(),
			CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
			Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic,
		})
	}
	sourcePtrs, err := s.SnapshotReadDocumentSourceDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot document_source_def_active: %w", err)
	}
	sec.DocSourceDefActive.Entries = make([]DocSourceDefActiveEntry, 0, len(sourcePtrs))
	for _, p := range sourcePtrs {
		sec.DocSourceDefActive.Entries = append(sec.DocSourceDefActive.Entries, DocSourceDefActiveEntry{
			Name: p.Name, TenantID: p.TenantID, DefID: p.DefID, PromotedAt: p.PromotedAt.UTC(), PromotedByAgentID: p.PromotedByAgentID,
		})
	}
	return nil
}

// dialedKeyBody is the part of a memory-backend or document-source body that
// says which env var a call to the peer sends.
type dialedKeyBody struct {
	Kind   string `json:"kind"`
	Config struct {
		APIKeyEnv string `json:"api_key_env"`
	} `json:"config"`
	TenancyStrategy struct {
		Kind       string `json:"kind"`
		EnvPattern string `json:"env_pattern"`
	} `json:"tenancy_strategy"`
}

// dialedKeyRefs names the env var a call through this def would resolve on
// this host, for the missing-credential scan. It mirrors the peer clients'
// authHeader: a key_per_tenant env_pattern, with {tenant_id} replaced by the
// run's tenant, wins over api_key_env.
//
//   - A tenant-owned def serves only its own tenant's runs, so the pattern is
//     resolved with that tenant.
//   - An operator-layer def is the shared base for EVERY tenant, so its
//     pattern names a different var per calling tenant; nothing here can say
//     which will be set, and it is not checked.
//   - dialed=false (an in-process memory backend) sends nothing, so nothing
//     it names is checked: warning about a var that is never read would only
//     be noise.
func dialedKeyRefs(tenantID string, body json.RawMessage, dialed func(kind string) bool) []credRef {
	var b dialedKeyBody
	if err := json.Unmarshal(body, &b); err != nil || !dialed(b.Kind) {
		return nil
	}
	if b.TenancyStrategy.Kind == "key_per_tenant" && b.TenancyStrategy.EnvPattern != "" {
		if tenantID == "" {
			return nil
		}
		return []credRef{{field: "tenancy_strategy.env_pattern",
			env: strings.ReplaceAll(b.TenancyStrategy.EnvPattern, "{tenant_id}", tenantID)}}
	}
	if v := b.Config.APIKeyEnv; v != "" {
		// "$cred:<name>" as the whole value is a stored credential, resolved in
		// the def's own tenant at tenant level only — what the scan checks.
		if m := credRefRe.FindStringSubmatch(v); m != nil && m[0] == v && m[1] == "cred" {
			return []credRef{{field: "config.api_key_env", cred: v, name: m[2], tenantOnly: true}}
		}
		return []credRef{{field: "config.api_key_env", env: v}}
	}
	return nil
}

// Only a remote memory backend dials its base_url; every document source does.
func memoryBackendDialed(kind string) bool { return kind == "remote" }
func docSourceDialed(string) bool          { return true }

// restoredMemorySourceDef is one def row, for the shared restore loop.
type restoredMemorySourceDef struct {
	defID, tenantID, name string
	version               int
	retired               bool
	body                  json.RawMessage
	insert                func() (bool, error)
}

// restoreMemorySourceDefs validates and inserts each def the target does not
// have, in lineage order, and queues the key each one would send for the
// missing-credential scan. A def it skips or refuses is not on the target,
// so the pointer pass refuses a pointer at it (admitActivePointer).
func restoreMemorySourceDefs(opts RestoreOptions, section, kind string, dialed func(string) bool,
	defs []restoredMemorySourceDef, scan *credScan, counter *int, result *RestoreResult) {
	for _, d := range defs {
		where := fmt.Sprintf("%s %s v%d (def %s)", kind, qualifiedName(d.tenantID, d.name), d.version, d.defID)
		if !validRestoredBody(opts, section, where, d.body, result) {
			continue
		}
		inserted, err := d.insert()
		if err != nil {
			// Typically a live def on the same (tenant, name, version) — this
			// instance's own yaml bootstrap, say. The live row stands.
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored, the live definition stands: %v", where, err))
			continue
		}
		if !inserted {
			continue // already here on this def_id: the live row stands
		}
		*counter++
		if !d.retired {
			scan.addRefs(kind+" "+qualifiedName(d.tenantID, d.name), d.tenantID, dialedKeyRefs(d.tenantID, d.body, dialed))
		}
	}
}

func restoreMemoryBackendDefs(ctx context.Context, s store.Store, sec *MemoryBackendDefsSection, opts RestoreOptions, scan *credScan, result *RestoreResult) {
	defs := make([]restoredMemorySourceDef, 0, len(sec.Entries))
	for _, e := range sec.Entries {
		defs = append(defs, restoredMemorySourceDef{
			defID: e.DefID, tenantID: e.TenantID, name: e.Name, version: e.Version, retired: e.Retired, body: e.Definition,
			insert: func() (bool, error) {
				return s.SnapshotRestoreMemoryBackendDef(ctx, store.MemoryBackendDefRow{
					DefID: e.DefID, TenantID: e.TenantID, Name: e.Name, Version: e.Version, ParentDefID: e.ParentDefID,
					Definition: e.Definition, Description: e.Description, CreatedAt: e.CreatedAt,
					CreatedByAgentID: e.CreatedByAgentID, CreatedByRunID: e.CreatedByRunID,
					Retired: e.Retired, BootstrappedFromStatic: e.BootstrappedFromStatic,
				})
			},
		})
	}
	restoreMemorySourceDefs(opts, migrations.SectionMemoryBackendDefs, "memory_backend_def", memoryBackendDialed,
		defs, scan, &result.MemoryBackendDefsRestored, result)
}

func restoreMemoryBackendDefActive(ctx context.Context, s store.Store, sec *MemoryBackendDefActiveSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		if restoreActivePointer(ctx, s, memoryBackendDefOwner, migrations.SectionMemoryBackendDefActive, e.TenantID, e.Name, e.DefID, func() (bool, error) {
			return s.SnapshotRestoreMemoryBackendDefActive(ctx, store.MemoryBackendDefActiveEntry{
				Name: e.Name, TenantID: e.TenantID, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
			})
		}, result) {
			result.MemoryBackendDefActiveRestored++
		}
	}
}

func restoreDocSourceDefs(ctx context.Context, s store.Store, sec *DocSourceDefsSection, opts RestoreOptions, scan *credScan, result *RestoreResult) {
	defs := make([]restoredMemorySourceDef, 0, len(sec.Entries))
	for _, e := range sec.Entries {
		defs = append(defs, restoredMemorySourceDef{
			defID: e.DefID, tenantID: e.TenantID, name: e.Name, version: e.Version, retired: e.Retired, body: e.Definition,
			insert: func() (bool, error) {
				return s.SnapshotRestoreDocumentSourceDef(ctx, store.DocumentSourceDefRow{
					DefID: e.DefID, TenantID: e.TenantID, Name: e.Name, Version: e.Version, ParentDefID: e.ParentDefID,
					Definition: e.Definition, Description: e.Description, CreatedAt: e.CreatedAt,
					CreatedByAgentID: e.CreatedByAgentID, CreatedByRunID: e.CreatedByRunID,
					Retired: e.Retired, BootstrappedFromStatic: e.BootstrappedFromStatic,
				})
			},
		})
	}
	restoreMemorySourceDefs(opts, migrations.SectionDocSourceDefs, "document_source_def", docSourceDialed,
		defs, scan, &result.DocSourceDefsRestored, result)
}

func restoreDocSourceDefActive(ctx context.Context, s store.Store, sec *DocSourceDefActiveSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		if restoreActivePointer(ctx, s, docSourceDefOwner, migrations.SectionDocSourceDefActive, e.TenantID, e.Name, e.DefID, func() (bool, error) {
			return s.SnapshotRestoreDocumentSourceDefActive(ctx, store.DocumentSourceDefActiveEntry{
				Name: e.Name, TenantID: e.TenantID, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
			})
		}, result) {
			result.DocSourceDefActiveRestored++
		}
	}
}
