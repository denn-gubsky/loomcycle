package builtin

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// MCPServerDefStore is the store reads RehydrateMCPRegistry needs.
type MCPServerDefStore interface {
	MCPServerDefListNames(ctx context.Context) ([]store.MCPServerDefNameSummary, error)
	MCPServerDefGetActive(ctx context.Context, tenantID, name string) (store.MCPServerDefRow, error)
}

// RehydrateMCPRegistry loads every active, non-retired MCP server def from the
// store into the in-process registry the pool's build callback consults. Boot
// calls it to bring previous registrations back after a restart; a snapshot
// restore calls it so the defs it just wrote go live without one. One function
// for both, so the two paths cannot drift.
//
// Each def is read and Set inside reg.Serialize, the lock MCPServerDef's
// retire and promote hold across their store write and registry change. The
// read is by (tenant, name), not by the def_id the list returned: a promote
// or retire that lands between the list and the lock is then seen, where a
// read of the listed def_id would Set a version the store has moved off.
//
// It only Sets: an entry already present is re-Set to the store's active spec,
// which for a live def the restore left alone is the spec it already had, so
// the pool's cached client for it keeps serving. Nothing is removed.
//
// Returns how many entries it made live — absent before, or different — and
// an error only when the def list cannot be read. A row it cannot load or
// parse is logged and skipped: it blocks only its own server.
func RehydrateMCPRegistry(ctx context.Context, st MCPServerDefStore, static map[string]config.MCPServer,
	reg *loommcp.DynamicRegistry, logf func(format string, args ...any)) (int, error) {
	names, err := st.MCPServerDefListNames(ctx)
	if err != nil {
		return 0, err
	}
	activated := 0
	for _, ns := range names {
		if ns.ActiveDefID == "" {
			continue
		}
		// Skip SHARED-tenant names that collide with a static yaml
		// `mcp_servers:` entry — yaml is ground truth for the "" tenant, so a
		// shared registry entry of that name would be unreachable
		// (lookup.MCPServer's "" path is static → shared-dynamic) and only
		// inflate diagnostics. A per-TENANT row that shares the name is a
		// legitimate RFC N override (the tenant-dynamic pass shadows the static
		// base), so it is NOT skipped. Mirrors the execCreate refusal, which
		// only refuses over yaml within the same tenant scope.
		if _, ok := static[ns.Name]; ok && ns.TenantID == "" {
			logf("mcp_server_defs: skipping shared %q — name collides with static yaml entry (yaml takes precedence)", ns.Name)
			continue
		}
		reg.Serialize(func() {
			if rehydrateOne(ctx, st, ns, reg, logf) {
				activated++
			}
		})
	}
	return activated, nil
}

// rehydrateOne loads the active def for one listed name and Sets it. The
// caller holds reg.Serialize. Reports whether the entry was absent or
// different before.
func rehydrateOne(ctx context.Context, st MCPServerDefStore, ns store.MCPServerDefNameSummary,
	reg *loommcp.DynamicRegistry, logf func(format string, args ...any)) bool {
	active, err := st.MCPServerDefGetActive(ctx, ns.TenantID, ns.Name)
	if err != nil {
		// Not-found here means the pointer went away after the list.
		logf("mcp_server_defs: load active %q (tenant=%q): %v", ns.Name, ns.TenantID, err)
		return false
	}
	// SetRetired leaves the active pointer on the retired def_id (the
	// AgentDef/SkillDef semantics). Loading a retired spec would silently
	// revive a name the operator explicitly retired.
	if active.Retired {
		logf("mcp_server_defs: skipping %q (tenant=%q) — active row is retired (def_id=%s)", ns.Name, ns.TenantID, active.DefID)
		return false
	}
	var ov mcpServerOverlay
	if err := json.Unmarshal(active.Definition, &ov); err != nil {
		logf("mcp_server_defs: parse active %q (tenant=%q): %v", ns.Name, ns.TenantID, err)
		return false
	}
	// RFC N: keyed by the def's own (tenant, name), so only that tenant's
	// runs resolve it.
	spec := specFromOverlay(active.TenantID, active.Name, ov)
	spec.DefID, spec.Version = active.DefID, active.Version
	changed := false
	if prev, ok := reg.Get(spec.TenantID, spec.Name); !ok || !reflect.DeepEqual(prev, spec) {
		changed = true
	}
	reg.Set(spec)
	return changed
}
