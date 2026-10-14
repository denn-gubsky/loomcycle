package http

// run_tree_delete.go — delete one run tree: a run, every descendant run, and
// what is stored about them.
//
// Before this the only deletes were a whole subject (erasure) and an age (the
// retention sweeper). DeleteSessionCascade, which both use, removes exactly one
// session, and each sub-agent has its own — so neither could remove a tree.
//
// One core serves both HTTP routes, the gRPC RPC and the MCP tool, so the
// authorization and the set of tables do not depend on the transport.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// maxRunTreeRuns caps the tree one call deletes. The walk is one query per
// run and the delete is one transaction, so an unbounded tree would be an
// unbounded lock; a larger tree is deleted from its sub-trees.
const maxRunTreeRuns = 10000

// runTreeDerivedFactsBound caps the count of facts derived from the tree's
// sessions. A hit makes the count a floor.
const runTreeDerivedFactsBound = 5000

// runTreeTables are the row counts every answer carries, zero included.
var runTreeTables = []string{
	"runs", "events", "sessions", "session_embeddings",
	"interrupts", "evaluations", "memory_pending", "trace_index",
}

const runTreeKeptUsageLedger = "kept by design: token_usage and usage_archive are " +
	"accounting records (tokens, model, cost), not content, and stay as subject erasure leaves them"

// runTreeErr is a refusal from a run-tree delete.
type runTreeErr struct {
	status int
	code   string
	msg    string
	// runID is the live run a run_tree_live refusal names.
	runID string
	// scope is the scope an insufficient_scope refusal names.
	scope string
}

func (e *runTreeErr) Error() string { return e.msg }

// HTTPStatus lets gRPC and MCP map the refusal without importing this type
// (the compactErr / draftErr pattern).
func (e *runTreeErr) HTTPStatus() int { return e.status }

func runTreeRefusal(status int, code, format string, args ...any) *runTreeErr {
	return &runTreeErr{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

// SetRunDeleteAudit wires the sink run-tree deletes are recorded to. Nil (no
// audit path configured) leaves the delete available and unrecorded there: it
// is still logged, and unlike a subject erasure it removes what a retention
// sweep would remove anyway.
func (s *Server) SetRunDeleteAudit(sink audit.Sink) {
	s.runDeleteAudit = sink
}

// DeleteRunTree implements connector.Connector.
func (s *Server) DeleteRunTree(ctx context.Context, req connector.DeleteRunTreeRequest) (connector.RunTreeDeletion, error) {
	res := emptyRunTreeDeletion(req)
	if s.store == nil {
		return res, runTreeRefusal(http.StatusServiceUnavailable, "persistence_required", "deleting a run tree requires persistence (no Store configured)")
	}
	switch {
	case (req.RunID == "") == (req.SessionID == ""):
		return res, runTreeRefusal(http.StatusBadRequest, "invalid_request", "exactly one of run_id and session_id is required")
	case req.RunID != "" && !validIdent(req.RunID):
		return res, runTreeRefusal(http.StatusBadRequest, "invalid_run_id", "run_id must match [A-Za-z0-9_-]{1,128}")
	case req.SessionID != "" && !validIdent(req.SessionID):
		return res, runTreeRefusal(http.StatusBadRequest, "invalid_session_id", "session_id must match [A-Za-z0-9_-]{1,128}")
	}
	// Each transport gates on runs:delete before it gets here. Checked again
	// because this is the one place every transport passes through, and a
	// transport added later that forgot its gate would otherwise let a
	// runs:read token delete. No principal is open mode, which is an admin.
	if p, ok := auth.PrincipalFromContext(ctx); ok && !p.Legacy && !auth.HasScope(p.Scopes, auth.ScopeRunsDelete) {
		refusal := runTreeRefusal(http.StatusForbidden, "insufficient_scope", "deleting a run tree requires the %s scope", auth.ScopeRunsDelete)
		refusal.scope = auth.ScopeRunsDelete
		return res, refusal
	}

	tenant, out, err := s.deleteRunTree(ctx, req)
	s.recordRunTreeDelete(ctx, req, tenant, out, err)
	return out, err
}

func emptyRunTreeDeletion(req connector.DeleteRunTreeRequest) connector.RunTreeDeletion {
	res := connector.RunTreeDeletion{
		RunID:      req.RunID,
		SessionID:  req.SessionID,
		Counts:     make(map[string]int, len(runTreeTables)),
		Kept:       map[string]string{"usage_ledger": runTreeKeptUsageLedger},
		NotReached: []connector.NotReached{},
	}
	for _, t := range runTreeTables {
		res.Counts[t] = 0
	}
	return res
}

// deleteRunTree resolves the roots, walks the tree and deletes it. tenant is
// the tree's, "" when no root was found.
func (s *Server) deleteRunTree(ctx context.Context, req connector.DeleteRunTreeRequest) (string, connector.RunTreeDeletion, error) {
	res := emptyRunTreeDeletion(req)
	tenant, roots, sessionIDs, found, err := s.runTreeRoots(ctx, req)
	if err != nil {
		return "", res, err
	}
	if !found {
		// Unknown, already deleted, or not this caller's to read: one answer
		// for all three, and the answer a repeated call gets. A 404 for the
		// last two would make the first call's success an existence oracle
		// for the second caller.
		return "", res, nil
	}
	runs, err := s.collectRunTree(ctx, tenant, roots)
	if err != nil {
		return tenant, res, err
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}

	// Read before the delete: both are found through ids the delete removes.
	facts, factsFloor, factsErr := s.runTreeDerivedFacts(ctx, tenant, runs)
	snapshots, snapshotsErr := s.runTreeSnapshots(ctx, runs)

	del, err := s.store.DeleteRunSet(ctx, ids, sessionIDs)
	if err != nil {
		var live *store.ErrRunLive
		if errors.As(err, &live) {
			return tenant, res, &runTreeErr{
				status: http.StatusConflict, code: "run_tree_live", runID: live.RunID, msg: live.Error(),
			}
		}
		return tenant, res, fmt.Errorf("delete run tree: %w", err)
	}
	res.DeletedRuns = del.Runs
	res.Counts["runs"] = del.Runs
	res.Counts["events"] = del.Events
	res.Counts["sessions"] = del.Sessions
	res.Counts["session_embeddings"] = del.SessionEmbeddings
	res.Counts["interrupts"] = del.Interrupts
	res.Counts["evaluations"] = del.Evaluations
	res.Counts["memory_pending"] = del.MemoryPending
	if del.Runs == 0 && del.Sessions == 0 {
		return tenant, res, nil
	}

	traces, unattributed, traceErr := s.deleteRunTreeTraces(ctx, runs, del.RemovedSessionIDs)
	res.Counts["trace_index"] = traces
	switch {
	case traceErr != nil:
		// The rows are gone and cannot be found again from the run ids, so
		// this is said once, here. They are not kept for ever: an indexed
		// turn expires on the trace index's own window.
		res.NotReached = append(res.NotReached, connector.NotReached{
			Name: "trace_index",
			Reason: "the search index of conversation turns could not be fully cleared (" + traceErr.Error() +
				"); what is left expires with the trace index window",
		})
	case unattributed > 0:
		n := unattributed
		res.NotReached = append(res.NotReached, connector.NotReached{
			Name: "trace_index", Count: &n,
			Reason: "indexed turns written by a trace backfill do not record their run; in a session that " +
				"keeps other runs they cannot be told apart, so they stay until the trace index window expires them",
		})
	}

	snap := connector.NotReached{
		Name: "snapshots",
		Reason: "a stored runtime snapshot taken while one of these runs was paused holds a copy of its " +
			"transcript; delete such snapshots separately",
	}
	if snapshotsErr == nil {
		snap.Count = &snapshots
	}
	res.NotReached = append(res.NotReached, snap, connector.NotReached{
		Name: "retention_exports",
		Reason: "export files a retention or usage sweep already wrote for these sessions are files on " +
			"disk, outside the store; remove them from the export directory",
	})
	derived := connector.NotReached{
		Name: "derived_facts", CountIsFloor: factsFloor,
		Reason: "facts that memory consolidation derived from these sessions are stored as memory, not as " +
			"part of a run; they are removed through the Memory surface or a subject erasure",
	}
	if factsErr == nil {
		derived.Count = &facts
	}
	res.NotReached = append(res.NotReached, derived)
	return tenant, res, nil
}

// runTreeRoots resolves the request to the runs the walk starts from. found is
// false when there is nothing the caller may delete: no such run or session,
// or one it cannot read.
func (s *Server) runTreeRoots(ctx context.Context, req connector.DeleteRunTreeRequest) (tenant string, roots []store.Run, sessionIDs []string, found bool, err error) {
	var nf *store.ErrNotFound
	ts := s.tenantStore(ctx)
	if req.RunID != "" {
		// The gate the run reads use: the tenant accessor, then the row rule
		// that confines an isolated member to its own runs.
		run, gerr := ts.GetRun(ctx, req.RunID)
		switch {
		case errors.As(gerr, &nf):
			return "", nil, nil, false, nil
		case gerr != nil:
			return "", nil, nil, false, fmt.Errorf("delete run tree: %w", gerr)
		case !runOwnershipOK(ctx, run):
			return "", nil, nil, false, nil
		}
		return run.TenantID, []store.Run{run}, nil, true, nil
	}
	sess, gerr := s.store.GetSession(ctx, req.SessionID)
	switch {
	case errors.As(gerr, &nf):
		return "", nil, nil, false, nil
	case gerr != nil:
		return "", nil, nil, false, fmt.Errorf("delete session: %w", gerr)
	case !ts.visible(sess.TenantID) || !sessionOwnershipOK(ctx, sess):
		return "", nil, nil, false, nil
	}
	roots, gerr = s.store.RunsForSession(ctx, sess.ID)
	if gerr != nil {
		return "", nil, nil, false, fmt.Errorf("delete session: %w", gerr)
	}
	return sess.TenantID, roots, []string{sess.ID}, true, nil
}

// collectRunTree walks parent_run_id down from roots, level by level, and
// returns every run reached, each once.
func (s *Server) collectRunTree(ctx context.Context, tenant string, roots []store.Run) ([]store.Run, error) {
	seen := make(map[string]struct{}, len(roots))
	out := make([]store.Run, 0, len(roots))
	queue := append([]store.Run(nil), roots...)
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		// parent_run_id is written by the runtime, and a run cannot be its own
		// ancestor; the set is what keeps a corrupt row from looping the walk.
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		// A child runs in its parent's tenant by construction. Refused rather
		// than trusted: the caller was authorized against the root's tenant
		// only, and this is a delete.
		if r.TenantID != tenant {
			return nil, runTreeRefusal(http.StatusConflict, "run_tree_crosses_tenants",
				"run %s under this tree belongs to another tenant; nothing was deleted", r.ID)
		}
		if len(out) == maxRunTreeRuns {
			return nil, runTreeRefusal(http.StatusUnprocessableEntity, "run_tree_too_large",
				"the tree holds more than %d runs; delete its sub-trees first", maxRunTreeRuns)
		}
		out = append(out, r)
		children, err := s.store.ListRunsByParentRunID(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("delete run tree: list children of %s: %w", r.ID, err)
		}
		queue = append(queue, children...)
	}
	return out, nil
}

// runTreeDerivedFacts counts the memory rows consolidation derived from the
// tree's sessions. They are not deleted: a fact is filed under whatever scope
// it is about, and removing it is a decision about that scope.
func (s *Server) runTreeDerivedFacts(ctx context.Context, tenant string, runs []store.Run) (n int, floor bool, err error) {
	seen := map[string]struct{}{}
	var sessions []string
	for _, r := range runs {
		if _, dup := seen[r.SessionID]; !dup && r.SessionID != "" {
			seen[r.SessionID] = struct{}{}
			sessions = append(sessions, r.SessionID)
		}
	}
	// In chunks: the store binds one parameter per session id.
	const chunk = 500
	for len(sessions) > 0 && n < runTreeDerivedFactsBound {
		c := sessions
		if len(c) > chunk {
			c = c[:chunk]
		}
		sessions = sessions[len(c):]
		rows, lerr := s.store.MemoryListBySourceSessions(ctx, tenant, c, runTreeDerivedFactsBound-n)
		if lerr != nil {
			return 0, false, lerr
		}
		n += len(rows)
	}
	return n, n >= runTreeDerivedFactsBound, nil
}

// runTreeSnapshots counts the stored snapshots taken while a run of the tree
// was in flight — the only time a snapshot can have captured its transcript.
func (s *Server) runTreeSnapshots(ctx context.Context, runs []store.Run) (int, error) {
	var from, to time.Time
	for _, r := range runs {
		if r.CompletedAt.IsZero() {
			continue // a draft never ran, so no snapshot holds it
		}
		if from.IsZero() || r.StartedAt.Before(from) {
			from = r.StartedAt
		}
		if r.CompletedAt.After(to) {
			to = r.CompletedAt
		}
	}
	if from.IsZero() {
		return 0, nil
	}
	list, err := s.store.SnapshotList(ctx, "", 1000)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, snap := range list {
		if !snap.CreatedAt.Before(from) && !snap.CreatedAt.After(to) {
			n++
		}
	}
	return n, nil
}

// deleteRunTreeTraces removes the trace-index rows of the deleted runs.
//
// A row is keyed `trace.turn:<session_id>:…` in the run's user scope, and
// carries its run id in the value. So a removed session's rows are its whole
// key prefix, and a surviving session's are the rows under its prefix whose
// value names a deleted run. unattributed counts the rows of a surviving
// session that name no run at all (a backfill writes those): they are left.
func (s *Server) deleteRunTreeTraces(ctx context.Context, runs []store.Run, removedSessions []string) (deleted, unattributed int, err error) {
	removed := make(map[string]bool, len(removedSessions))
	for _, id := range removedSessions {
		removed[id] = true
	}
	inTree := make(map[string]bool, len(runs))
	type scope struct{ tenant, user, session string }
	var scopes []scope
	seen := map[scope]bool{}
	for _, r := range runs {
		inTree[r.ID] = true
		// The index files a turn only under a user and a session.
		if strings.TrimSpace(r.UserID) == "" || r.SessionID == "" {
			continue
		}
		sc := scope{r.TenantID, r.UserID, r.SessionID}
		if !seen[sc] {
			seen[sc] = true
			scopes = append(scopes, sc)
		}
	}
	for _, sc := range scopes {
		prefix := store.TraceTurnKeyPrefix + sc.session + ":"
		after := ""
		for {
			page, lerr := s.store.MemoryListAfter(ctx, sc.tenant, store.MemoryScopeUser, sc.user, prefix, after, 1000)
			if lerr != nil {
				return deleted, unattributed, lerr
			}
			for _, e := range page {
				after = e.Key
				// Re-checked here rather than trusted to the store's prefix
				// match: a session id holds `_`, which a LIKE reads as "any
				// character", and this loop deletes.
				if !strings.HasPrefix(e.Key, prefix) {
					continue
				}
				if !removed[sc.session] {
					var v traceTurnValue
					if json.Unmarshal(e.Value, &v) != nil || v.RunID == "" {
						unattributed++
						continue
					}
					if !inTree[v.RunID] {
						continue
					}
				}
				ok, derr := s.store.MemoryDelete(ctx, sc.tenant, store.MemoryScopeUser, sc.user, e.Key)
				if derr != nil {
					return deleted, unattributed, derr
				}
				if ok {
					deleted++
				}
			}
			if len(page) < 1000 {
				break
			}
		}
	}
	return deleted, unattributed, nil
}

// recordRunTreeDelete writes the one audit record a call gets, and logs it.
// Ids and counts only.
func (s *Server) recordRunTreeDelete(ctx context.Context, req connector.DeleteRunTreeRequest, tenant string, res connector.RunTreeDeletion, callErr error) {
	ev := audit.Event{
		Action: "run_tree_delete", TargetTenant: tenant,
		DeleteRunID: req.RunID, DeleteSessionID: req.SessionID,
		SourceAddr: req.SourceAddr,
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	var rte *runTreeErr
	switch {
	case errors.As(callErr, &rte):
		ev.DeleteRefused = rte.code
	case callErr != nil:
		ev.DeleteRefused = "internal"
	default:
		ev.DeleteCounts = res.Counts
	}
	log.Printf("run tree delete: run=%q session=%q tenant=%q actor=%s/%s refused=%q counts=%v",
		req.RunID, req.SessionID, tenant, ev.ActorTenant, ev.ActorSubject, ev.DeleteRefused, ev.DeleteCounts)
	if s.runDeleteAudit == nil {
		return
	}
	if err := s.runDeleteAudit.Record(ev); err != nil {
		// Logged, not returned: the rows are gone either way.
		log.Printf("audit: run_tree_delete record failed (run=%q session=%q): %v", req.RunID, req.SessionID, err)
	}
}

// writeRunTreeErr writes a run-tree delete's refusal as the JSON error body
// the other run routes use; a run_tree_live refusal also names the run, and an
// insufficient_scope refusal the scope.
func writeRunTreeErr(w http.ResponseWriter, err error) {
	var rte *runTreeErr
	if !errors.As(err, &rte) {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if rte.scope != "" {
		writeScopeError(w, rte.scope, rte.msg)
		return
	}
	if rte.runID == "" {
		writeJSONError(w, rte.status, rte.code, rte.msg)
		return
	}
	writeJSONErrorBody(w, rte.status, struct {
		Code  string `json:"code"`
		Error string `json:"error"`
		RunID string `json:"run_id"`
	}{rte.code, rte.msg, rte.runID})
}

// handleDeleteRunTree serves DELETE /v1/runs/{run_id}/tree: the run and every
// descendant run. (DELETE /v1/runs/{run_id}, without /tree, discards a draft.)
func (s *Server) handleDeleteRunTree(w http.ResponseWriter, r *http.Request) {
	res, err := s.DeleteRunTree(r.Context(), connector.DeleteRunTreeRequest{
		RunID: r.PathValue("run_id"), SourceAddr: r.RemoteAddr,
	})
	if err != nil {
		writeRunTreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleDeleteSession serves DELETE /v1/sessions/{session_id}?cascade=children:
// every run of the session, each with its descendants. The parameter is
// required: a delete that left sub-agent sessions behind is the gap this
// closes, and no route offers it.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("cascade") != "children" {
		writeJSONError(w, http.StatusBadRequest, "cascade_required",
			"deleting a session deletes its runs' descendants too: pass ?cascade=children")
		return
	}
	res, err := s.DeleteRunTree(r.Context(), connector.DeleteRunTreeRequest{
		SessionID: r.PathValue("id"), SourceAddr: r.RemoteAddr,
	})
	if err != nil {
		writeRunTreeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
