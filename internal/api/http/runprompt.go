package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// redactPromptSnapshot masks registered secrets in a snapshot's text before it
// is persisted — the same treatment the tool_call / tool_result / context_state
// rows get. The live run is unaffected: the snapshot is never forwarded.
func redactPromptSnapshot(r *redact.Redactor, snap providers.PromptSnapshotInfo) providers.PromptSnapshotInfo {
	mask := func(blocks []providers.ContentBlock) []providers.ContentBlock {
		out := make([]providers.ContentBlock, len(blocks))
		copy(out, blocks)
		for i := range out {
			out[i].Text = r.String(out[i].Text)
		}
		return out
	}
	return providers.PromptSnapshotInfo{System: mask(snap.System), Input: mask(snap.Input)}
}

// handleGetRunPrompt serves GET /v1/runs/{run_id}/prompt (RFC DI): the prompt
// the run's first model call received — system blocks and the run's input —
// read from the run's prompt_snapshot event rather than re-derived, because
// {{...}} expansion is per-stage and unmemoised: re-assembling now could show a
// different prompt than the one the model saw.
//
// Gated like every run read that returns content (runOwnershipOK): another
// tenant's run, or another user's for an isolated member, is the opaque 404 a
// missing one gets. The snapshot can
// hold resolved {{document:}} / {{memory:}} content the caller could not read
// directly; that is the same content the run's transcript already holds, and
// the same runs:read boundary covers both.
func (s *Server) handleGetRunPrompt(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "run prompts require persistence (no store configured)", http.StatusServiceUnavailable)
		return
	}
	run, err := s.tenantStore(r.Context()).GetRun(r.Context(), runID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "unknown_run", "no run found for that run_id")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !runOwnershipOK(r.Context(), run) {
		writeJSONError(w, http.StatusNotFound, "unknown_run", "no run found for that run_id")
		return
	}
	snap, found, err := s.firstPromptSnapshot(r, runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		// A run that never reached a model call, or one that predates the
		// snapshot. Distinct from an unknown run so a caller can tell them apart.
		writeJSONError(w, http.StatusNotFound, "no_prompt_snapshot", "this run has no recorded prompt (it never reached a model call, or it predates prompt snapshots)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": runID,
		"system": snap.System,
		"input":  snap.Input,
	})
}

// firstPromptSnapshot scans the run's own events for its FIRST snapshot. A
// resumed run records another one on re-entry; the first is what the run was
// started with, which is the question this endpoint answers.
func (s *Server) firstPromptSnapshot(r *http.Request, runID string) (providers.PromptSnapshotInfo, bool, error) {
	const page = 200
	var after int64
	for {
		evs, err := s.store.GetRunEventsSince(r.Context(), runID, after, page)
		if err != nil {
			return providers.PromptSnapshotInfo{}, false, err
		}
		for _, ev := range evs {
			if ev.Type == string(providers.EventPromptSnapshot) {
				var decoded providers.Event
				if err := json.Unmarshal(ev.Payload, &decoded); err != nil || decoded.PromptSnapshot == nil {
					return providers.PromptSnapshotInfo{}, false, errors.New("prompt snapshot is unreadable")
				}
				return *decoded.PromptSnapshot, true, nil
			}
			after = ev.Seq
		}
		if len(evs) < page {
			return providers.PromptSnapshotInfo{}, false, nil
		}
	}
}
