package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func finishedRun(t *testing.T, s *Store) (store.Session, store.Run) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a", UserID: "alice", TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	return sess, run
}

// A stored state this build does not recognise is not a finished run. The
// delete refuses it and removes nothing; it must never read "not one of the
// live states I know" as "finished".
func TestDeleteRunSet_RefusesARunWhoseStoredStateItDoesNotKnow(t *testing.T) {
	fix := freshSchema(t, pgDSNFromEnv(t))
	defer fix.cleanup()
	s := fix.store.(*Store)
	ctx := context.Background()
	_, run := finishedRun(t, s)

	for _, tc := range []struct{ name, column, value, restore string }{
		{"an unknown status", "status", "zombie", string(store.RunCompleted)},
		{"an unknown pause state", "pause_state", "half-paused", store.PauseStateRunning},
		{"an empty pause state", "pause_state", "", store.PauseStateRunning},
	} {
		if _, err := s.Pool().Exec(ctx, `UPDATE runs SET `+tc.column+` = $1 WHERE id = $2`, tc.value, run.ID); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, err := s.DeleteRunSet(ctx, []string{run.ID}, nil)
		var live *store.ErrRunLive
		if !errors.As(err, &live) || live.RunID != run.ID {
			t.Errorf("%s: DeleteRunSet err = %v, want *store.ErrRunLive naming the run", tc.name, err)
		}
		if _, err := s.GetRun(ctx, run.ID); err != nil {
			t.Fatalf("%s: the run was deleted: %v", tc.name, err)
		}
		if _, err := s.Pool().Exec(ctx, `UPDATE runs SET `+tc.column+` = $1 WHERE id = $2`, tc.restore, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.DeleteRunSet(ctx, []string{run.ID}, nil); err != nil || got.Runs != 1 {
		t.Errorf("with its state restored: deleted %d runs, err %v; want 1, nil", got.Runs, err)
	}
}

// A continuation that adds a run to the session while the delete is in flight
// keeps the session, and its run. Without a lock on the session row taken
// BEFORE "no run of this session is left" is read, that read uses a snapshot
// from before the continuation committed; the session is then deleted, and the
// foreign key's cascade takes the continuation's live run with it.
func TestDeleteRunSet_KeepsASessionThatGainsARunWhileTheDeleteWaits(t *testing.T) {
	fix := freshSchema(t, pgDSNFromEnv(t))
	defer fix.cleanup()
	s := fix.store.(*Store)
	ctx := context.Background()
	sess, run := finishedRun(t, s)

	// The continuation: its run is inserted and not yet committed.
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO runs(id, session_id, status, started_at) VALUES ('r_continuation', $1, 'running', now())`,
		sess.ID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		got store.RunSetDeletion
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := s.DeleteRunSet(ctx, []string{run.ID}, nil)
		done <- result{got, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("DeleteRunSet returned (%+v, %v) before the continuation committed; it must wait for it", r.got, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DeleteRunSet did not return after the continuation committed")
	}
	if r.err != nil {
		t.Fatalf("DeleteRunSet: %v", r.err)
	}
	if r.got.Runs != 1 || r.got.Sessions != 0 {
		t.Errorf("deleted %d runs and %d sessions, want the 1 run and no session", r.got.Runs, r.got.Sessions)
	}
	if _, err := s.GetSession(ctx, sess.ID); err != nil {
		t.Errorf("the session was removed with a running run in it: %v", err)
	}
	if _, err := s.GetRun(ctx, "r_continuation"); err != nil {
		t.Errorf("the continuation's run was removed: %v", err)
	}
}

// A draft started while the delete is in flight is not deleted as a draft.
// The start moves the row configured → running; the delete locks the row
// before it reads the state, so it sees whichever committed first.
func TestDeleteRunSet_RefusesARunThatWentLiveWhileTheDeleteWaits(t *testing.T) {
	fix := freshSchema(t, pgDSNFromEnv(t))
	defer fix.cleanup()
	s := fix.store.(*Store)
	ctx := context.Background()
	_, run := finishedRun(t, s)

	// Another writer moves the run back to a live state and has not committed.
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE runs SET pause_state = 'paused' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.DeleteRunSet(ctx, []string{run.ID}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("DeleteRunSet returned %v before the other writer committed; it must wait for the row", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DeleteRunSet did not return after the other writer committed")
	}
	var live *store.ErrRunLive
	if !errors.As(err, &live) || live.RunID != run.ID {
		t.Errorf("DeleteRunSet err = %v, want *store.ErrRunLive: the run was paused before the delete read it", err)
	}
	if _, err := s.GetRun(ctx, run.ID); err != nil {
		t.Errorf("the paused run was deleted: %v", err)
	}
}
