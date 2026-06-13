package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// handleRegisterHook accepts a connector.RegisterHookRequest, registers
// it through the Connector layer, and returns the assigned id.
//
// Idempotency: if the (owner, name) tuple already maps to a registered
// hook, the prior entry is replaced in-place (preserving chain order)
// and a fresh id is assigned. Apps re-registering on their own startup
// don't have to worry about cascading duplicates.
//
// Refactored in the hooks-connector series: business logic moved to
// Server.RegisterHook (connector_impl_hooks.go); this handler is now
// pure wire-translation, mirroring the v0.8.18 handlePauseRuntime /
// handleSnapshot style.
func (s *Server) handleRegisterHook(w http.ResponseWriter, r *http.Request) {
	var req connector.RegisterHookRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON: "+err.Error())
		return
	}
	resp, err := s.RegisterHook(r.Context(), req)
	if err != nil {
		if errors.Is(err, connector.ErrHookInvalidRegistration) {
			writeJSONError(w, http.StatusBadRequest, "invalid_registration", err.Error())
			return
		}
		if errors.Is(err, connector.ErrHookNotConfigured) {
			writeJSONError(w, http.StatusServiceUnavailable, "hooks_not_configured", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListHooks returns every currently-registered hook in
// registration order. Useful for debug; no pagination because the
// registry is intentionally small (operator-curated set).
func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	resp, err := s.ListHooks(r.Context())
	if err != nil {
		if errors.Is(err, connector.ErrHookNotConfigured) {
			writeJSONError(w, http.StatusServiceUnavailable, "hooks_not_configured", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
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
	if err := s.DeleteHook(r.Context(), id); err != nil {
		if errors.Is(err, connector.ErrHookNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no hook with id %q", id))
			return
		}
		if errors.Is(err, connector.ErrHookNotConfigured) {
			writeJSONError(w, http.StatusServiceUnavailable, "hooks_not_configured", err.Error())
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
