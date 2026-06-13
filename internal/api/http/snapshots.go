package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Snapshot admin endpoints — v0.8.17 Pause/Resume/Snapshot (PR 2).
// Wire shape:
//
//   POST   /v1/_snapshots                    — capture a new snapshot
//     body: {"label?": "...", "include_history?": false, "include_history_since?": "RFC3339"}
//     → 201 {"id": "snap_...", "byte_size": N, "created_at": "..."}
//     → 413 if exceeds LOOMCYCLE_SNAPSHOT_MAX_BYTES (or operator-supplied cap)
//
//   GET    /v1/_snapshots?label_contains=&limit=200
//     → 200 {"entries": [...metadata only, no JSON payload...]}
//
//   GET    /v1/_snapshots/{id}                — full row including JSON
//     → 200 {"id": ..., "json_content": {...}}
//     → 404 *ErrNotFound
//
//   DELETE /v1/_snapshots/{id}
//     → 204 idempotent (true OR false from store.SnapshotDelete both
//       map to 204 — operators scripting cleanup never see 404 on a
//       missing row, only "row no longer exists")
//
// Auth: bearer-token middleware applied at mux registration time.
// No agent surface — these are operator-only.

// snapshotCreateRequest is the body of POST /v1/_snapshots.
type snapshotCreateRequest struct {
	Label               string `json:"label,omitempty"`
	IncludeHistory      bool   `json:"include_history,omitempty"`
	IncludeHistorySince string `json:"include_history_since,omitempty"` // RFC3339; optional even when IncludeHistory=true
	MaxBytes            int64  `json:"max_bytes,omitempty"`             // override; 0 = use DefaultMaxBytes
}

// snapshotCreateResponse is the 201 response — metadata only; the
// caller fetches the full JSON via GET /v1/_snapshots/{id}.
type snapshotCreateResponse struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Label         string `json:"label,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	ByteSize      int64  `json:"byte_size"`
}

// snapshotListResponse wraps the metadata listing.
type snapshotListResponse struct {
	Entries []snapshotListEntryResponse `json:"entries"`
}

type snapshotListEntryResponse struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Label         string `json:"label,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	ByteSize      int64  `json:"byte_size"`
}

// snapshotGetResponse carries the full row including the JSON
// payload. JSONContent is emitted as a raw nested object — the
// envelope is already valid JSON, so re-marshalling would be wasted.
type snapshotGetResponse struct {
	ID            string          `json:"id"`
	CreatedAt     string          `json:"created_at"`
	Label         string          `json:"label,omitempty"`
	SchemaVersion int             `json:"schema_version"`
	ByteSize      int64           `json:"byte_size"`
	JSONContent   json.RawMessage `json:"json_content"`
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotCreateRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		// Empty body is fine (all fields optional); only error on
		// malformed JSON.
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON object (or empty)")
		return
	}
	opts := snapshot.CaptureOptions{
		Label:          req.Label,
		MaxBytes:       req.MaxBytes,
		IncludeHistory: req.IncludeHistory,
		Channels:       channelConfigForSnapshot(s.cfg),
	}
	if req.IncludeHistorySince != "" {
		ts, err := time.Parse(time.RFC3339, req.IncludeHistorySince)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_since", "include_history_since must be RFC3339")
			return
		}
		opts.IncludeHistorySince = ts
	}
	row, _, err := snapshot.Capture(r.Context(), s.store, opts)
	if err != nil {
		var tooLarge *snapshot.ErrSnapshotTooLarge
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "snapshot_too_large", tooLarge.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "capture_failed", err.Error())
		return
	}
	if err := s.store.SnapshotCreate(r.Context(), *row); err != nil {
		// Defensive: SnapshotCreate's id collision is rare (8 hex
		// bytes + ms timestamp) but possible under scripted bulk
		// captures. Surface as 409 so the caller can retry.
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			writeJSONError(w, http.StatusConflict, "snapshot_id_conflict", conflict.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "persist_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(snapshotCreateResponse{
		ID:            row.ID,
		CreatedAt:     row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Label:         row.Label,
		SchemaVersion: row.SchemaVersion,
		ByteSize:      row.ByteSize,
	})
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	labelContains := r.URL.Query().Get("label_contains")
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be a non-negative integer")
			return
		}
		// limit=0 keeps the default (200). The codebase convention is
		// "0 = use default", not "no limit" — an operator scripting
		// curl ?limit=0 expecting empty results would otherwise get
		// the entire table. True unlimited isn't a wire feature; bump
		// the limit param when more rows are needed.
		if n > 0 {
			limit = n
		}
	}
	rows, err := s.store.SnapshotList(r.Context(), labelContains, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	entries := make([]snapshotListEntryResponse, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, snapshotListEntryResponse{
			ID:            r.ID,
			CreatedAt:     r.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Label:         r.Label,
			SchemaVersion: r.SchemaVersion,
			ByteSize:      r.ByteSize,
		})
	}
	writeJSON(w, http.StatusOK, snapshotListResponse{Entries: entries})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.store.SnapshotGet(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot with id "+id)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshotGetResponse{
		ID:            row.ID,
		CreatedAt:     row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Label:         row.Label,
		SchemaVersion: row.SchemaVersion,
		ByteSize:      row.ByteSize,
		JSONContent:   row.JSONContent,
	})
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Idempotent: both (true) and (false) map to 204. Operators
	// scripting cleanup never see 404 on a missing row.
	if _, err := s.store.SnapshotDelete(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// snapshotRestoreRequest is the body of POST /v1/_snapshots/{id}/restore.
type snapshotRestoreRequest struct {
	// IncludeHistory toggles restoring the optional
	// interaction_history section. Default false (the running-state
	// restore most operators want).
	IncludeHistory bool `json:"include_history,omitempty"`
	// JSON, when set, overrides the {id} path — restore from the
	// supplied envelope rather than looking up by id. Used by the
	// CLI's `loomcycle snapshot restore <file.json>` flow which
	// posts the envelope directly without persisting first.
	JSON json.RawMessage `json:"json,omitempty"`
}

// snapshotRestoreResponse mirrors snapshot.RestoreResult for the wire.
type snapshotRestoreResponse struct {
	AgentDefsRestored       int `json:"agent_defs_restored"`
	AgentDefActiveRestored  int `json:"agent_def_active_restored"`
	MemoryRestored          int `json:"memory_restored"`
	ChannelMessagesRestored int `json:"channel_messages_restored"`
	ChannelCursorsRestored  int `json:"channel_cursors_restored"`
	EvaluationsRestored     int `json:"evaluations_restored"`
	PausedRunsRestored      int `json:"paused_runs_restored"`
	// PausedRunsResumed is how many of the restored paused runs were
	// re-dispatched as live loops (F42 / RFC X Phase 2). May be < restored
	// when a run's agent no longer resolves or it isn't auto-resumable; those
	// are flagged failed and surfaced in Warnings.
	PausedRunsResumed          int      `json:"paused_runs_resumed"`
	SynthesizedSessions        int      `json:"synthesized_sessions"`
	TranscriptEventsRestored   int      `json:"transcript_events_restored"`
	InteractionHistoryRestored int      `json:"interaction_history_restored"`
	Warnings                   []string `json:"warnings,omitempty"`
}

func (s *Server) handleRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req snapshotRestoreRequest
	// The body may carry an inline snapshot envelope (req.json) up to the
	// snapshot ceiling, so the cap is the envelope ceiling + envelope-field
	// headroom — not the 1 MiB control-body cap used elsewhere.
	r.Body = http.MaxBytesReader(w, r.Body, snapshot.DefaultMaxBytes+(1<<20))
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON object (or empty)")
		return
	}

	// Resolve the envelope bytes — either the supplied JSON or
	// fetch by id.
	var rawBytes []byte
	if len(req.JSON) > 0 {
		rawBytes = req.JSON
	} else {
		row, err := s.store.SnapshotGet(r.Context(), id)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				writeJSONError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot with id "+id)
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "get_failed", err.Error())
			return
		}
		rawBytes = row.JSONContent
	}

	// Restore — passes ForceProbe so the resolver matrix is
	// refreshed before this returns. Operators can call Resume
	// immediately after a successful restore without waiting for
	// the periodic probe.
	opts := snapshot.RestoreOptions{
		IncludeHistory: req.IncludeHistory,
	}
	if s.resolver != nil {
		opts.ForceProbe = s.resolver.ForceProbe
	}
	result, err := snapshot.Restore(r.Context(), s.store, rawBytes, opts)
	if err != nil {
		// Migration / version errors map to 422 (semantically valid
		// JSON, semantically invalid state). The error message
		// carries the section + version strings so operators see
		// what to do.
		var tooNew *migrations.ErrSnapshotVersionTooNew
		var unknown *migrations.ErrUnknownSectionVersion
		switch {
		case errors.As(err, &tooNew):
			writeJSONError(w, http.StatusUnprocessableEntity, "snapshot_version_too_new", err.Error())
			return
		case errors.As(err, &unknown):
			writeJSONError(w, http.StatusUnprocessableEntity, "snapshot_version_unknown", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "restore_failed", err.Error())
		return
	}

	// F42 / RFC X Phase 2: re-dispatch the just-restored paused runs so a
	// snapshotted mid-run experiment genuinely continues on this instance
	// (reconstruct each loop from its transcript). Runs whose agent no longer
	// resolves — or that aren't auto-resumable — are flagged failed and
	// surfaced in the warnings. Uses a detached background context so a slow
	// re-dispatch (or a long-lived resumed loop) doesn't block the response.
	warnings := result.Warnings
	resumed, resumeWarnings := s.ResumePausedRuns(context.WithoutCancel(r.Context()))
	warnings = append(warnings, resumeWarnings...)

	writeJSON(w, http.StatusOK, snapshotRestoreResponse{
		AgentDefsRestored:          result.AgentDefsRestored,
		AgentDefActiveRestored:     result.AgentDefActiveRestored,
		MemoryRestored:             result.MemoryRestored,
		ChannelMessagesRestored:    result.ChannelMessagesRestored,
		ChannelCursorsRestored:     result.ChannelCursorsRestored,
		EvaluationsRestored:        result.EvaluationsRestored,
		PausedRunsRestored:         result.PausedRunsRestored,
		PausedRunsResumed:          resumed,
		SynthesizedSessions:        result.SynthesizedSessions,
		TranscriptEventsRestored:   result.TranscriptEventsRestored,
		InteractionHistoryRestored: result.InteractionHistoryRestored,
		Warnings:                   warnings,
	})
}

func (s *Server) handleExportSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.store.SnapshotGet(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot with id "+id)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	// Serve the raw JSON content with a Content-Disposition header
	// so CLI consumers using `curl -O` save it under the snapshot's
	// id. The bytes are already canonical (Capture stored them via
	// json.Marshal); no re-marshal needed.
	//
	// Sanitize the id before splicing into the header value: snapshot
	// IDs from mintID are clean ("snap_<ms>_<hex>") but the id here is
	// a path param under operator control, and trusting it would
	// permit header injection / response-splitting via embedded
	// quotes, CR, or LF. Replace those with '_' rather than rejecting
	// so a misuse still gets the body (just with a sanitized filename).
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeSnapshotFilename(id)+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(row.JSONContent)
}

// sanitizeSnapshotFilename strips characters that would break the
// Content-Disposition header value (double quote, CR, LF) and replaces
// them with '_'. mintID-produced ids never trip this in practice; it's
// defense-in-depth against operator-supplied or future-format ids.
func sanitizeSnapshotFilename(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, id)
}

// channelConfigForSnapshot translates cfg.Channels (operator-yaml
// shape, map[string]config.Channel) into the snapshot envelope's
// []snapshot.ChannelConfigEntry. Stable ordering across captures
// would require sorting; today the map iteration order is Go's
// random-per-run order, but snapshot.Capture's deterministic-
// ordering tests focus on store-read sections (memory, evaluations)
// rather than the operator-config passthrough. If operators report
// nondeterminism here, sort by Name in a follow-up.
func channelConfigForSnapshot(cfg *config.Config) []snapshot.ChannelConfigEntry {
	if cfg == nil || len(cfg.Channels) == 0 {
		return nil
	}
	out := make([]snapshot.ChannelConfigEntry, 0, len(cfg.Channels))
	for name, ch := range cfg.Channels {
		out = append(out, snapshot.ChannelConfigEntry{
			Name:        name,
			Description: ch.Semantic,
			Scope:       ch.Scope,
			TTLSeconds:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
		})
	}
	return out
}
