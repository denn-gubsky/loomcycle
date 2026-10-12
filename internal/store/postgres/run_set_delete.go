package postgres

import (
	"context"
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DeleteRunSet: see the store.Store interface for the contract.
func (s *Store) DeleteRunSet(ctx context.Context, runIDs, sessionIDs []string) (store.RunSetDeletion, error) {
	var out store.RunSetDeletion
	runIDs, sessionIDs = store.DedupIDs(runIDs), store.DedupIDs(sessionIDs)
	if len(runIDs) == 0 && len(sessionIDs) == 0 {
		return out, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin delete-run-set tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	deletable := make([]string, 0, 5)
	for _, st := range store.DeletableRunStatuses() {
		deletable = append(deletable, string(st))
	}
	// FOR UPDATE: at READ COMMITTED a plain read would let a draft be started,
	// or a run be paused, between this check and the delete below. Locking the
	// rows makes that writer wait, then find them gone. The same pass collects
	// the sessions these runs belong to and refuses a live one.
	candidates := append([]string(nil), sessionIDs...)
	rows, err := tx.Query(ctx,
		`SELECT id, session_id,
		        (status <> ALL($2) OR COALESCE(pause_state, 'running') IN ('paused', 'pausing')) AS live
		 FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, runIDs, deletable)
	if err != nil {
		return out, fmt.Errorf("lock run set: %w", err)
	}
	var liveID string
	for rows.Next() {
		var (
			id, sessionID string
			live          bool
		)
		if err := rows.Scan(&id, &sessionID, &live); err != nil {
			rows.Close()
			return out, fmt.Errorf("scan run set: %w", err)
		}
		if live && liveID == "" {
			liveID = id
		}
		candidates = append(candidates, sessionID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("lock run set: %w", err)
	}
	if liveID != "" {
		return out, &store.ErrRunLive{RunID: liveID}
	}
	var outside string
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MIN(id), '') FROM runs WHERE parent_run_id = ANY($1) AND NOT (id = ANY($1))`,
		runIDs).Scan(&outside)
	if err != nil {
		return out, fmt.Errorf("check run set is a whole tree: %w", err)
	}
	if outside != "" {
		return out, &store.ErrRunLive{RunID: outside, Outside: true}
	}

	del := func(dst *int, what, query string, ids []string) error {
		tag, err := tx.Exec(ctx, query, ids)
		if err != nil {
			return fmt.Errorf("delete run set %s: %w", what, err)
		}
		*dst += int(tag.RowsAffected())
		return nil
	}
	// Events before runs: they reference the run row.
	for _, d := range []struct {
		dst         *int
		what, query string
	}{
		{&out.Events, "events", `DELETE FROM events WHERE run_id = ANY($1)`},
		{&out.Interrupts, "interrupts", `DELETE FROM interrupts WHERE run_id = ANY($1)`},
		{&out.Evaluations, "evaluations", `DELETE FROM evaluations WHERE run_id = ANY($1)`},
		{&out.MemoryPending, "memory_pending", `DELETE FROM memory_pending WHERE source_run_id = ANY($1)`},
		{&out.Runs, "runs", `DELETE FROM runs WHERE id = ANY($1)`},
	} {
		if err := del(d.dst, d.what, d.query, runIDs); err != nil {
			return out, err
		}
	}

	// A session goes only when no run of it is left: a continued session
	// holds several runs, and the ones outside this set keep it.
	rows, err = tx.Query(ctx,
		`SELECT id FROM sessions WHERE id = ANY($1)
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.session_id = sessions.id)
		 FOR UPDATE`, store.DedupIDs(candidates))
	if err != nil {
		return out, fmt.Errorf("find emptied sessions: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, fmt.Errorf("scan emptied session: %w", err)
		}
		out.RemovedSessionIDs = append(out.RemovedSessionIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("find emptied sessions: %w", err)
	}
	sort.Strings(out.RemovedSessionIDs)
	if len(out.RemovedSessionIDs) > 0 {
		for _, d := range []struct {
			dst         *int
			what, query string
		}{
			{&out.MemoryPending, "memory_pending", `DELETE FROM memory_pending WHERE source_session_id = ANY($1)`},
			{&out.Events, "events", `DELETE FROM events WHERE session_id = ANY($1)`},
			{&out.SessionEmbeddings, "session_embeddings", `DELETE FROM session_embeddings WHERE session_id = ANY($1)`},
			{&out.Sessions, "sessions", `DELETE FROM sessions WHERE id = ANY($1)`},
		} {
			if err := del(d.dst, d.what, d.query, out.RemovedSessionIDs); err != nil {
				return out, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit delete-run-set: %w", err)
	}
	return out, nil
}
