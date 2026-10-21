package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// maxWalkRunsPage bounds one page of a walk's runs; the store caps at the same.
const maxWalkRunsPage = 1000

// errWalkIDInvalid is a walk id no run could carry. Transports map it to their
// bad-request answer.
var errWalkIDInvalid = errors.New("walk_id must match [A-Za-z0-9_-]{1,128}")

// walkRuns reads one page of a team walk's runs — the walk's own run and every
// member it spawned — for the principal on ctx. Shared by GET /v1/runs and the
// connector's ListWalkRuns (MCP list_runs), so both apply one gate.
//
// The gate is the single-run read's: the caller must be allowed to read the
// walk's OWN run (tenantStore folds another tenant's walk into ErrNotFound,
// runOwnershipOK folds another user's walk for an isolated member into the
// same), so a walk the caller cannot read is indistinguishable from one that
// does not exist. Each member is then held to the same row rule, which for
// anyone who passed the walk's gate drops nothing a walk spawns under its own
// caller.
func (s *Server) walkRuns(ctx context.Context, walkID string, limit int, cursor string) ([]store.Run, string, error) {
	if !validIdent(walkID) {
		return nil, "", errWalkIDInvalid
	}
	if cursor != "" {
		if _, _, err := store.DecodeRunCursor(cursor); err != nil {
			return nil, "", err
		}
	}
	ts := s.tenantStore(ctx)
	walk, err := ts.GetRun(ctx, walkID)
	if err != nil {
		return nil, "", err
	}
	// A token limited to certain agents reads only walks in trees it could
	// have started. Members are in the walk's own tree, so each passes the
	// per-row check below from what the tree has already read.
	tree := s.runTree(ctx)
	if !runOwnershipOK(ctx, walk) || !tree.ReachesKnown(runLink(walk)) {
		return nil, "", &store.ErrNotFound{Kind: "run", ID: walkID}
	}
	queryTenant := ts.tenantID
	if ts.allTenants {
		queryTenant = ""
	}
	rows, next, err := s.store.ListRunsByWalk(ctx, queryTenant, walkID, limit, cursor)
	if err != nil {
		return nil, "", err
	}
	out := rows[:0]
	for _, r := range rows {
		if runOwnershipOK(ctx, r) && tree.ReachesKnown(runLink(r)) {
			out = append(out, r)
		}
	}
	return out, next, nil
}

// errParentRunIDInvalid is childRuns' refusal of a malformed parent run id.
var errParentRunIDInvalid = errors.New("parent_run_id must match [A-Za-z0-9_-]{1,128}")

// childRuns is one page of a run's direct children, as the caller may see
// them. It backs GET /v1/runs?parent_run_id= and the connector's
// ListChildRuns (MCP list_runs), so both apply one gate.
//
// The gate is walkRuns': the caller must be allowed to read the PARENT run,
// so a run the caller cannot read is indistinguishable from one that does not
// exist, and each child is then held to the same row rule.
func (s *Server) childRuns(ctx context.Context, parentRunID string, limit int, cursor string) ([]store.Run, string, error) {
	if !validIdent(parentRunID) {
		return nil, "", errParentRunIDInvalid
	}
	if cursor != "" {
		if _, _, err := store.DecodeRunCursor(cursor); err != nil {
			return nil, "", err
		}
	}
	ts := s.tenantStore(ctx)
	parent, err := ts.GetRun(ctx, parentRunID)
	if err != nil {
		return nil, "", err
	}
	// The allowlist gate as walkRuns': a run's children are in its tree.
	tree := s.runTree(ctx)
	if !runOwnershipOK(ctx, parent) || !tree.ReachesKnown(runLink(parent)) {
		return nil, "", &store.ErrNotFound{Kind: "run", ID: parentRunID}
	}
	queryTenant := ts.tenantID
	if ts.allTenants {
		queryTenant = ""
	}
	rows, next, err := s.store.ListChildRuns(ctx, queryTenant, parentRunID, limit, cursor)
	if err != nil {
		return nil, "", err
	}
	out := rows[:0]
	for _, r := range rows {
		if runOwnershipOK(ctx, r) && tree.ReachesKnown(runLink(r)) {
			out = append(out, r)
		}
	}
	return out, next, nil
}

// handleListRuns serves GET /v1/runs with exactly one of two filters, oldest
// first, a page at a time (&limit=…&cursor=…):
//
//   - ?walk_id=… — one team walk's runs: the walk's own run, its members and
//     the runs under them.
//   - ?parent_run_id=… — one run's direct children, of every status. Not the
//     run itself, and not its grandchildren.
//
// A filter is required — there is
// no unfiltered listing of a deployment's runs. Rows are the user listing's
// (GET /v1/users/{user_id}/agents), with a running row's awaited state; the
// page's next_cursor is "" on the last page. The walk's OWN row also carries
// its result — final text and the end state reached — which is what a caller
// listing a walk is asking; members' rows leave theirs out, as any listing
// does.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	walkID, parentRunID := q.Get("walk_id"), q.Get("parent_run_id")
	switch {
	case walkID == "" && parentRunID == "":
		// The code predates parent_run_id; kept so a client matching on it
		// still does.
		writeJSONError(w, http.StatusBadRequest, "walk_id_required",
			"walk_id or parent_run_id is required: GET /v1/runs lists one team walk's runs, or one run's children")
		return
	case walkID != "" && parentRunID != "":
		writeJSONError(w, http.StatusBadRequest, "one_filter_only",
			"pass walk_id or parent_run_id, not both")
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxWalkRunsPage {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit",
				fmt.Sprintf("limit must be an integer 1..%d", maxWalkRunsPage))
			return
		}
		limit = n
	}
	if parentRunID != "" {
		s.listChildRuns(w, r, parentRunID, limit, q.Get("cursor"))
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusNotFound, "unknown_walk_id", "listing a walk's runs requires persistence (no store configured)")
		return
	}
	runs, next, err := s.walkRuns(r.Context(), walkID, limit, q.Get("cursor"))
	if err != nil {
		var nf *store.ErrNotFound
		switch {
		case errors.Is(err, errWalkIDInvalid):
			writeJSONError(w, http.StatusBadRequest, "invalid_walk_id", err.Error())
		case errors.Is(err, store.ErrInvalidRunCursor):
			writeJSONError(w, http.StatusBadRequest, "invalid_cursor",
				"cursor must be a next_cursor this endpoint returned")
		case errors.As(err, &nf):
			// walkID passed validIdent, so it needs no quoting inside the message.
			writeJSONError(w, http.StatusNotFound, "unknown_walk_id", "no walk found for walk_id "+walkID)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	out := make([]agentResponse, 0, len(runs))
	for _, run := range runs {
		a := runToAgentResponse(run, s.RunLive(run.AgentID, run.ID))
		if run.ID == walkID {
			a.Result = run.Result
		}
		out = append(out, a)
	}
	fillAwaitedStateForRunning(r.Context(), s.store, out)
	writeJSON(w, http.StatusOK, map[string]any{"agents": out, "next_cursor": next})
}

// listChildRuns answers GET /v1/runs?parent_run_id=: the run's direct
// children. No row carries a result, as in any listing: the walk's own row is
// the one exception, and it is never its own child.
func (s *Server) listChildRuns(w http.ResponseWriter, r *http.Request, parentRunID string, limit int, cursor string) {
	if s.store == nil {
		writeJSONError(w, http.StatusNotFound, "unknown_parent_run_id", "listing a run's children requires persistence (no store configured)")
		return
	}
	runs, next, err := s.childRuns(r.Context(), parentRunID, limit, cursor)
	if err != nil {
		var nf *store.ErrNotFound
		switch {
		case errors.Is(err, errParentRunIDInvalid):
			writeJSONError(w, http.StatusBadRequest, "invalid_parent_run_id", err.Error())
		case errors.Is(err, store.ErrInvalidRunCursor):
			writeJSONError(w, http.StatusBadRequest, "invalid_cursor",
				"cursor must be a next_cursor this endpoint returned")
		case errors.As(err, &nf):
			// parentRunID passed validIdent, so it needs no quoting.
			writeJSONError(w, http.StatusNotFound, "unknown_parent_run_id", "no run found for parent_run_id "+parentRunID)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	out := make([]agentResponse, 0, len(runs))
	for _, run := range runs {
		out = append(out, runToAgentResponse(run, s.RunLive(run.AgentID, run.ID)))
	}
	fillAwaitedStateForRunning(r.Context(), s.store, out)
	writeJSON(w, http.StatusOK, map[string]any{"agents": out, "next_cursor": next})
}
