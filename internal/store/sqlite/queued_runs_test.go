package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A queued run's recorded request — which may hold sealed secrets — does not
// outlive its wait: the start clears it, and so do the end of a run cancelled
// while queued, the end of one that never started, and the failure of one a
// restart could not start.
func TestQueuedInput_ClearedWhenTheRunStartsOrEnds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "default", "u")
	if err != nil {
		t.Fatal(err)
	}
	ends := map[string]func(id string) error{
		"start": func(id string) error {
			_, err := s.StartQueuedRun(ctx, id, store.RunIdentity{})
			return err
		},
		"cancel": func(id string) error {
			return s.FinishRun(ctx, id, store.RunCancelled, "", store.Usage{}, "")
		},
		"fail at restart": func(id string) error {
			return s.FailQueuedRun(ctx, id, "restarted")
		},
		"end before start": func(id string) error {
			return s.EndQueuedRun(ctx, id, store.RunCancelled, "", "")
		},
	}
	for name, end := range ends {
		run, err := s.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_" + name[:4], UserID: "u", TenantID: "t"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetQueuedInput(ctx, run.ID, []byte(`{"secrets":{"key_id":"k"}}`)); err != nil {
			t.Fatal(err)
		}
		if err := end(run.ID); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var input sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT queued_input FROM runs WHERE id = ?`, run.ID).Scan(&input); err != nil {
			t.Fatal(err)
		}
		if input.Valid {
			t.Errorf("%s: queued_input = %q, want cleared", name, input.String)
		}
	}
}
