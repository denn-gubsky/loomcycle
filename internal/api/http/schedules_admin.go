// schedules_admin.go — v1.x RFC E /ui/schedules backend support.
// Two read-only endpoints that drive the Web UI's schedules tab:
//
//	GET /v1/_schedules/list-all          — merged yaml + substrate list
//	GET /v1/_schedules/{def_id}/state    — per-def runtime state row
//
// Mirrors the v0.9.x /v1/_library/* shape — same merged-envelope
// pattern (one entry per name with `source: static|dynamic|both`).
// The substrate-write endpoint POST /v1/_scheduledef shipped earlier
// covers create/fork/get/list/retire ops directly; these new
// endpoints just provide the UI's enumeration + state queries.
//
// Read-only + bearer-authed via the same authMiddleware that wraps
// every /v1/_* endpoint.
package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ScheduleListEntry is one row in the /ui/schedules list. Mirrors
// LibraryEntry's shape so the UI can reuse rendering logic. Static
// entries inline the yaml-side ScheduledRun definition for inline
// display; dynamic entries omit it (clients fetch via
// POST /v1/_scheduledef {op:"get",def_id} when the user expands the
// row).
type ScheduleListEntry struct {
	Name             string          `json:"name"`
	Source           string          `json:"source"` // "static-only" | "dynamic-only" | "both"
	InStatic         bool            `json:"in_static"`
	InSubstrate      bool            `json:"in_substrate"`
	VersionCount     int             `json:"version_count,omitempty"`
	ActiveDefID      string          `json:"active_def_id,omitempty"`
	LatestVersion    int             `json:"latest_version,omitempty"`
	LastUpdated      time.Time       `json:"last_updated,omitempty"`
	StaticDefinition json.RawMessage `json:"static_definition,omitempty"`
}

type schedulesListResponse struct {
	Entries []ScheduleListEntry `json:"entries"`
}

// handleListSchedules serves GET /v1/_schedules/list-all.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	subRows := map[string]store.ScheduleDefNameSummary{}
	if s.store != nil {
		rows, err := s.store.ScheduleDefListNames(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			subRows[row.Name] = row
		}
	}

	entries := make([]ScheduleListEntry, 0, len(s.cfg.ScheduledRuns)+len(subRows))
	seen := map[string]struct{}{}

	for name, sr := range s.cfg.ScheduledRuns {
		entry := ScheduleListEntry{
			Name:             name,
			InStatic:         true,
			StaticDefinition: marshalStaticScheduledRun(sr),
		}
		if sub, ok := subRows[name]; ok {
			entry.InSubstrate = true
			entry.VersionCount = sub.VersionCount
			entry.ActiveDefID = sub.ActiveDefID
			entry.LatestVersion = sub.LatestVersion
			entry.LastUpdated = sub.LastUpdated
		}
		entry.Source = deriveSource(entry.InStatic, entry.InSubstrate)
		entries = append(entries, entry)
		seen[name] = struct{}{}
	}
	for name, sub := range subRows {
		if _, ok := seen[name]; ok {
			continue
		}
		entries = append(entries, ScheduleListEntry{
			Name:          name,
			Source:        deriveSource(false, true),
			InSubstrate:   true,
			VersionCount:  sub.VersionCount,
			ActiveDefID:   sub.ActiveDefID,
			LatestVersion: sub.LatestVersion,
			LastUpdated:   sub.LastUpdated,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSONOK(w, schedulesListResponse{Entries: entries})
}

// ScheduleStateView is the wire shape for GET .../state. Bearers +
// any other sensitive substrate-stored fields stay on the substrate
// path (POST /v1/_scheduledef {op:"get"}); this endpoint only
// surfaces runtime telemetry (last/next + status + error).
type ScheduleStateView struct {
	DefID       string    `json:"def_id"`
	LastRunAt   time.Time `json:"last_run_at,omitempty"`
	LastRunID   string    `json:"last_run_id,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	NextRunAt   time.Time `json:"next_run_at"`
	PausedUntil time.Time `json:"paused_until,omitempty"`
}

// handleGetScheduleState serves GET /v1/_schedules/{def_id}/state.
func (s *Server) handleGetScheduleState(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("def_id")
	if defID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_def_id", "def_id path param required")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "no store configured")
		return
	}
	row, err := s.store.ScheduleRunStateGet(r.Context(), defID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "not_found", "no run-state row for def_id")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSONOK(w, ScheduleStateView{
		DefID:       row.DefID,
		LastRunAt:   row.LastRunAt,
		LastRunID:   row.LastRunID,
		LastStatus:  row.LastStatus,
		LastError:   row.LastError,
		NextRunAt:   row.NextRunAt,
		PausedUntil: row.PausedUntil,
	})
}

// marshalStaticScheduledRun renders the yaml-side `config.ScheduledRun`
// for inline display in the UI's detail pane. Returns `null` JSON on
// marshal failure (which should never happen for a struct with simple
// fields).
func marshalStaticScheduledRun(sr config.ScheduledRun) json.RawMessage {
	b, err := json.Marshal(sr)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// handleScheduleRunNow serves POST /v1/_schedules/{def_id}/run-now.
// Forces an immediate fire by setting next_run_at to time.Now() — the
// sweeper picks it up on the next tick. The schedule's regular
// next_run_at advance happens after the run completes (per the
// scheduler's normal fire path), so a forced run doesn't skip the
// schedule's cadence going forward.
//
// Race caveat: if a run for this def_id is already in progress when
// run-now is invoked, the in-flight fire's post-completion
// ScheduleRunStateRecordResult will overwrite next_run_at with the
// next scheduled time — silently discarding the operator's intent.
// The endpoint returns 200 in that case (the upsert itself succeeds),
// but the schedule fires at most once extra. Fixing this fully needs
// a separate force-fire flag column or a per-def mutex; v1 accepts
// the race as low-impact for the admin use case.
func (s *Server) handleScheduleRunNow(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("def_id")
	if defID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_def_id", "def_id path param required")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "no store configured")
		return
	}
	// Pre-flight existence check so unknown def_ids return 404 instead
	// of the FK-constraint-violation 500 the bare upsert would produce.
	// Matches the 404 shape of pause/resume. The state-row check is
	// cheap (single indexed lookup) and covers the realistic cases:
	// either the def has a state row (active schedule, fire allowed),
	// or it doesn't (not yet promoted, retired-and-cascade-deleted,
	// or never existed).
	if _, err := s.store.ScheduleRunStateGet(r.Context(), defID); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "not_found", "no run-state row for def_id (def may not be promoted, or has been retired)")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if err := s.store.ScheduleRunStateSeed(r.Context(), defID, time.Now().Add(-1*time.Second)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"def_id": defID, "scheduled": "next-tick"})
}

// handleSchedulePause serves POST /v1/_schedules/{def_id}/pause.
// Sets paused_until to a far-future time (year 9999) so the sweeper's
// `paused_until IS NULL OR paused_until <= now` filter drops the row
// indefinitely. Resume clears the field; admin-driven pause is
// distinct from the runtime-wide PauseManager (which gates the sweeper
// as a whole).
func (s *Server) handleSchedulePause(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("def_id")
	if defID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_def_id", "def_id path param required")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "no store configured")
		return
	}
	// "Indefinite pause" — 100 years from now. Year 9999 would
	// overflow SQLite's int64-nanosecond timestamp storage; 100 years
	// is safely under int64.MaxInt64 nanos (~2262-04-11) and matches
	// the "effectively forever from an operator perspective" intent.
	// Resume clears the field entirely (zero time → NULL).
	farFuture := time.Now().Add(100 * 365 * 24 * time.Hour)
	if err := s.store.ScheduleRunStatePause(r.Context(), defID, farFuture); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "not_found", "no run-state row for def_id")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"def_id": defID, "paused": true})
}

// handleScheduleResume serves POST /v1/_schedules/{def_id}/resume.
// Clears paused_until (passes zero time, which the store treats as
// NULL) so the sweeper considers this def due-eligible again.
func (s *Server) handleScheduleResume(w http.ResponseWriter, r *http.Request) {
	defID := r.PathValue("def_id")
	if defID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_def_id", "def_id path param required")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "no store configured")
		return
	}
	if err := s.store.ScheduleRunStatePause(r.Context(), defID, time.Time{}); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "not_found", "no run-state row for def_id")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"def_id": defID, "paused": false})
}
