package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/pause"
)

// v0.8.17 Pause/Resume/State admin endpoints (PR 4). The pause Manager
// (internal/pause) owns the in-memory state + activeTools registry;
// these handlers are thin wrappers that translate manager errors to
// HTTP status codes.
//
// Wire shape:
//
//   POST /v1/_pause
//     body: {"timeout_ms"?: 30000}  // 0 = use the manager's default
//     → 200 {"state":"paused","duration_ms":N,"force_cancelled_count":N,"paused_runs_count":N,"warnings"?:[]}
//     → 409 {"error":"already_pausing"} when Manager already past Running
//     → 503 {"error":"pause_not_configured"} when no pause Manager wired
//
//   POST /v1/_resume
//     → 200 {"state":"running","resumed_runs_count":N,"warnings"?:[]}
//     → 409 {"error":"not_paused"}
//     → 503 {"error":"pause_not_configured"}
//
//   GET /v1/_state
//     → 200 {"state":"running"|"pausing"|"paused","paused_runs_count":N}
//     → 503 {"error":"pause_not_configured"}
//
// Auth: bearer-token middleware applied at mux registration time.
// No agent surface — these are operator-only.

// pauseRequest is the POST /v1/_pause body. Both fields optional.
type pauseRequest struct {
	// TimeoutMs caps the wait-for-non-idempotent-tools stage. 0 ⇒ use
	// the manager's default (LOOMCYCLE_PAUSE_TIMEOUT_MS or
	// pause.DefaultPauseTimeout). Capped at pause.MaxPauseTimeout.
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
}

func (s *Server) handlePauseRuntime(w http.ResponseWriter, r *http.Request) {
	if s.pauseMgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "pause_not_configured", "no pause manager wired on this server")
		return
	}
	var req pauseRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON object (or empty)")
		return
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	res, err := s.pauseMgr.Pause(r.Context(), timeout)
	if err != nil {
		if errors.Is(err, pause.ErrAlreadyPausing) {
			writeJSONError(w, http.StatusConflict, "already_pausing", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "pause_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleResumeRuntime(w http.ResponseWriter, r *http.Request) {
	if s.pauseMgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "pause_not_configured", "no pause manager wired on this server")
		return
	}
	res, err := s.pauseMgr.Resume(r.Context())
	if err != nil {
		if errors.Is(err, pause.ErrNotPaused) {
			writeJSONError(w, http.StatusConflict, "not_paused", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "resume_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuntimeState(w http.ResponseWriter, r *http.Request) {
	if s.pauseMgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "pause_not_configured", "no pause manager wired on this server")
		return
	}
	snap, err := s.pauseMgr.Snapshot(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "state_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// parseTimeoutMs is a small helper for callers that want to parse a
// raw query/form value into a milliseconds-clamped time.Duration. Not
// used by the handler above (which reads from the JSON body) but kept
// here so future verbs (POST /v1/_pause?timeout_ms=...) reuse the
// same clamping rather than reimplementing it.
func parseTimeoutMs(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("timeout_ms must be a non-negative integer")
	}
	return time.Duration(n) * time.Millisecond, nil
}
