package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		// Empty body is fine (all fields optional); only error on
		// malformed JSON. Use errors.Is rather than string-comparing
		// err.Error() — the json package may wrap io.EOF on streaming
		// decoders depending on Go version.
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
