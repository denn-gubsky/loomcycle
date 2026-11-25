package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// child makes a run row of parent's user and tree and returns its Owner.
func (f *fixture) child(t *testing.T, parent Owner) Owner {
	t.Helper()
	c := f.run(t, parent.UserID)
	c.AgentID = "a_child_" + c.RunID
	c.RootRunID = parent.RootRunID
	return c
}

// noRowsOf asserts o holds no workspace row at all.
func (f *fixture) noRowsOf(t *testing.T, o Owner, why string) {
	t.Helper()
	rows, err := f.st.WorkspaceListByOwnerRun(context.Background(), o.TenantID, o.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("%s: %d rows were made", why, len(rows))
	}
}

// RFC GW §5.4 "For a fan-out": the parent's workspace is forked for the
// child, which owns the fork; it lands on the source's worker, and its
// routes are the source's that the child's grant also names (D-P6-17).
func TestForkForChild_TheForkIsTheChildsOnTheSourcesWorker(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	ctx := context.Background()
	parent := f.run(t, "alice")
	src, err := f.m.Open(ctx, parent, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"pypi", "pypi-files"}})
	if err != nil {
		t.Fatal(err)
	}
	child := f.child(t, parent)
	fork, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "work", Egress: []string{"npm", "pypi"}})
	if err != nil {
		t.Fatalf("fork for the child: %v", err)
	}
	if fork.Name != "work" || fork.OwnerRunID != child.RunID || fork.OwnerAgentID != child.AgentID ||
		fork.RootRunID != parent.RootRunID || fork.OwnerKind != store.WorkspaceOwnerRun || fork.TenantID != "acme" || fork.UserID != "alice" {
		t.Errorf("the fork's owner = %+v", fork)
	}
	if fork.WorkerID != src.WorkerID || fork.Engine != src.Engine || fork.ID == src.ID || fork.State != store.WorkspaceReady {
		t.Errorf("the fork = %+v, source %+v", fork, src)
	}
	if !reflect.DeepEqual(fork.Egress, []string{"pypi"}) {
		t.Errorf("the fork's routes = %v, want the source's within the child's grant", fork.Egress)
	}
	if got, err := f.m.Get(ctx, child, "work"); err != nil || got.ID != fork.ID {
		t.Errorf("the child's pin = %+v, %v", got, err)
	}
	if got, err := f.m.Get(ctx, parent, "work"); err != nil || got.ID != src.ID {
		t.Errorf("the parent's own workspace after the fork = %+v, %v", got, err)
	}
	named, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "work", As: "second"})
	if err != nil || named.Name != "second" || len(named.Egress) != 0 {
		t.Errorf("a second fork named by as, with no routes granted = %+v, %v", named, err)
	}
	if _, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "work"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a fork onto a name the child holds = %v, want ErrNameTaken", err)
	}
}

// The source is named against the parent's own pin: a workspace of another
// run (a sibling's, the child's own, another user's) is not found, and
// nothing is made for the child.
func TestForkForChild_RefusesASourceTheParentDoesNotOwn(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	ctx := context.Background()
	parent := f.run(t, "alice")
	child := f.child(t, parent)
	f.open(t, f.run(t, "bob"), "theirs")
	f.open(t, f.run(t, "alice"), "sibling")
	for _, name := range []string{"theirs", "sibling", "missing"} {
		if _, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: name}); !errors.Is(err, ErrNotFound) {
			t.Errorf("forking %q = %v, want ErrNotFound", name, err)
		}
	}
	f.noRowsOf(t, child, "refused forks")
	// The child's own workspace is not the parent's either.
	f.open(t, child, "own")
	if _, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "own", As: "copy"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("forking the child's own workspace as the parent = %v, want ErrNotFound", err)
	}
}

// A fork for a child never crosses a tenant or a user, never goes to or
// from a principal (RFC GW D35), and a run is not its own child
// (D-P6-16). Each is refused with nothing made.
func TestForkForChild_NeverCrossesATenantOrAUser(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	ctx := context.Background()
	parent := f.run(t, "alice")
	f.open(t, parent, "work")

	otherTenant := f.child(t, parent)
	otherTenant.TenantID = "globex"
	otherUser := f.child(t, parent)
	otherUser.UserID = "bob"
	principal := PrincipalOwner("acme", "alice", "tok-1")
	noRun := f.child(t, parent)
	noRun.RunID = ""
	for name, c := range map[string]Owner{"another tenant": otherTenant, "another user": otherUser, "a principal": principal, "no run": noRun, "itself": parent} {
		_, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: c, Name: "work", As: "copy"})
		if !errors.Is(err, errChildFork) {
			t.Errorf("a fork for %s = %v, want errChildFork", name, err)
		}
		if c.RunID != "" && c.RunID != parent.RunID {
			f.noRowsOf(t, c, "a fork for "+name)
		}
	}
	if _, err := f.m.ForkForChild(ctx, principal, ChildFork{Child: f.child(t, parent), Name: "work"}); !errors.Is(err, errChildFork) {
		t.Errorf("a fork by a principal = %v, want errChildFork", err)
	}
}

// The child's engine floor holds (D-P6-18), and the fork counts against the
// child's own workspace limit, not the parent's.
func TestForkForChild_HoldsTheChildsEngineFloorAndLimit(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 1}, worker.Options{})
	ctx := context.Background()
	parent := f.run(t, "alice")
	src := f.open(t, parent, "work")
	child := f.child(t, parent)
	if workspace.EngineRank(src.Engine) >= workspace.EngineRank(workspace.EngineContainer) {
		t.Fatalf("the fixture's worker runs %q; the test needs a weaker engine", src.Engine)
	}
	if _, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "work", Engine: workspace.EngineContainer}); !errors.Is(err, errChildFork) {
		t.Errorf("a fork below the child's engine floor = %v, want errChildFork", err)
	}
	f.noRowsOf(t, child, "a fork below the engine floor")
	// The parent holds its one; the child's first is still within its own.
	if _, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "work"}); err != nil {
		t.Fatalf("the child's first fork: %v", err)
	}
	if _, err := f.m.ForkForChild(ctx, parent, ChildFork{Child: child, Name: "work", As: "two"}); !errors.Is(err, ErrLimit) {
		t.Errorf("a fork past the child's max_per_run = %v, want ErrLimit", err)
	}
}
