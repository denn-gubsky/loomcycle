package http

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

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
	// second verb. OMITTED (or null) leaves the arming as it is: a PUT carrying
	// only review_ttl_seconds used to decode as the empty list and disarm
	// everything. A pointer because the two must not read the same.
	Breakpoints *[]string `json:"breakpoints"`
	// ReviewTTLSeconds, when present, replaces the walk's review deadline: a
	// member hold nobody rules on within it ends rejected. 0 is no deadline,
	// as it is when the walk starts; omitted leaves the deadline as it is.
	// A pointer because those two must not read the same.
	ReviewTTLSeconds *int `json:"review_ttl_seconds,omitempty"`
}

type breakpointsResponse struct {
	RunID string `json:"run_id"`
	// Armed is canonical — always phase-qualified and sorted — so a caller
	// reading back its own arming sees what the walk will actually do rather
	// than an echo of its own shorthand.
	Armed []string `json:"armed"`
	// ReviewTTLSeconds is the deadline a member's hold beginning now gets;
	// 0 = none. Always present: 0 is an answer, not an absence.
	ReviewTTLSeconds int `json:"review_ttl_seconds"`
}

// breakpointsView renders a live set as the endpoint reports it.
func breakpointsView(runID string, set *breakpoints.Set) breakpointsResponse {
	return breakpointsResponse{RunID: runID, Armed: set.List(), ReviewTTLSeconds: int(set.ReviewTTL() / time.Second)}
}

// handleGetRunBreakpoints serves GET /v1/runs/{run_id}/breakpoints.
func (s *Server) handleGetRunBreakpoints(w http.ResponseWriter, r *http.Request) {
	set, runID, ok := s.liveBreakpointSet(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, breakpointsView(runID, set))
}

// handlePutRunBreakpoints serves PUT /v1/runs/{run_id}/breakpoints — replace
// the armed set of the team walk running under this run.
//
// It takes effect at the NEXT point the walk consults it: the before_dispatch
// of a wave that has not started, and — for "<state>:review" — each member run
// as it finishes its answer, so arming mid-wave holds the members still out.
// Disarming "<state>:review" releases that state's held members at once, as
// approved. It cannot un-publish a result the next stage has already seen, and
// does not pretend to.
//
// A review_ttl_seconds sent with the set applies from each member's NEXT hold.
// A hold already in progress keeps its deadline: it announced its expires_at
// when it began, and the person reviewing it may be working to that. Like the
// specs it lives only in memory — a member restored after a restart holds on
// the deadline its own run recorded, the one in force when it started.
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
	var reviewTTL *time.Duration
	if req.ReviewTTLSeconds != nil {
		ttl, err := breakpoints.ReviewTTLFromSeconds(*req.ReviewTTLSeconds)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_review_ttl", err.Error())
			return
		}
		reviewTTL = &ttl
	}
	if req.Breakpoints == nil {
		// Nothing about the arming was sent, so nothing is disarmed and no held
		// member is released; only the deadline, if one came, changes.
		if reviewTTL != nil {
			set.SetReviewTTL(*reviewTTL)
		}
		writeJSON(w, http.StatusOK, breakpointsView(runID, set))
		return
	}
	// Apply validates before it mutates — the syntax, and that each state is
	// one the walk has and of a kind that phase can arm — so a rejected call
	// leaves the previous arming exactly as it was, deadline included: an
	// operator fixing a typo must not discover they have also disarmed
	// everything that was working.
	if err := set.Apply(*req.Breakpoints, reviewTTL); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_breakpoint", err.Error())
		return
	}
	s.releaseDisarmedMembers(r.Context(), runID)
	writeJSON(w, http.StatusOK, breakpointsView(runID, set))
}

// releaseDisarmedMembers releases the walk's members held for review whose
// state's review the new set no longer arms, the way a retune disarm releases
// a single run. Without it they waited for their hold's next heartbeat, up to
// 30 seconds, to notice. That heartbeat stays the backstop for a member that
// finishes its answer as the set changes.
//
// Every member still armed is left held, and releaseHeldRun leaves a hold an
// agent_stop hook took: disarming review did not take it.
func (s *Server) releaseDisarmedMembers(ctx context.Context, walkRunID string) {
	for memberRunID, armed := range s.reviewMembers.of(walkRunID) {
		if !armed(ctx) {
			s.releaseHeldRun(ctx, memberRunID)
		}
	}
}

// walkReviewMembers maps a live walk's run_id → member run_id → the member's
// live review arming, for each member running under that walk. An entry lives
// exactly as long as its member's run.
type walkReviewMembers struct {
	mu sync.Mutex
	m  map[string]map[string]func(context.Context) bool
}

// add registers a member and returns its removal. A member with no walk run id
// has no breakpoint set anyone can change, so there is nothing to register.
func (w *walkReviewMembers) add(walkRunID, memberRunID string, armed func(context.Context) bool) func() {
	if walkRunID == "" || memberRunID == "" || armed == nil {
		return func() {}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]map[string]func(context.Context) bool{}
	}
	if w.m[walkRunID] == nil {
		w.m[walkRunID] = map[string]func(context.Context) bool{}
	}
	w.m[walkRunID][memberRunID] = armed
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.m[walkRunID], memberRunID)
		if len(w.m[walkRunID]) == 0 {
			delete(w.m, walkRunID)
		}
	}
}

// of returns a copy of a walk's members, so the caller reads their arming —
// which may write the run record — without holding the lock.
func (w *walkReviewMembers) of(walkRunID string) map[string]func(context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]func(context.Context) bool, len(w.m[walkRunID]))
	for id, armed := range w.m[walkRunID] {
		out[id] = armed
	}
	return out
}

// liveBreakpointSet resolves the armed set for a run, or writes the refusal.
//
// A run the caller's tenant cannot see, one an isolated member does not own,
// and a run with no walk in flight all fold into the SAME 404: run_ids are not secret (they are returned to callers
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
	run, err := s.tenantStore(r.Context()).GetRun(r.Context(), runID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "no_live_walk", "no live team walk for that run_id")
		return nil, "", false
	}
	if run.SessionID != "" {
		// The tenant read does not confine an isolated member to its own runs;
		// the session gate does, as it does for the review verb and a walk's
		// cancel. Disarming another user's walk would release their members
		// held for review.
		sess, serr := s.store.GetSession(r.Context(), run.SessionID)
		if serr != nil || !sessionOwnershipOK(r.Context(), sess) {
			writeJSONError(w, http.StatusNotFound, "no_live_walk", "no live team walk for that run_id")
			return nil, "", false
		}
	} else if !runOwnershipOK(r.Context(), run) {
		// No session to gate on: the run's own owner confines it instead.
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
// register its walk under someone else's run. targets is the walk's own check,
// which a PUT runs before it arms anything: this handler holds no definition.
// reviewTTL is the run argument's review deadline, which a PUT can change too.
func (s *Server) openTeamBreakpoints(ctx context.Context, seed []string, reviewTTL time.Duration, targets func(spec string) error) (teamrun.BreakpointSource, func(), error) {
	set, release, err := s.breakpointReg.Open(tools.RunID(ctx), seed, reviewTTL, targets)
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

// ReviewTTL hands the walk's members the deadline the set holds now, so a PUT
// that changes it applies from each member's next hold.
func (l liveBreakpoints) ReviewTTL() time.Duration { return l.set.ReviewTTL() }

var _ teamrun.ReviewDeadlineSource = liveBreakpoints{}
