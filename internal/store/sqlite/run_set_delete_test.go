package sqlite

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
	s := newTestStore(t)
	ctx := context.Background()
	_, run := finishedRun(t, s)

	for _, tc := range []struct{ name, column, value, restore string }{
		{"an unknown status", "status", "zombie", string(store.RunCompleted)},
		{"an unknown pause state", "pause_state", "half-paused", store.PauseStateRunning},
		{"an empty pause state", "pause_state", "", store.PauseStateRunning},
	} {
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET `+tc.column+` = ? WHERE id = ?`, tc.value, run.ID); err != nil {
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
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET `+tc.column+` = ? WHERE id = ?`, tc.restore, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.DeleteRunSet(ctx, []string{run.ID}, nil); err != nil || got.Runs != 1 {
		t.Errorf("with its state restored: deleted %d runs, err %v; want 1, nil", got.Runs, err)
	}
}

// A continuation that adds a run to the session while the delete is waiting
// keeps the session. The delete must decide "no run of this session is left"
// from the rows as they are when it holds the write lock, not from a read it
// took before the continuation committed.
func TestDeleteRunSet_KeepsASessionThatGainsARunWhileTheDeleteWaits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sess, run := finishedRun(t, s)

	writer, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO runs(id, session_id, status, started_at) VALUES ('r_continuation', ?, 'running', ?)`,
		sess.ID, time.Now().UnixNano()); err != nil {
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
		t.Fatalf("DeleteRunSet returned (%+v, %v) while another writer held the database", r.got, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := writer.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DeleteRunSet did not return after the other writer committed")
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
