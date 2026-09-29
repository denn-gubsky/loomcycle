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
	if !runOwnershipOK(ctx, walk) {
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
		if runOwnershipOK(ctx, r) {
			out = append(out, r)
		}
	}
	return out, next, nil
}

// handleListRuns serves GET /v1/runs?walk_id=…&limit=…&cursor=…: one team
// walk's runs, oldest first, a page at a time. walk_id is required — there is
// no unfiltered listing of a deployment's runs. Rows are the user listing's
// (GET /v1/users/{user_id}/agents), with a running row's awaited state; the
// page's next_cursor is "" on the last page.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	walkID := q.Get("walk_id")
	if walkID == "" {
		writeJSONError(w, http.StatusBadRequest, "walk_id_required",
			"walk_id is required: GET /v1/runs lists one team walk's runs")
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
		out = append(out, runToAgentResponse(run, s.RunLive(run.AgentID, run.ID)))
	}
	fillAwaitedStateForRunning(r.Context(), s.store, out)
	writeJSON(w, http.StatusOK, map[string]any{"agents": out, "next_cursor": next})
}
