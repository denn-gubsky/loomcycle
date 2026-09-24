package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// reviewRequest is the body of POST /v1/runs/{run_id}/review.
type reviewRequest struct {
	Decision string `json:"decision"`
	Feedback string `json:"feedback,omitempty"`
}

// ReviewRun delivers an operator's verdict to a run held for review: approve
// lets it complete on the held answer; reject with feedback sends the feedback
// as its next user turn, and it revises and is held again; reject with no
// feedback ends it rejected. source is resolved by the caller at its auth
// boundary, never taken from the wire.
//
// This is an operator act and is never offered as a tool: an agent must not be
// able to approve its own answer, or a sibling's.
//
// Gated on the tenant-scoped store rather than on the local steer registry, so
// a verdict sent to a replica that does not own the run still reaches it (the
// push below routes across replicas). A run the caller may not see is the same
// opaque ErrRunNotInFlight as one that does not exist.
func (s *Server) ReviewRun(ctx context.Context, runID, decision, feedback, source string) (bool, error) {
	if s.steerReg == nil || s.store == nil {
		return false, connector.ErrSteeringUnavailable
	}
	feedback = strings.TrimSpace(feedback)
	var kind string
	switch decision {
	case "approve":
		if feedback != "" {
			// Refused rather than dropped: feedback on an approval would be
			// read by nobody, and the caller should know that.
			return false, fmt.Errorf("%w: feedback is only read with reject", connector.ErrInvalidReviewDecision)
		}
		kind = steer.KindApprove
	case "reject":
		kind = steer.KindReject
	default:
		return false, connector.ErrInvalidReviewDecision
	}

	run, err := s.tenantStore(ctx).GetRun(ctx, runID)
	if err != nil {
		return false, connector.ErrRunNotInFlight
	}
	if run.SessionID != "" {
		// The tenant read above does not confine an isolated member to its own
		// runs; the session gate does, as it does for steering.
		sess, serr := s.store.GetSession(ctx, run.SessionID)
		if serr != nil || !sessionOwnershipOK(ctx, sess) {
			return false, connector.ErrRunNotInFlight
		}
	}
	if run.Status != store.RunRunning {
		return false, connector.ErrRunNotInFlight
	}
	// Held means the run's latest event says so. A hold re-announces itself
	// after anything it emits while waiting (a compaction), so this stays true
	// for as long as the run is held.
	last, err := s.store.GetLastEventForRun(ctx, runID)
	if err != nil || last.Type != string(providers.EventAwaitingReview) {
		return false, connector.ErrRunNotHeld
	}

	delivered, err := s.steerReg.Push(ctx, runID, steer.Message{
		Kind: kind, Text: feedback, Source: source, EnqueuedAt: time.Now(),
	})
	switch {
	case errors.Is(err, steer.ErrQueueFull):
		return false, connector.ErrSteerQueueFull
	case errors.Is(err, steer.ErrRunNotFound):
		return false, connector.ErrRunNotInFlight
	case err != nil:
		return false, err
	}
	return delivered, nil
}

// handleRunReview serves POST /v1/runs/{run_id}/review — an operator's verdict
// on a run held for review. 404 for a run that is not live (or not the
// caller's); 409 {code:"not_held"} for a live run that is not held; 400 for a
// decision other than approve/reject; 429 when the run's input queue is full.
func (s *Server) handleRunReview(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	var req reviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}
	source := store.InterruptResolvedByAPI
	if hasSessionCookie(r) {
		source = store.InterruptResolvedByWebUI
	}
	delivered, err := s.ReviewRun(r.Context(), runID, req.Decision, req.Feedback, source)
	switch {
	case errors.Is(err, connector.ErrInvalidReviewDecision):
		http.Error(w, strings.TrimPrefix(err.Error(), "connector: "), http.StatusBadRequest)
		return
	case errors.Is(err, connector.ErrRunNotHeld):
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": "not_held", "error": "the run is not held for review",
		})
		return
	case errors.Is(err, connector.ErrSteerQueueFull):
		w.Header().Set("Retry-After", "1")
		http.Error(w, "run input queue full; retry shortly", http.StatusTooManyRequests)
		return
	case errors.Is(err, connector.ErrRunNotInFlight):
		http.Error(w, "no in-flight run for that run_id", http.StatusNotFound)
		return
	case errors.Is(err, connector.ErrSteeringUnavailable):
		http.Error(w, "review is not enabled on this server", http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "decision": req.Decision, "delivered": delivered})
}
