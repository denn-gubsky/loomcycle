package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// hookRegisterRequest is the wire shape POSTed to /v1/hooks. Mirrors
// hooks.Hook minus the loomcycle-assigned fields (ID, RegisteredAt).
type hookRegisterRequest struct {
	Owner       string         `json:"owner"`
	Name        string         `json:"name"`
	Phase       hooks.Phase    `json:"phase"`
	Agents      []string       `json:"agents"`
	Tools       []string       `json:"tools"`
	CallbackURL string         `json:"callback_url"`
	FailMode    hooks.FailMode `json:"fail_mode"`
	TimeoutMs   int            `json:"timeout_ms"`
}

// hookRegisterResponse is the wire shape returned on success. The id
// is loomcycle-assigned; clients use it on DELETE /v1/hooks/{id}.
type hookRegisterResponse struct {
	ID string `json:"id"`
}

// handleRegisterHook accepts a hookRegisterRequest, registers it
// against the in-memory registry, and returns the assigned id.
//
// Idempotency: if the (owner, name) tuple already maps to a registered
// hook, the prior entry is replaced in-place (preserving chain order)
// and a fresh id is assigned. Apps re-registering on their own startup
// don't have to worry about cascading duplicates.
func (s *Server) handleRegisterHook(w http.ResponseWriter, r *http.Request) {
	var req hookRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON: "+err.Error())
		return
	}
	h := &hooks.Hook{
		Owner:       req.Owner,
		Name:        req.Name,
		Phase:       req.Phase,
		Agents:      req.Agents,
		Tools:       req.Tools,
		CallbackURL: req.CallbackURL,
		FailMode:    req.FailMode,
		TimeoutMs:   req.TimeoutMs,
	}
	id, err := s.hookRegistry.Register(h)
	if err != nil {
		if errors.Is(err, hooks.ErrInvalidRegistration) {
			writeJSONError(w, http.StatusBadRequest, "invalid_registration", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(hookRegisterResponse{ID: id})
}

// handleListHooks returns every currently-registered hook in
// registration order. Useful for debug; no pagination because the
// registry is intentionally small (operator-curated set).
func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hooks": s.hookRegistry.List(),
	})
}

// handleDeleteHook removes the hook with the given id from the
// registry. Returns 404 if no such id exists. Idempotent in the
// sense that a second DELETE on the same id always returns 404 once
// the first has succeeded.
func (s *Server) handleDeleteHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_id", "hook id required in path")
		return
	}
	if err := s.hookRegistry.Delete(id); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no hook with id %q", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"deleted": id})
}

// writeJSONError emits a uniform error envelope. Mirrors the existing
// inline pattern in server.go (handleRuns / handleCancelAgent etc.) so
// adapters can rely on `{"code": "...", "error": "..."}` for every 4xx.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%q,"error":%q}`, code, msg)
}
