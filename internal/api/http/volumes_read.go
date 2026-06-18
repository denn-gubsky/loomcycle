package http

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// volumes_read.go — RFC AH Phase 4 (Web UI volume management) two ADDITIVE,
// read-only endpoints the console needs to render the volume universe. They
// add NO runtime primitive; CRUD stays on the existing POST /v1/_volumedef
// (create/delete/purge) dispatched via the substrate admin handler.
//
// Both reads are tenant-scoped from the AUTHORITATIVE principal stamped on
// ctx by the auth middleware — NEVER a wire/query field. The scope gate
// (requiredScopeFor → ScopeTenant for these GETs) gates them to the tenant
// operator; admin/legacy satisfies ScopeTenant. The DYNAMIC + EPHEMERAL rows
// are filtered to the caller's tenant (opaque: a cross-tenant row is simply
// absent, never a 403/404 oracle). STATIC volumes are operator-authored
// config — the shared bind FLOOR every tenant may bind to — so they are shown
// to everyone.

// volumesReadTenant resolves the caller's authoritative tenant for the read
// endpoints. Mirrors substrateAdminCtx's tenant derivation: the principal the
// auth middleware stamped, never the wire. "" (no principal: legacy
// LOOMCYCLE_AUTH_TOKEN / open mode) = the shared tenant — the correct
// single-tenant behaviour.
func volumesReadTenant(r *http.Request) string {
	principal, _ := auth.PrincipalFromContext(r.Context())
	return principal.TenantID
}

// persistentVolumeEntry is one row of GET /v1/_volumes.
type persistentVolumeEntry struct {
	Name string `json:"name"`
	// Source is "static" (operator yaml, read-only) or "dynamic" (a tenant's
	// VolumeDef row, CRUD-able).
	Source string `json:"source"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	// Default marks the static volume the operator flagged `default: true`.
	// Dynamic volumes are never the default (only static config can be).
	Default bool `json:"default"`
	// DynamicRoot marks the static volume that is the operator-blessed parent
	// dynamic VolumeDefs are provisioned inside. Always false for dynamic rows.
	DynamicRoot bool `json:"dynamic_root"`
	// CreatedAt is set for dynamic rows (the substrate stamps it); empty for
	// static volumes (config has no creation timestamp).
	CreatedAt string `json:"created_at,omitempty"`
}

type persistentVolumesResponse struct {
	Entries []persistentVolumeEntry `json:"entries"`
}

// handleListVolumes serves GET /v1/_volumes — the merged PERSISTENT volume
// universe for the caller's tenant: every static cfg.Volumes entry (the shared
// bind floor, read-only) plus the tenant's own dynamic VolumeDef rows.
func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	entries := make([]persistentVolumeEntry, 0, len(s.cfg.Volumes))

	// Static volumes — the operator-authored universe, shown to every tenant
	// (it's the bind floor). Read-only from the UI; config is ground truth.
	for name, vol := range s.cfg.Volumes {
		mode := vol.Mode
		if mode == "" {
			mode = "rw" // empty defaults to rw, validated at config-load
		}
		entries = append(entries, persistentVolumeEntry{
			Name:        name,
			Source:      "static",
			Path:        vol.Path,
			Mode:        mode,
			Default:     vol.Default,
			DynamicRoot: vol.DynamicRoot,
		})
	}

	// Dynamic VolumeDefs — filtered to the caller's authoritative tenant. Nil
	// store (tests / no persistence) → no dynamic rows, statics still surface.
	if s.store != nil {
		rows, err := s.store.VolumeDefList(r.Context(), volumesReadTenant(r))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			path, mode := decodeVolumeDefBody(row.Definition)
			entries = append(entries, persistentVolumeEntry{
				Name:      row.Name,
				Source:    "dynamic",
				Path:      path,
				Mode:      mode,
				CreatedAt: row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		// Stable secondary key when a static + dynamic name coincide (the tool
		// refuses dynamic creates over a static name, so this is defensive).
		return entries[i].Source < entries[j].Source
	})
	writeJSONOK(w, persistentVolumesResponse{Entries: entries})
}

// ephemeralVolumeEntry is one row of GET /v1/_volumes/ephemeral.
type ephemeralVolumeEntry struct {
	Name      string `json:"name"`
	RootRunID string `json:"root_run_id"`
	Path      string `json:"path"`
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at"`
}

type ephemeralVolumesResponse struct {
	Entries []ephemeralVolumeEntry `json:"entries"`
}

// handleListEphemeralVolumes serves GET /v1/_volumes/ephemeral — the LIVE
// ephemeral volumes for the caller's tenant. The persisted
// ephemeral_volume_defs table is the cross-replica source of truth (rows are
// deleted at run completion / by the sweeper), so a tenant-scoped read returns
// exactly the currently-active ephemeral volumes. Tenant filtering happens at
// the store boundary (EphemeralVolumeListByTenant) — a tenant never observes
// another tenant's rows.
func (s *Server) handleListEphemeralVolumes(w http.ResponseWriter, r *http.Request) {
	entries := make([]ephemeralVolumeEntry, 0)
	if s.store != nil {
		rows, err := s.store.EphemeralVolumeListByTenant(r.Context(), volumesReadTenant(r))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			path, mode := decodeVolumeDefBody(row.Definition)
			entries = append(entries, ephemeralVolumeEntry{
				Name:      row.Name,
				RootRunID: row.RootRunID,
				Path:      path,
				Mode:      mode,
				CreatedAt: row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
			})
		}
	}
	writeJSONOK(w, ephemeralVolumesResponse{Entries: entries})
}

// decodeVolumeDefBody extracts {path, mode} from a VolumeDef/ephemeral row's
// runtime-derived definition JSON. A malformed body yields empty strings
// rather than failing the whole list (defensive — the tool always writes a
// well-formed body, so this never fires in practice).
func decodeVolumeDefBody(def json.RawMessage) (path, mode string) {
	var body struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(def, &body)
	return body.Path, body.Mode
}
