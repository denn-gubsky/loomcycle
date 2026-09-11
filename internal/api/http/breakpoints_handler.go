package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/breakpoints"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Live breakpoint arming for a team walk — GET/PUT /v1/runs/{run_id}/breakpoints.
//
// The run argument on `TeamDef op=run` covers the case where you already know
// the workflow is broken. It cannot cover the common one: you start a run
// expecting it to work, watch a wave go wrong, and want to stop before the next
// one. There is nothing to pass at that moment, so the arming has to be
// something the walk keeps re-reading and this endpoint can write to.
//
// Addressed by run_id, the same handle every other live-run surface uses
// (cancel, steer, interrupts) and the one the loomboard canvas already holds.

type breakpointsRequest struct {
	// Breakpoints is the WHOLE desired set, not a delta. The caller holds the
	// configuration and pushes it, so two operators cannot interleave a
	// read-modify-write, and "turn Debug off" is the empty list rather than a
	// second verb.
	Breakpoints []string `json:"breakpoints"`
}

type breakpointsResponse struct {
	RunID string `json:"run_id"`
	// Armed is canonical — always phase-qualified and sorted — so a caller
	// reading back its own arming sees what the walk will actually do rather
	// than an echo of its own shorthand.
	Armed []string `json:"armed"`
}

// handleGetRunBreakpoints serves GET /v1/runs/{run_id}/breakpoints.
func (s *Server) handleGetRunBreakpoints(w http.ResponseWriter, r *http.Request) {
	set, runID, ok := s.liveBreakpointSet(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, breakpointsResponse{RunID: runID, Armed: set.List()})
}

// handlePutRunBreakpoints serves PUT /v1/runs/{run_id}/breakpoints — replace
// the armed set of the team walk running under this run.
//
// It takes effect at the NEXT pause point the walk reaches: the before_dispatch
// of a wave that has not started, or the after_collection of one whose results
// have not gone out yet. It cannot un-publish a result the next stage has
// already seen, and does not pretend to.
func (s *Server) handlePutRunBreakpoints(w http.ResponseWriter, r *http.Request) {
	set, runID, ok := s.liveBreakpointSet(w, r)
	if !ok {
		return
	}
	var req breakpointsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return
	}
	// Replace validates before it mutates, so a rejected call leaves the
	// previous arming exactly as it was — an operator fixing a typo must not
	// discover they have also disarmed everything that was working.
	if err := set.Replace(req.Breakpoints); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_breakpoint", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, breakpointsResponse{RunID: runID, Armed: set.List()})
}

// liveBreakpointSet resolves the armed set for a run, or writes the refusal.
//
// A run the caller's tenant cannot see and a run with no walk in flight both
// fold into the SAME 404: run_ids are not secret (they are returned to callers
// and shown in the UI), so the gate must not become an existence oracle.
func (s *Server) liveBreakpointSet(w http.ResponseWriter, r *http.Request) (*breakpoints.Set, string, bool) {
	if s.breakpointReg == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "team breakpoints are not enabled on this server")
		return nil, "", false
	}
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_run_id", "run_id must match [A-Za-z0-9_-]{1,128}")
		return nil, "", false
	}
	if _, err := s.tenantStore(r.Context()).GetRun(r.Context(), runID); err != nil {
		writeJSONError(w, http.StatusNotFound, "no_live_walk", "no live team walk for that run_id")
		return nil, "", false
	}
	set, ok := s.breakpointReg.Get(runID)
	if !ok {
		// Single-process today: a walk on ANOTHER replica is not reachable from
		// here and reports the same miss. Loudly, never silently — an arming
		// that appeared to succeed and then never paused anything would be the
		// worst possible outcome for a debugger. Cross-replica owner-routing is
		// the same follow-on cancel and steer each took.
		writeJSONError(w, http.StatusNotFound, "no_live_walk",
			"no live team walk for that run_id on this replica")
		return nil, "", false
	}
	return set, runID, true
}

// openTeamBreakpoints is what TeamDef op=run calls to get its armed set. The
// run id comes from ctx, never from tool input — a caller must not be able to
// register its walk under someone else's run.
func (s *Server) openTeamBreakpoints(ctx context.Context, seed []string) (teamrun.BreakpointSource, func(), error) {
	set, release, err := s.breakpointReg.Open(tools.RunID(ctx), seed)
	if err != nil {
		return nil, nil, err
	}
	return liveBreakpoints{set}, release, nil
}

// liveBreakpoints adapts the registry's Set to what the walk asks.
//
// The adapter exists so internal/breakpoints stays a leaf: the walk's phase is
// a named string type in teamrun, and importing it into the registry would put
// the graph-walker inside the HTTP layer's dependency set for the sake of one
// conversion. The two phase spellings are pinned against each other by
// TestBreakpointPhases_MatchTheRegistrys.
type liveBreakpoints struct{ set *breakpoints.Set }

func (l liveBreakpoints) Armed(state string, phase teamrun.BreakpointPhase) bool {
	return l.set.Armed(state, string(phase))
}
