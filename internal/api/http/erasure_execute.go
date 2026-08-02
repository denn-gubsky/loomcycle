package http

// erasure_execute.go — the HTTP face of POST /v1/_erasure.
//
// The operation lives in internal/erasure, including the confirm guard: that is
// a property of the erasure, not of HTTP, and leaving it to each transport would
// mean the newest one is the one missing it.
//
// What stays here is the DEFAULTING, which is genuinely a wire concern. dry_run
// is a *bool so an omitted field — or POST {} from a client with a buggy
// serialiser — means DO NOTHING. A plain bool would make the zero value
// destructive.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
)

type erasureRequest struct {
	Subject string `json:"subject"`
	DryRun  *bool  `json:"dry_run"`
	Confirm string `json:"confirm"`
}

// handleErasureExecute serves POST /v1/_erasure.
func (s *Server) handleErasureExecute(w http.ResponseWriter, r *http.Request) {
	var req erasureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "decode body: "+err.Error())
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_store", "no store configured")
		return
	}
	tenant, ok := s.resolveErasureTenant(w, r)
	if !ok {
		return
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	res, err := s.erasureService().Execute(r.Context(), erasure.ExecuteRequest{
		Tenant: tenant, Subject: req.Subject, DryRun: dryRun, Confirm: req.Confirm,
	})
	switch {
	case errors.Is(err, erasure.ErrNoSubject):
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	case errors.Is(err, erasure.ErrConfirmMismatch):
		writeJSONError(w, http.StatusBadRequest, "confirm_mismatch", err.Error())
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "erasure_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
