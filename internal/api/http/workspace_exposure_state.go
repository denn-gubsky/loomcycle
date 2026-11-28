package http

// workspace_exposure_state.go — a page's state on request (RFC HI §4).
//
// GET /v1/workspaces/exposures/{id} answers one page's state, live or closed
// (a closed page's row is kept for a day, RFC HI D6), and GET
// /v1/workspaces/exposures lists the caller's live pages, so a client that
// pinned a page learns after a reload whether it is still open and, if not,
// why it went, without reading a transcript.
//
//   - Only the page's own user: a principal whose tenant and subject are the
//     page's, as the route decides. The legacy token and open mode name no
//     user and reach none. Every other case answers as an unknown id.
//   - Neither carries a bearer, a ticket, a capability or a host path: the
//     page's address is its id, which the client holds already.
//   - Reading the state is not a use of the page: it moves no idle deadline.
//
// The page itself is served under /v1/workspaces/exposures/{id}/… (always
// with the slash after the id: expose returns base_path so). The state read
// is the address without it, a pattern of its own, so the two never meet.

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// exposureState is one page's state as GET answers it.
type exposureState struct {
	ExposureID string `json:"exposure_id"`
	// State is live or closed.
	State     string `json:"state"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Workspace string `json:"workspace"`
	RunID     string `json:"run_id"`
	// ExpiresAt is when a live page closes idle: its last use plus its idle
	// timeout, or its lease's end when later. Absent once closed.
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	ClosedReason   string     `json:"closed_reason,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
}

const (
	exposureStateLive   = "live"
	exposureStateClosed = "closed"
)

func (h *exposureHandler) stateOf(e store.WorkspaceExposure) exposureState {
	out := exposureState{ExposureID: e.ID, State: exposureStateLive, Title: e.Title, Kind: e.Kind,
		Workspace: e.WorkspaceName, RunID: e.RunID, LeaseExpiresAt: e.LeaseExpiresAt}
	if !e.Live() {
		out.State, out.ClosedReason, out.ClosedAt = exposureStateClosed, e.ClosedReason, e.ClosedAt
		return out
	}
	at := h.x.ExposureDeadline(e).UTC()
	out.ExpiresAt = &at
	return out
}

// exposureStatePrincipal is the caller a state read may answer: one that
// names a user, as the route admits.
func exposureStatePrincipal(r *http.Request) (auth.Principal, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || p.Legacy || p.TenantID == "" || p.Subject == "" {
		return auth.Principal{}, false
	}
	return p, true
}

func exposureStateNotFound(w http.ResponseWriter) {
	writeJSONError(w, http.StatusNotFound, "not_found", "no such page")
}

// handleExposureState serves GET /v1/workspaces/exposures/{id}.
func (s *Server) handleExposureState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := exposureStatePrincipal(r)
	if s.exposures == nil || !ok {
		exposureStateNotFound(w)
		return
	}
	e, err := s.exposures.x.ExposureState(r.Context(), p.TenantID, p.Subject, r.PathValue("id"))
	switch {
	case errors.Is(err, lifecycle.ErrExposureNotFound):
		exposureStateNotFound(w)
		return
	case err != nil:
		slog.WarnContext(r.Context(), "workspace exposure: cannot read the page's state", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "the page's state cannot be read now")
		return
	}
	writeJSON(w, http.StatusOK, s.exposures.stateOf(e))
}

// handleExposureList serves GET /v1/workspaces/exposures: the caller's live
// pages. A caller that names no user has none.
func (s *Server) handleExposureList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := []exposureState{}
	p, ok := exposureStatePrincipal(r)
	if s.exposures == nil || !ok {
		writeJSON(w, http.StatusOK, map[string]any{"exposures": out})
		return
	}
	list, err := s.exposures.x.UserExposures(r.Context(), p.TenantID, p.Subject)
	if err != nil {
		slog.WarnContext(r.Context(), "workspace exposure: cannot list the caller's pages", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "the pages cannot be listed now")
		return
	}
	for _, e := range list {
		out = append(out, s.exposures.stateOf(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"exposures": out})
}
