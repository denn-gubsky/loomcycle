package lookup

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// VolumeSpec is the resolved runtime spec for a volume NAME — the path the
// file/exec tools confine to + its access mode. Source ("static" /
// "dynamic") is for log lines + UI badges.
type VolumeSpec struct {
	// Path is the resolved absolute directory root. For a static volume
	// this is the config-validated absolute path; for a dynamic VolumeDef
	// it is the runtime-derived <dynamic_root>/<tenant-segment>/<name>.
	Path string
	// Mode is "rw" or "ro".
	Mode string
	// Source — "static" (operator yaml) or "dynamic" (VolumeDef substrate).
	Source string
}

// VolumeDefStore is the subset of store.Store the volume resolver uses —
// a single by-name read. Declared here so callers + tests can mock without
// depending on the full store interface.
type VolumeDefStore interface {
	VolumeDefGetByName(ctx context.Context, tenantID, name string) (store.VolumeDefRow, error)
}

// volumeDefBody mirrors the JSON shape the VolumeDef tool persists in
// volume_defs.definition (`{"path":..,"mode":..}`).
type volumeDefBody struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// VolumeDef resolves a volume NAME to its effective runtime spec within
// the caller's tenant. Precedence — STATIC IS GROUND TRUTH FIRST (the
// opposite of MCPServer, where a per-tenant dynamic shadow may override
// the static base):
//
//  1. static cfg.Volumes (operator yaml). An operator-declared static
//     volume name can NEVER be shadowed by a dynamic VolumeDef — the
//     operator's floor wins. (The VolumeDef tool also refuses `create`
//     over a static name, so this is belt-and-braces.)
//  2. (tenantID != "") tenant-scoped dynamic VolumeDef.
//  3. shared dynamic VolumeDef (tenant_id="").
//
// Returns (zero, false) when no source has the name.
//
// No in-memory cache (cf. the MCP DynamicRegistry): run-start volume
// resolution is not a hot path, and a stale cache could hand an agent a
// purged volume's path. Always-correct beats fast here — the store read
// happens on each resolution. A store fault (or a malformed definition) on
// a dynamic tier is treated as "not resolvable here" and falls through; a
// dynamic-volume read is never the floor.
//
// tenantID MUST come from the authoritative principal in ctx (never a wire
// field), exactly as the other resolvers require. A nil st skips the
// dynamic tiers (static-only deployment).
func VolumeDef(ctx context.Context, cfg *config.Config, st VolumeDefStore, tenantID, name string) (VolumeSpec, bool) {
	// 1. Static cfg.Volumes — ground truth, the operator floor.
	if cfg != nil {
		if v, ok := cfg.Volumes[name]; ok {
			mode := "rw"
			if v.ReadOnly() {
				mode = "ro"
			}
			return VolumeSpec{Path: v.Path, Mode: mode, Source: "static"}, true
		}
	}
	if st == nil {
		return VolumeSpec{}, false
	}
	// 2. Tenant-scoped dynamic shadow (skipped for the shared "" tenant so
	//    its order is static → shared-dynamic with no redundant first read).
	if tenantID != "" {
		if spec, ok := resolveDynamicVolume(ctx, st, tenantID, name); ok {
			return spec, true
		}
	}
	// 3. Shared dynamic VolumeDef (tenant_id="").
	if spec, ok := resolveDynamicVolume(ctx, st, "", name); ok {
		return spec, true
	}
	return VolumeSpec{}, false
}

// resolveDynamicVolume reads one (tenantID, name) dynamic row and decodes
// its {path,mode} body. ok=false on a miss, a store fault, or a malformed
// body — the caller falls through to the next tier rather than failing the
// whole resolution (a dynamic read is never the floor).
func resolveDynamicVolume(ctx context.Context, st VolumeDefStore, tenantID, name string) (VolumeSpec, bool) {
	row, err := st.VolumeDefGetByName(ctx, tenantID, name)
	if err != nil {
		// *ErrNotFound is the common "no such dynamic volume" case; any
		// other fault is logged by the store layer and likewise falls
		// through (the static floor already missed, so the name is simply
		// unresolvable for this run).
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return VolumeSpec{}, false
		}
		return VolumeSpec{}, false
	}
	var body volumeDefBody
	if err := json.Unmarshal(row.Definition, &body); err != nil || body.Path == "" {
		return VolumeSpec{}, false
	}
	mode := body.Mode
	if mode == "" {
		mode = "rw"
	}
	return VolumeSpec{Path: body.Path, Mode: mode, Source: "dynamic"}, true
}
