package postgres

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DeleteRunSet: see the store.Store interface for the contract.
//
// The checks decide whether to delete, so each reads rows this transaction
// has ALREADY LOCKED, in a statement of its own. At READ COMMITTED a
// statement sees what was committed when it began; a check written into the
// locking statement itself can wait on another writer's lock and then act on
// what it saw before that writer committed. So: lock first, read after.
//
// Lock order is sessions, then runs, both by id. What the locks hold off:
//
//   - a session row lock blocks the insert of a new run into that session (a
//     continuation takes a key-share lock on it through the foreign key), so
//     "no run of this session is left" stays true until commit;
//   - a run row lock blocks every write to that run: a draft being started,
//     a run being paused or resumed.
//
// A run is spawned only by a live ancestor, and the walk the caller did
// reaches every descendant, so once every named run is locked and found
// finished, and no run outside the set names one inside it as parent, no
// child can appear under the set either.
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

	ids := func(what, query string, args ...any) ([]string, error) {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("delete run set: %s: %w", what, err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("delete run set: %s: %w", what, err)
		}
		return got, nil
	}

	// A run's session never changes, so this may be read before any lock.
	fromRuns, err := ids("sessions of the set", `SELECT DISTINCT session_id FROM runs WHERE id = ANY($1)`, runIDs)
	if err != nil {
		return out, err
	}
	candidates := store.DedupIDs(append(append([]string(nil), sessionIDs...), fromRuns...))
	if _, err := ids("lock sessions", `SELECT id FROM sessions WHERE id = ANY($1) ORDER BY id FOR UPDATE`, candidates); err != nil {
		return out, err
	}
	if _, err := ids("lock runs", `SELECT id FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, runIDs); err != nil {
		return out, err
	}

	// The locked rows' state. Read as nullable so a NULL reaches RunDeletable
	// as "" and is refused, rather than being coalesced into a state that
	// passes.
	rows, err := tx.Query(ctx, `SELECT id, status, pause_state FROM runs WHERE id = ANY($1) ORDER BY id`, runIDs)
	if err != nil {
		return out, fmt.Errorf("delete run set: read run states: %w", err)
	}
	live := ""
	for rows.Next() {
		var (
			id                 string
			status, pauseState *string
		)
		if err := rows.Scan(&id, &status, &pauseState); err != nil {
			rows.Close()
			return out, fmt.Errorf("delete run set: scan run state: %w", err)
		}
		if live == "" && (status == nil || pauseState == nil || !store.RunDeletable(*status, *pauseState)) {
			live = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("delete run set: read run states: %w", err)
	}
	if live != "" {
		return out, &store.ErrRunLive{RunID: live}
	}
	// The set must still be a whole tree.
	outside, err := ids("check the set is a whole tree",
		`SELECT id FROM runs WHERE parent_run_id = ANY($1) AND NOT (id = ANY($1)) ORDER BY id LIMIT 1`, runIDs)
	if err != nil {
		return out, err
	}
	if len(outside) > 0 {
		return out, &store.ErrRunLive{RunID: outside[0], Outside: true}
	}

	del := func(dst *int, what, query string, arg []string) error {
		tag, err := tx.Exec(ctx, query, arg)
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
	// holds several runs, and the ones outside this set keep it. A statement
	// of its own, after the session locks above: it sees every run committed
	// before those locks were granted, and none can be added since.
	out.RemovedSessionIDs, err = ids("find emptied sessions",
		`SELECT id FROM sessions WHERE id = ANY($1)
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.session_id = sessions.id)`, candidates)
	if err != nil {
		return out, err
	}
	sort.Strings(out.RemovedSessionIDs)
	if len(out.RemovedSessionIDs) == 0 {
		out.RemovedSessionIDs = nil // as the sqlite twin reports "none"
	} else {
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
