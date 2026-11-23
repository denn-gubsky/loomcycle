package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// Fork takes the source row it is handed only when that row is the
// caller's own: a row of another run, or of another tenant, is not found,
// whatever the caller read it from, and nothing is made.
func TestFork_RefusesASourceRowTheCallerDoesNotOwn(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	alice, bob := f.run(t, "alice"), f.run(t, "bob")
	src := f.open(t, alice, "work")
	ctx := context.Background()
	if _, _, err := f.m.Fork(ctx, bob, src, "stolen", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob forking alice's row = %v, want ErrNotFound", err)
	}
	other := alice
	other.TenantID = "globex"
	if _, _, err := f.m.Fork(ctx, other, src, "stolen", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant forking the row = %v, want ErrNotFound", err)
	}
	rows, _ := f.st.WorkspaceListByOwnerRun(ctx, "acme", bob.RunID)
	if len(rows) != 0 {
		t.Errorf("a refused fork made %d rows", len(rows))
	}
	fork, _, err := f.m.Fork(ctx, alice, src, "mine", "")
	if err != nil || fork.OwnerRunID != alice.RunID || fork.WorkerID != src.WorkerID {
		t.Errorf("the owner's fork = %+v, %v", fork, err)
	}
}
