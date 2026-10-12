package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// inJSON is an IN-list bound as ONE parameter (a JSON array of ids). A run
// tree can hold thousands of runs, and one `?` per id would both rebuild the
// statement per call and run into the bound-variable limit.
const inJSON = `(SELECT value FROM json_each(?))`

// DeleteRunSet: see the store.Store interface for the contract.
//
// BEGIN IMMEDIATE, on a pinned connection (the MemoryIncrement pattern: the
// driver has no per-transaction way to ask for it). The checks below decide
// whether to delete, so they must read the rows the delete will remove and no
// writer may get between the two. A deferred transaction would read under a
// shared lock and take the write lock only at the first DELETE; IMMEDIATE
// holds it from the first statement, and SQLite has one writer.
func (s *Store) DeleteRunSet(ctx context.Context, runIDs, sessionIDs []string) (store.RunSetDeletion, error) {
	var out store.RunSetDeletion
	runIDs, sessionIDs = store.DedupIDs(runIDs), store.DedupIDs(sessionIDs)
	if len(runIDs) == 0 && len(sessionIDs) == 0 {
		return out, nil
	}
	runsJSON, err := json.Marshal(runIDs)
	if err != nil {
		return out, err
	}
	runs := string(runsJSON)

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return out, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Every named run's state, and the session it belongs to. The columns are
	// read as nullable so a NULL reaches RunDeletable as "" and is refused,
	// rather than being coalesced into a state that passes.
	candidates := append([]string(nil), sessionIDs...)
	rows, err := conn.QueryContext(ctx,
		`SELECT id, session_id, status, pause_state FROM runs WHERE id IN `+inJSON+` ORDER BY id`, runs)
	if err != nil {
		return out, err
	}
	live := ""
	for rows.Next() {
		var (
			id, sessionID      string
			status, pauseState sql.NullString
		)
		if err := rows.Scan(&id, &sessionID, &status, &pauseState); err != nil {
			rows.Close()
			return out, err
		}
		if live == "" && !store.RunDeletable(status.String, pauseState.String) {
			live = id
		}
		candidates = append(candidates, sessionID)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if live != "" {
		return out, &store.ErrRunLive{RunID: live}
	}
	// The set must still be a whole tree: a child started under it after the
	// caller walked it would otherwise be left with a parent that is gone.
	var outside sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT MIN(id) FROM runs WHERE parent_run_id IN `+inJSON+` AND id NOT IN `+inJSON,
		runs, runs).Scan(&outside); err != nil {
		return out, err
	}
	if outside.Valid {
		return out, &store.ErrRunLive{RunID: outside.String, Outside: true}
	}

	del := func(dst *int, query string, arg string) error {
		res, err := conn.ExecContext(ctx, query, arg)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		*dst += int(n)
		return err
	}
	// Events before runs: they reference the run row. Explicit rather than by
	// FK cascade, so the result does not depend on the foreign_keys pragma.
	for _, d := range []struct {
		dst   *int
		query string
	}{
		{&out.Events, `DELETE FROM events WHERE run_id IN ` + inJSON},
		{&out.Interrupts, `DELETE FROM interrupts WHERE run_id IN ` + inJSON},
		{&out.Evaluations, `DELETE FROM evaluations WHERE run_id IN ` + inJSON},
		{&out.MemoryPending, `DELETE FROM memory_pending WHERE source_run_id IN ` + inJSON},
		{&out.Runs, `DELETE FROM runs WHERE id IN ` + inJSON},
	} {
		if err := del(d.dst, d.query, runs); err != nil {
			return out, err
		}
	}

	// A session goes only when no run of it is left: a continued session
	// holds several runs, and the ones outside this set keep it. Decided here,
	// from the rows as they are after the delete above and under the same
	// write lock, so a continuation cannot add a run in between.
	candsJSON, err := json.Marshal(store.DedupIDs(candidates))
	if err != nil {
		return out, err
	}
	rows, err = conn.QueryContext(ctx,
		`SELECT id FROM sessions WHERE id IN `+inJSON+`
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.session_id = sessions.id)`, string(candsJSON))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		out.RemovedSessionIDs = append(out.RemovedSessionIDs, id)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	sort.Strings(out.RemovedSessionIDs)
	if len(out.RemovedSessionIDs) > 0 {
		removedJSON, err := json.Marshal(out.RemovedSessionIDs)
		if err != nil {
			return out, err
		}
		for _, d := range []struct {
			dst   *int
			query string
		}{
			{&out.MemoryPending, `DELETE FROM memory_pending WHERE source_session_id IN ` + inJSON},
			{&out.Events, `DELETE FROM events WHERE session_id IN ` + inJSON},
			{&out.SessionEmbeddings, `DELETE FROM session_embeddings WHERE session_id IN ` + inJSON},
			{&out.Sessions, `DELETE FROM sessions WHERE id IN ` + inJSON},
		} {
			if err := del(d.dst, d.query, string(removedJSON)); err != nil {
				return out, err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return out, err
	}
	committed = true
	return out, nil
}
