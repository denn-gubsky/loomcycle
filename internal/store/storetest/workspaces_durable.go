package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P12 (plan §3.1, PR 1): durable rows, pinned or parked.

func mkDurable(id, tenant, user, ownerRun, name string) store.WorkspaceRow {
	r := mkWorkspace(id, tenant, ownerRun, name, "w1")
	r.UserID = user
	r.Lifetime = store.WorkspaceLifetimeDurable
	return r
}

func isConflict(err error) bool {
	var c *store.ErrConflict
	return errors.As(err, &c)
}

// A durable name is one per (tenant, user): a second durable row of it
// conflicts, an ephemeral row of the name and another user's or tenant's
// durable row do not; the lookup answers only the asker's own.
func testWorkspaceDurableNameIsUniquePerUser(t *testing.T, s store.Store) {
	ctx := context.Background()
	d := createWorkspace(t, s, mkDurable("ws_d1", "acme", "alice", "run-1", "work"))
	if d.Lifetime != store.WorkspaceLifetimeDurable {
		t.Fatalf("lifetime = %q", d.Lifetime)
	}
	if _, err := s.WorkspaceCreate(ctx, mkDurable("ws_d2", "acme", "alice", "run-2", "work")); !isConflict(err) {
		t.Errorf("a second durable row of alice's name: %v, want a conflict", err)
	}
	if _, err := s.WorkspaceCreate(ctx, mkDurable("ws_nouser", "acme", "", "run-9", "solo")); err == nil {
		t.Error("a durable row with no user was created")
	}
	eph := mkWorkspace("ws_e", "acme", "run-3", "work", "w1")
	createWorkspace(t, s, eph)
	createWorkspace(t, s, mkDurable("ws_bob", "acme", "bob", "run-4", "work"))
	createWorkspace(t, s, mkDurable("ws_beta", "beta", "alice", "run-5", "work"))

	got, err := s.WorkspaceGetDurable(ctx, "acme", "alice", "work")
	if err != nil || got.ID != "ws_d1" {
		t.Errorf("alice's durable work = %s, %v", got.ID, err)
	}
	for _, c := range []struct{ tenant, user, name string }{
		{"acme", "carol", "work"}, {"acme", "alice", "other"}, {"gamma", "alice", "work"}, {"acme", "", "work"},
	} {
		if _, err := s.WorkspaceGetDurable(ctx, c.tenant, c.user, c.name); !isNotFound(err) {
			t.Errorf("durable %v: %v, want not found", c, err)
		}
	}
	if got, err := s.WorkspaceGetDurable(ctx, "beta", "alice", "work"); err != nil || got.ID != "ws_beta" {
		t.Errorf("beta's = %s, %v", got.ID, err)
	}
}

// Pin and unpin are compare-and-set: of two pins from the parked key one
// wins; unpin parks the row with no agent, root or shares; neither moves an
// ephemeral row, a row of another tenant or a row read stale.
func testWorkspacePinAndUnpinAreCompareAndSet(t *testing.T, s store.Store) {
	ctx := context.Background()
	d := mkDurable("ws_d", "acme", "alice", "run-1", "work")
	d.Shares = []store.WorkspaceShare{{RunID: "run-peer", AgentID: "ag_peer", Access: store.WorkspaceShareRead}}
	createWorkspace(t, s, d)
	createWorkspace(t, s, mkWorkspace("ws_e", "acme", "run-1", "tmp", "w1"))

	if ok, err := s.WorkspaceUnpin(ctx, "beta", "ws_d", "run-1"); err != nil || ok {
		t.Errorf("unpin from another tenant: %v, %v", ok, err)
	}
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_d", "run-other"); err != nil || ok {
		t.Errorf("unpin from a run that does not hold it: %v, %v", ok, err)
	}
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_e", "run-1"); err != nil || ok {
		t.Errorf("unpin of an ephemeral row: %v, %v", ok, err)
	}
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_d", "run-1"); err != nil || !ok {
		t.Fatalf("unpin: %v, %v", ok, err)
	}
	p, err := s.WorkspaceGet(ctx, "acme", "ws_d")
	if err != nil {
		t.Fatal(err)
	}
	key := store.WorkspaceParkedKey("ws_d")
	if p.OwnerKind != store.WorkspaceOwnerParked || p.OwnerRunID != key || p.OwnerAgentID != "" || p.RootRunID != "" || len(p.Shares) != 0 {
		t.Errorf("parked row = kind %q run %q agent %q root %q shares %v", p.OwnerKind, p.OwnerRunID, p.OwnerAgentID, p.RootRunID, p.Shares)
	}
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_d", "run-1"); err != nil || ok {
		t.Errorf("a second unpin: %v, %v", ok, err)
	}

	to := store.WorkspaceOwner{RunID: "run-2", AgentID: "ag_2", RootRunID: "root-2"}
	if ok, err := s.WorkspacePin(ctx, "beta", "ws_d", key, to); err != nil || ok {
		t.Errorf("pin in another tenant: %v, %v", ok, err)
	}
	if ok, err := s.WorkspacePin(ctx, "acme", "ws_e", "run-1", to); err != nil || ok {
		t.Errorf("pin of an ephemeral row: %v, %v", ok, err)
	}
	if ok, err := s.WorkspacePin(ctx, "acme", "ws_d", "run-1", to); err != nil || ok {
		t.Errorf("pin from a stale owner: %v, %v", ok, err)
	}
	if ok, err := s.WorkspacePin(ctx, "acme", "ws_d", key, to); err != nil || !ok {
		t.Fatalf("pin: %v, %v", ok, err)
	}
	if ok, err := s.WorkspacePin(ctx, "acme", "ws_d", key, store.WorkspaceOwner{RunID: "run-3", AgentID: "ag_3"}); err != nil || ok {
		t.Errorf("a second pin from the parked key: %v, %v, want the first to have won", ok, err)
	}
	r, _ := s.WorkspaceGet(ctx, "acme", "ws_d")
	if r.OwnerKind != store.WorkspaceOwnerRun || r.OwnerRunID != "run-2" || r.OwnerAgentID != "ag_2" || r.RootRunID != "root-2" {
		t.Errorf("pinned row = kind %q run %q agent %q root %q", r.OwnerKind, r.OwnerRunID, r.OwnerAgentID, r.RootRunID)
	}
	// From a run that ended (the caller read it): moves to the new run.
	if ok, err := s.WorkspacePin(ctx, "acme", "ws_d", "run-2", store.WorkspaceOwner{RunID: "run-4", AgentID: "ag_4"}); err != nil || !ok {
		t.Errorf("pin from the ended holder: %v, %v", ok, err)
	}
	// A run that already holds a workspace of the name cannot take it.
	createWorkspace(t, s, mkWorkspace("ws_other", "acme", "run-5", "work", "w1"))
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_d", "run-4"); err != nil || !ok {
		t.Fatalf("unpin: %v, %v", ok, err)
	}
	if _, err := s.WorkspacePin(ctx, "acme", "ws_d", key, store.WorkspaceOwner{RunID: "run-5"}); !isConflict(err) {
		t.Errorf("pin to a run holding the name: %v, want a conflict", err)
	}
}

// A parked row is reached by no run-keyed read, is never a sweep candidate,
// and holds no capacity; it is still a worker's row.
func testWorkspaceParkedRowIsReachedByNoRun(t *testing.T, s store.Store) {
	ctx := context.Background()
	d := mkDurable("ws_d", "acme", "alice", "run-gone", "work")
	d.Shares = []store.WorkspaceShare{{RunID: "run-peer", AgentID: "ag_peer", Access: store.WorkspaceShareRead}}
	createWorkspace(t, s, d)
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_d", store.WorkspaceOpening, store.WorkspaceReady); err != nil || !ok {
		t.Fatal(ok, err)
	}
	countBoth := func() (int, int) {
		w, err1 := s.WorkspaceCountActiveByWorker(ctx, "w1")
		n, err2 := s.WorkspaceCountActiveByTenant(ctx, "acme")
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		return w, n
	}
	if w, n := countBoth(); w != 1 || n != 1 {
		t.Fatalf("pinned counts = %d, %d", w, n)
	}
	if c, _ := s.WorkspaceSweepCandidates(ctx); !sameIDs(workspaceIDs(c), []string{"ws_d"}) {
		t.Fatalf("the pinned row of a gone run is a candidate: %v", workspaceIDs(c))
	}
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_d", "run-gone"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if w, n := countBoth(); w != 0 || n != 0 {
		t.Errorf("parked counts = %d, %d, want none", w, n)
	}
	if c, _ := s.WorkspaceSweepCandidates(ctx); len(c) != 0 {
		t.Errorf("sweep candidates = %v, want none", workspaceIDs(c))
	}
	if _, err := s.WorkspaceGetByName(ctx, "acme", "run-gone", "work"); !isNotFound(err) {
		t.Errorf("by name for the old run: %v", err)
	}
	if l, _ := s.WorkspaceListByOwnerRun(ctx, "acme", "run-gone"); len(l) != 0 {
		t.Errorf("by owner run: %v", workspaceIDs(l))
	}
	if l, _ := s.WorkspaceListByOwnerAgent(ctx, "acme", "ag_run-gone", "work"); len(l) != 0 {
		t.Errorf("by owner agent: %v", workspaceIDs(l))
	}
	if l, _ := s.WorkspaceListSharedWith(ctx, "acme", "run-peer"); len(l) != 0 {
		t.Errorf("shared with the old peer: %v", workspaceIDs(l))
	}
	if l, _ := s.WorkspaceListByWorker(ctx, "w1"); !sameIDs(workspaceIDs(l), []string{"ws_d"}) {
		t.Errorf("by worker = %v, want the parked row", workspaceIDs(l))
	}
	if n, _ := s.WorkspaceTouchByWorker(ctx, "w1"); n != 1 {
		t.Errorf("touch reached %d rows, want the parked row too (a worker that leaves marks it lost)", n)
	}
}

// The per-user durable count is the tenant's durable rows of that user,
// pinned or parked, and nothing else.
func testWorkspaceCountDurableByUser(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkDurable("ws_a", "acme", "alice", "run-1", "one"))
	createWorkspace(t, s, mkDurable("ws_b", "acme", "alice", "run-2", "two"))
	createWorkspace(t, s, mkDurable("ws_c", "acme", "bob", "run-3", "one"))
	createWorkspace(t, s, mkDurable("ws_d", "beta", "alice", "run-4", "one"))
	createWorkspace(t, s, mkWorkspace("ws_e", "acme", "run-5", "eph", "w1"))
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_b", "run-2"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, c := range []struct {
		tenant, user string
		want         int
	}{{"acme", "alice", 2}, {"acme", "bob", 1}, {"beta", "alice", 1}, {"acme", "carol", 0}} {
		if n, err := s.WorkspaceCountDurableByUser(ctx, c.tenant, c.user); err != nil || n != c.want {
			t.Errorf("durable of %s/%s = %d, %v; want %d", c.tenant, c.user, n, err, c.want)
		}
		if l, err := s.WorkspaceListDurableByUser(ctx, c.tenant, c.user); err != nil || len(l) != c.want {
			t.Errorf("durable list of %s/%s = %d, %v; want %d", c.tenant, c.user, len(l), err, c.want)
		}
	}
}

// A parked durable row keeps its git record (RFC GW P11's column), and a
// pin keeps it too: unpin and pin move the owner, nothing else.
func testWorkspaceParkingKeepsTheGitRecord(t *testing.T, s store.Store) {
	ctx := context.Background()
	d := mkDurable("ws_g", "acme", "alice", "run-1", "repo")
	d.Kind = store.WorkspaceKindGit
	createWorkspace(t, s, d)
	git := store.WorkspaceGit{URL: "https://forge.example/acme/repo.git", Branch: "loom/repo-1", Credential: "$cred:forge", Mirror: "m1"}
	if ok, err := s.WorkspaceSetGit(ctx, "acme", "ws_g", git); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.WorkspaceUnpin(ctx, "acme", "ws_g", "run-1"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	p, _ := s.WorkspaceGet(ctx, "acme", "ws_g")
	if p.Git == nil || *p.Git != git {
		t.Errorf("parked git = %+v, want %+v", p.Git, git)
	}
	if ok, err := s.WorkspacePin(ctx, "acme", "ws_g", store.WorkspaceParkedKey("ws_g"), store.WorkspaceOwner{RunID: "run-2"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if r, _ := s.WorkspaceGet(ctx, "acme", "ws_g"); r.Git == nil || *r.Git != git {
		t.Errorf("re-pinned git = %+v", r.Git)
	}
}
