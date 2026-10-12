package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// inJSON is an IN-list bound as ONE parameter (a JSON array of ids). A run
// tree can hold thousands of runs, and one `?` per id would both rebuild the
// statement per call and run into the bound-variable limit.
const inJSON = `(SELECT value FROM json_each(?))`

// DeleteRunSet: see the store.Store interface for the contract.
func (s *Store) DeleteRunSet(ctx context.Context, runIDs, sessionIDs []string) (store.RunSetDeletion, error) {
	var out store.RunSetDeletion
	runIDs, sessionIDs = store.DedupIDs(runIDs), store.DedupIDs(sessionIDs)
	if len(runIDs) == 0 && len(sessionIDs) == 0 {
		return out, nil
	}
	runs, err := json.Marshal(runIDs)
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	// Both refusals read inside the transaction that deletes, so a run that
	// went live, or a child that was started, after the caller walked the
	// tree cannot be removed from under itself.
	deletable := store.DeletableRunStatuses()
	args := []any{string(runs)}
	for _, st := range deletable {
		args = append(args, string(st))
	}
	var live string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM runs WHERE id IN `+inJSON+`
		   AND (status NOT IN (`+placeholders(len(deletable))+`)
		        OR COALESCE(pause_state, 'running') IN ('paused', 'pausing'))
		 ORDER BY id LIMIT 1`, args...).Scan(&live)
	if err == nil {
		return out, &store.ErrRunLive{RunID: live}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM runs WHERE parent_run_id IN `+inJSON+` AND id NOT IN `+inJSON+`
		 ORDER BY id LIMIT 1`, string(runs), string(runs)).Scan(&live)
	if err == nil {
		return out, &store.ErrRunLive{RunID: live, Outside: true}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}

	// The sessions these runs belong to, read before the runs go.
	candidates := append([]string(nil), sessionIDs...)
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM runs WHERE id IN `+inJSON, string(runs))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		candidates = append(candidates, id)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	del := func(dst *int, query string, arg string) error {
		res, err := tx.ExecContext(ctx, query, arg)
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
		if err := del(d.dst, d.query, string(runs)); err != nil {
			return out, err
		}
	}

	// A session goes only when no run of it is left: a continued session
	// holds several runs, and the ones outside this set keep it.
	cands, err := json.Marshal(store.DedupIDs(candidates))
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx,
		`SELECT id FROM sessions WHERE id IN `+inJSON+`
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.session_id = sessions.id)`, string(cands))
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
		removed, err := json.Marshal(out.RemovedSessionIDs)
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
			if err := del(d.dst, d.query, string(removed)); err != nil {
				return out, err
			}
		}
	}
	return out, tx.Commit()
}
