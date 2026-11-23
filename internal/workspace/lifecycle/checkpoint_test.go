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

// A workspace opened over the MCP server belongs to a token principal (RFC GW
// D35), not to a run. Its fork is the principal's too, carrying the owner kind
// and the opening token, so the run sweeper never reads it as a run whose row
// is gone. A run owner whose pin key happens to equal the principal's is a
// different owner and finds nothing.
func TestFork_APrincipalsForkIsThePrincipalsAndKindsDoNotMix(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	ctx := context.Background()
	p := PrincipalOwner("acme", "alice", "tok-1")
	src := f.open(t, p, "pw")
	fork, _, err := f.m.Fork(ctx, p, src, "pw-b", "")
	if err != nil {
		t.Fatalf("the principal's fork: %v", err)
	}
	if fork.OwnerKind != src.OwnerKind || fork.OwnerRunID != p.RunID || fork.OpenedToken != "tok-1" {
		t.Errorf("the fork = kind %q pin %q token %q, want the principal's", fork.OwnerKind, fork.OwnerRunID, fork.OpenedToken)
	}
	forged := f.run(t, "alice")
	forged.RunID = p.RunID
	if _, _, err := f.m.Fork(ctx, forged, src, "stolen", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a run owner with the principal's pin key forking = %v, want ErrNotFound", err)
	}
}
