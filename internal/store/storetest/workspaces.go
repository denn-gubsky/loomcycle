package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RunWorkspaces asserts the RFC GW workspace row contract against a backend,
// each case on a fresh store from factory. It is its own runner, beside Run,
// so each backend's test file calls it once.
func RunWorkspaces(t *testing.T, factory Factory) {
	t.Helper()
	for name, fn := range map[string]func(*testing.T, store.Store){
		"CreateFillsDefaultsAndReadsBack":       testWorkspaceCreateFillsDefaultsAndReadsBack,
		"DuplicateNameInOneRunIsAConflict":      testWorkspaceDuplicateNameInOneRunIsAConflict,
		"SetStateMovesOnlyFromTheExpectedState": testWorkspaceSetStateMovesOnlyFromTheExpectedState,
		"ListByWorkerFiltersByState":            testWorkspaceListByWorkerFiltersByState,
		"CountsOnlyActiveRows":                  testWorkspaceCountsOnlyActiveRows,
		"AnotherTenantSeesNothing":              testWorkspaceAnotherTenantSeesNothing,
		"SweepCandidatesAreEndedOwnersOnly":     testWorkspaceSweepCandidatesAreEndedOwnersOnly,
		"TouchKeepsAWorkersRowsFromGoingStale":  testWorkspaceTouchKeepsAWorkersRowsFromGoingStale,
		"SetSharesIsACompareAndSet":             testWorkspaceSetSharesIsACompareAndSet,
		"ListByOwnerAgentAndSharedWith":         testWorkspaceListByOwnerAgentAndSharedWith,
		"PrincipalOwnedRowsAreNeverSwept":       testWorkspacePrincipalOwnedRowsAreNeverSwept,
	} {
		t.Run(name, func(t *testing.T) {
			s, cleanup := factory(t)
			defer cleanup()
			fn(t, s)
		})
	}
}

func mkWorkspace(id, tenant, ownerRun, name, worker string) store.WorkspaceRow {
	return store.WorkspaceRow{ID: id, TenantID: tenant, UserID: "alice", Name: name, WorkerID: worker,
		Engine: "host", OwnerRunID: ownerRun, OwnerAgentID: "ag_" + ownerRun, RootRunID: ownerRun}
}

func createWorkspace(t *testing.T, s store.Store, row store.WorkspaceRow) store.WorkspaceRow {
	t.Helper()
	got, err := s.WorkspaceCreate(context.Background(), row)
	if err != nil {
		t.Fatalf("create workspace %s: %v", row.ID, err)
	}
	return got
}

func workspaceIDs(rows []store.WorkspaceRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func testWorkspaceCreateFillsDefaultsAndReadsBack(t *testing.T, s store.Store) {
	ctx := context.Background()
	got := createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "work", "w1"))
	if got.State != store.WorkspaceOpening || got.Kind != store.WorkspaceKindGeneral || got.Lifetime != store.WorkspaceLifetimeEphemeral {
		t.Errorf("defaults = state %q kind %q lifetime %q, want opening/general/ephemeral", got.State, got.Kind, got.Lifetime)
	}
	if got.CreatedAt.IsZero() || !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Errorf("timestamps = created %v updated %v, want both set and equal", got.CreatedAt, got.UpdatedAt)
	}
	want := mkWorkspace("ws_1", "acme", "run-1", "work", "w1")
	want.State, want.Kind, want.Lifetime, want.CreatedAt, want.UpdatedAt = got.State, got.Kind, got.Lifetime, got.CreatedAt, got.UpdatedAt
	want.Shares = []store.WorkspaceShare{} // read back as empty, never nil
	// A row written with no owner kind is a run's; the open stamps its last
	// use (RFC GW D35).
	want.OwnerKind, want.LastUsedAt = store.WorkspaceOwnerRun, got.CreatedAt
	if !got.LastUsedAt.Equal(got.CreatedAt) {
		t.Errorf("last used = %v, want the create time %v", got.LastUsedAt, got.CreatedAt)
	}
	want.LastUsedAt = got.LastUsedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("create returned %+v, want %+v", got, want)
	}
	byID, err := s.WorkspaceGet(ctx, "acme", "ws_1")
	if err != nil || !reflect.DeepEqual(byID, got) {
		t.Errorf("get = %+v, %v; want %+v", byID, err, got)
	}
	byName, err := s.WorkspaceGetByName(ctx, "acme", "run-1", "work")
	if err != nil || !reflect.DeepEqual(byName, got) {
		t.Errorf("get by name = %+v, %v; want %+v", byName, err, got)
	}
	createWorkspace(t, s, mkWorkspace("ws_2", "acme", "run-1", "alpha", "w1"))
	list, err := s.WorkspaceListByOwnerRun(ctx, "acme", "run-1")
	if err != nil || !sameIDs(workspaceIDs(list), []string{"ws_2", "ws_1"}) {
		t.Errorf("list by owner run = %v, %v; want [ws_2 ws_1] (by name)", workspaceIDs(list), err)
	}
	if list, err := s.WorkspaceListByOwnerRun(ctx, "acme", "run-none"); err != nil || list == nil || len(list) != 0 {
		t.Errorf("list of a run with none = %v, %v; want empty, not nil", list, err)
	}
	for name, bad := range map[string]store.WorkspaceRow{
		"no id":        mkWorkspace("", "acme", "run-1", "x", "w1"),
		"no owner run": mkWorkspace("ws_x", "acme", "", "x", "w1"),
		"no name":      mkWorkspace("ws_x", "acme", "run-1", "", "w1"),
		"no worker":    mkWorkspace("ws_x", "acme", "run-1", "x", ""),
		"bad state": func() store.WorkspaceRow {
			r := mkWorkspace("ws_x", "acme", "run-1", "x", "w1")
			r.State = "gone"
			return r
		}(),
	} {
		if _, err := s.WorkspaceCreate(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if ok, err := s.WorkspaceDelete(ctx, "acme", "ws_1"); err != nil || !ok {
		t.Fatalf("delete = %v, %v; want true", ok, err)
	}
	var nf *store.ErrNotFound
	if _, err := s.WorkspaceGet(ctx, "acme", "ws_1"); !errors.As(err, &nf) {
		t.Errorf("get after delete: err = %v, want *ErrNotFound", err)
	}
	if ok, err := s.WorkspaceDelete(ctx, "acme", "ws_1"); err != nil || ok {
		t.Errorf("second delete = %v, %v; want false", ok, err)
	}
}

func testWorkspaceDuplicateNameInOneRunIsAConflict(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "work", "w1"))
	var conflict *store.ErrConflict
	if _, err := s.WorkspaceCreate(ctx, mkWorkspace("ws_2", "acme", "run-1", "work", "w2")); !errors.As(err, &conflict) {
		t.Errorf("same name in one run: err = %v, want *ErrConflict", err)
	}
	if _, err := s.WorkspaceCreate(ctx, mkWorkspace("ws_1", "acme", "run-2", "other", "w1")); !errors.As(err, &conflict) {
		t.Errorf("same id: err = %v, want *ErrConflict", err)
	}
	// The name is the run's own: another run, of any tenant, may use it.
	createWorkspace(t, s, mkWorkspace("ws_3", "acme", "run-2", "work", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_4", "beta", "run-3", "work", "w1"))
	if got, err := s.WorkspaceGet(ctx, "acme", "ws_1"); err != nil || got.WorkerID != "w1" {
		t.Errorf("the refused create changed the first row: %+v, %v", got, err)
	}
}

func testWorkspaceSetStateMovesOnlyFromTheExpectedState(t *testing.T, s store.Store) {
	ctx := context.Background()
	created := createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "work", "w1"))

	// A replica that read "ready" while the row is still opening moves nothing.
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_1", store.WorkspaceReady, store.WorkspaceReleasing); err != nil || ok {
		t.Fatalf("move from the wrong state = %v, %v; want false", ok, err)
	}
	if got, _ := s.WorkspaceGet(ctx, "acme", "ws_1"); got.State != store.WorkspaceOpening {
		t.Fatalf("state after the refused move = %q, want opening", got.State)
	}
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_1", store.WorkspaceOpening, store.WorkspaceReady); err != nil || !ok {
		t.Fatalf("opening -> ready = %v, %v; want true", ok, err)
	}
	// Two replicas releasing one row: the first wins, the second finds it moved.
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_1", store.WorkspaceReady, store.WorkspaceReleasing); err != nil || !ok {
		t.Fatalf("first release = %v, %v; want true", ok, err)
	}
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_1", store.WorkspaceReady, store.WorkspaceReleasing); err != nil || ok {
		t.Fatalf("second release = %v, %v; want false", ok, err)
	}
	got, err := s.WorkspaceGet(ctx, "acme", "ws_1")
	if err != nil || got.State != store.WorkspaceReleasing || got.UpdatedAt.Before(created.UpdatedAt) || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("after the moves = %+v, %v; want releasing, created_at kept, updated_at not earlier", got, err)
	}
	if _, err := s.WorkspaceSetState(ctx, "acme", "ws_1", store.WorkspaceReleasing, "gone"); err == nil {
		t.Errorf("a move to an unknown state was accepted")
	}
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_missing", store.WorkspaceOpening, store.WorkspaceReady); err != nil || ok {
		t.Errorf("move of a missing row = %v, %v; want false", ok, err)
	}
}

func testWorkspaceListByWorkerFiltersByState(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "a", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_2", "beta", "run-2", "b", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_3", "acme", "run-3", "c", "w2"))
	if ok, err := s.WorkspaceSetState(ctx, "beta", "ws_2", store.WorkspaceOpening, store.WorkspaceReleasing); err != nil || !ok {
		t.Fatalf("release ws_2: %v, %v", ok, err)
	}
	all, err := s.WorkspaceListByWorker(ctx, "w1")
	if err != nil || len(all) != 2 {
		t.Fatalf("w1 every state = %v, %v; want ws_1 and ws_2 across tenants", workspaceIDs(all), err)
	}
	releasing, err := s.WorkspaceListByWorker(ctx, "w1", store.WorkspaceReleasing)
	if err != nil || !sameIDs(workspaceIDs(releasing), []string{"ws_2"}) {
		t.Errorf("w1 releasing = %v, %v; want [ws_2]", workspaceIDs(releasing), err)
	}
	active, err := s.WorkspaceListByWorker(ctx, "w1", store.WorkspaceActiveStates...)
	if err != nil || !sameIDs(workspaceIDs(active), []string{"ws_1"}) {
		t.Errorf("w1 active = %v, %v; want [ws_1]", workspaceIDs(active), err)
	}
	if none, err := s.WorkspaceListByWorker(ctx, "w9"); err != nil || len(none) != 0 {
		t.Errorf("unknown worker = %v, %v; want empty", workspaceIDs(none), err)
	}
}

func testWorkspaceCountsOnlyActiveRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "a", "w1")) // opening
	createWorkspace(t, s, mkWorkspace("ws_2", "acme", "run-1", "b", "w1")) // -> ready
	createWorkspace(t, s, mkWorkspace("ws_3", "acme", "run-1", "c", "w1")) // -> releasing
	createWorkspace(t, s, mkWorkspace("ws_4", "acme", "run-1", "d", "w2")) // -> lost
	createWorkspace(t, s, mkWorkspace("ws_5", "beta", "run-2", "a", "w1")) // another tenant, ready
	for _, m := range []struct {
		tenant, id string
		to         store.WorkspaceState
	}{
		{"acme", "ws_2", store.WorkspaceReady}, {"acme", "ws_3", store.WorkspaceReleasing},
		{"acme", "ws_4", store.WorkspaceLost}, {"beta", "ws_5", store.WorkspaceReady},
	} {
		if ok, err := s.WorkspaceSetState(ctx, m.tenant, m.id, store.WorkspaceOpening, m.to); err != nil || !ok {
			t.Fatalf("move %s: %v, %v", m.id, ok, err)
		}
	}
	for worker, want := range map[string]int{"w1": 3, "w2": 0, "w9": 0} {
		if n, err := s.WorkspaceCountActiveByWorker(ctx, worker); err != nil || n != want {
			t.Errorf("active on %s = %d, %v; want %d", worker, n, err, want)
		}
	}
	for tenant, want := range map[string]int{"acme": 2, "beta": 1, "gamma": 0} {
		if n, err := s.WorkspaceCountActiveByTenant(ctx, tenant); err != nil || n != want {
			t.Errorf("active in %s = %d, %v; want %d", tenant, n, err, want)
		}
	}
}

func testWorkspaceAnotherTenantSeesNothing(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "work", "w1"))
	var nf *store.ErrNotFound
	for _, tenant := range []string{"beta", ""} {
		if _, err := s.WorkspaceGet(ctx, tenant, "ws_1"); !errors.As(err, &nf) {
			t.Errorf("tenant %q get: err = %v, want *ErrNotFound", tenant, err)
		}
		if _, err := s.WorkspaceGetByName(ctx, tenant, "run-1", "work"); !errors.As(err, &nf) {
			t.Errorf("tenant %q get by name: err = %v, want *ErrNotFound", tenant, err)
		}
		if list, err := s.WorkspaceListByOwnerRun(ctx, tenant, "run-1"); err != nil || len(list) != 0 {
			t.Errorf("tenant %q list = %v, %v; want empty", tenant, workspaceIDs(list), err)
		}
		if ok, err := s.WorkspaceSetState(ctx, tenant, "ws_1", store.WorkspaceOpening, store.WorkspaceReleasing); err != nil || ok {
			t.Errorf("tenant %q set state = %v, %v; want false", tenant, ok, err)
		}
		if ok, err := s.WorkspaceDelete(ctx, tenant, "ws_1"); err != nil || ok {
			t.Errorf("tenant %q delete = %v, %v; want false", tenant, ok, err)
		}
	}
	if got, err := s.WorkspaceGet(ctx, "acme", "ws_1"); err != nil || got.State != store.WorkspaceOpening {
		t.Errorf("owner's row after the other tenants' calls = %+v, %v; want it unchanged", got, err)
	}
}

// testWorkspaceSweepCandidatesAreEndedOwnersOnly: the sweeper's list holds a
// workspace whose OWNER run ended, and no other. A running owner, and an
// ended one that is paused or pausing (parked, to be resumed), keep theirs.
// The pin is the owner run, not the tree: a sub-run's workspace is swept when
// the sub-run ends though its root still runs, and a running sub-run keeps
// its own though its root has ended.
func testWorkspaceSweepCandidatesAreEndedOwnersOnly(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	mkRun := func(agent, parent string) string {
		t.Helper()
		run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agent, TenantID: "acme", ParentRunID: parent})
		if err != nil {
			t.Fatalf("create run %s: %v", agent, err)
		}
		return run.ID
	}
	finish := func(id string) {
		t.Helper()
		if err := s.FinishRun(ctx, id, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
			t.Fatalf("finish %s: %v", id, err)
		}
	}
	pause := func(id, state string) {
		t.Helper()
		if err := s.SetRunPauseState(ctx, id, state); err != nil {
			t.Fatalf("pause %s: %v", id, err)
		}
	}

	done, running, paused, pausing := mkRun("a_done", ""), mkRun("a_running", ""), mkRun("a_paused", ""), mkRun("a_pausing", "")
	liveRoot := mkRun("a_live_root", "")
	endedSub := mkRun("a_ended_sub", liveRoot)
	endedRoot := mkRun("a_ended_root", "")
	liveSub := mkRun("a_live_sub", endedRoot)
	for _, id := range []string{done, paused, pausing, endedSub, endedRoot} {
		finish(id)
	}
	// Terminal status AND a pause state: the pause filter, not the status
	// filter, is what must keep these out.
	pause(paused, store.PauseStatePaused)
	pause(pausing, store.PauseStatePausing)
	for i, owner := range []string{done, running, paused, pausing, endedSub, liveSub} {
		createWorkspace(t, s, mkWorkspace("ws_"+string(rune('a'+i)), "acme", owner, "work", "w1"))
	}
	// An owner whose run row is gone (its tree was deleted) can never run
	// again: its workspace is swept like an ended one's.
	const gone = "run-deleted"
	createWorkspace(t, s, mkWorkspace("ws_orphan", "acme", gone, "work", "w1"))

	cands, err := s.WorkspaceSweepCandidates(ctx)
	if err != nil {
		t.Fatalf("sweep candidates: %v", err)
	}
	got := map[string]store.WorkspaceRow{}
	for _, c := range cands {
		got[c.OwnerRunID] = c
	}
	for owner, want := range map[string]bool{
		done: true, endedSub: true, gone: true,
		running: false, paused: false, pausing: false, liveSub: false,
	} {
		if _, in := got[owner]; in != want {
			t.Errorf("owner %s in the sweep list = %v, want %v", owner, in, want)
		}
	}
	if c := got[done]; c.TenantID != "acme" || c.WorkerID != "w1" {
		t.Errorf("candidate row = %+v; want the whole row (tenant, worker) for the release", c)
	}
}

// testWorkspaceTouchKeepsAWorkersRowsFromGoingStale: the stale list is the
// active rows not touched since a time; a touch moves a worker's active rows
// out of it and leaves its released and lost rows, and other workers' rows,
// as they were.
func testWorkspaceTouchKeepsAWorkersRowsFromGoingStale(t *testing.T, s store.Store) {
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "a", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_2", "acme", "run-1", "b", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_3", "other", "run-2", "a", "w2"))
	createWorkspace(t, s, mkWorkspace("ws_4", "acme", "run-1", "c", "w1"))
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_2", store.WorkspaceOpening, store.WorkspaceReady); err != nil || !ok {
		t.Fatalf("ready: %v %v", ok, err)
	}
	if ok, err := s.WorkspaceSetState(ctx, "acme", "ws_4", store.WorkspaceOpening, store.WorkspaceReleasing); err != nil || !ok {
		t.Fatalf("releasing: %v %v", ok, err)
	}
	if got, err := s.WorkspaceListStale(ctx, before); err != nil || len(got) != 0 {
		t.Fatalf("stale before any row was written = %v, %v; want none", workspaceIDs(got), err)
	}
	time.Sleep(20 * time.Millisecond)
	mid := time.Now()
	time.Sleep(20 * time.Millisecond)
	got, err := s.WorkspaceListStale(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first by updated_at, which ws_2's move to ready bumped.
	if ids := workspaceIDs(got); !sameIDs(ids, []string{"ws_1", "ws_3", "ws_2"}) {
		t.Fatalf("stale = %v, want the three active rows", ids)
	}
	n, err := s.WorkspaceTouchByWorker(ctx, "w1")
	if err != nil || n != 2 {
		t.Fatalf("touch w1 = %d, %v; want its 2 active rows", n, err)
	}
	got, err = s.WorkspaceListStale(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	if ids := workspaceIDs(got); !sameIDs(ids, []string{"ws_3"}) {
		t.Errorf("stale after touching w1 = %v, want only w2's row", ids)
	}
	if r, err := s.WorkspaceGet(ctx, "acme", "ws_4"); err != nil || r.State != store.WorkspaceReleasing || !r.UpdatedAt.Before(mid) {
		t.Errorf("a releasing row = %+v, %v; want it untouched", r, err)
	}
}

// RFC GW P8: shares round-trip, and a write naming shares that moved since
// it read them changes nothing.
func testWorkspaceSetSharesIsACompareAndSet(t *testing.T, s store.Store) {
	ctx := context.Background()
	row := createWorkspace(t, s, mkWorkspace("ws_1", "acme", "run-1", "work", "w1"))
	at := time.Date(2026, 11, 22, 1, 2, 3, 456789000, time.FixedZone("x", 3600))
	one := []store.WorkspaceShare{{RunID: "run-2", AgentID: "ag_2", Access: store.WorkspaceShareRead, CreatedAt: at}}
	if ok, err := s.WorkspaceSetShares(ctx, "acme", row.ID, row.Shares, one); err != nil || !ok {
		t.Fatalf("set shares = %v, %v; want true", ok, err)
	}
	got, err := s.WorkspaceGet(ctx, "acme", row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shares) != 1 || got.Shares[0].RunID != "run-2" || got.Shares[0].AgentID != "ag_2" ||
		got.Shares[0].Access != store.WorkspaceShareRead || !got.Shares[0].CreatedAt.Equal(at) {
		t.Fatalf("shares read back = %+v", got.Shares)
	}
	// A writer that read the row before the share is refused.
	two := []store.WorkspaceShare{{RunID: "run-3", AgentID: "ag_3", Access: store.WorkspaceShareReadWrite, CreatedAt: at}}
	if ok, err := s.WorkspaceSetShares(ctx, "acme", row.ID, row.Shares, two); err != nil || ok {
		t.Errorf("stale set = %v, %v; want false", ok, err)
	}
	// One that read it back succeeds, from exactly what it read.
	if ok, err := s.WorkspaceSetShares(ctx, "acme", row.ID, got.Shares, nil); err != nil || !ok {
		t.Errorf("set from read-back shares = %v, %v; want true", ok, err)
	}
	if again, _ := s.WorkspaceGet(ctx, "acme", row.ID); again.Shares == nil || len(again.Shares) != 0 {
		t.Errorf("cleared shares = %#v, want empty", again.Shares)
	}
	// Another tenant cannot write them.
	if ok, err := s.WorkspaceSetShares(ctx, "other", row.ID, nil, one); err != nil || ok {
		t.Errorf("other tenant's set = %v, %v; want false", ok, err)
	}
}

// RFC GW P8: a peer finds a shared workspace by its owner's agent id and
// name, and lists those shared with it, in its own tenant only.
func testWorkspaceListByOwnerAgentAndSharedWith(t *testing.T, s store.Store) {
	ctx := context.Background()
	a := createWorkspace(t, s, mkWorkspace("ws_a", "acme", "run-1", "work", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_b", "acme", "run-1", "other", "w1"))
	createWorkspace(t, s, mkWorkspace("ws_c", "beta", "run-9", "work", "w1"))
	rows, err := s.WorkspaceListByOwnerAgent(ctx, "acme", "ag_run-1", "work")
	if err != nil || !sameIDs(workspaceIDs(rows), []string{"ws_a"}) {
		t.Errorf("by owner agent = %v, %v; want [ws_a]", workspaceIDs(rows), err)
	}
	if rows, err := s.WorkspaceListByOwnerAgent(ctx, "beta", "ag_run-1", "work"); err != nil || len(rows) != 0 {
		t.Errorf("by owner agent in another tenant = %v, %v; want none", workspaceIDs(rows), err)
	}
	if rows, err := s.WorkspaceListByOwnerAgent(ctx, "acme", "", "work"); err != nil || len(rows) != 0 {
		t.Errorf("by an empty owner agent = %v, %v; want none", workspaceIDs(rows), err)
	}
	// A run id holding LIKE's wildcards matches only itself.
	sh := []store.WorkspaceShare{{RunID: "r_1%", AgentID: "ag", Access: store.WorkspaceShareRead, CreatedAt: time.Now()}}
	if ok, err := s.WorkspaceSetShares(ctx, "acme", a.ID, a.Shares, sh); err != nil || !ok {
		t.Fatalf("set shares = %v, %v", ok, err)
	}
	if rows, err := s.WorkspaceListSharedWith(ctx, "acme", "r_1%"); err != nil || !sameIDs(workspaceIDs(rows), []string{"ws_a"}) {
		t.Errorf("shared with r_1%% = %v, %v; want [ws_a]", workspaceIDs(rows), err)
	}
	for _, other := range []string{"r_1", "r_10", "r_1x", "r__1%", ""} {
		if rows, err := s.WorkspaceListSharedWith(ctx, "acme", other); err != nil || len(rows) != 0 {
			t.Errorf("shared with %q = %v, %v; want none", other, workspaceIDs(rows), err)
		}
	}
	if rows, err := s.WorkspaceListSharedWith(ctx, "beta", "r_1%"); err != nil || len(rows) != 0 {
		t.Errorf("shared with, another tenant = %v, %v; want none", workspaceIDs(rows), err)
	}
}

// testWorkspacePrincipalOwnedRowsAreNeverSwept: a workspace opened over the
// MCP server (RFC GW D35) has no run, so its pin key matches no run row. The
// run sweeper, which takes a row whose run is gone as ended, must never list
// it; the principal pass lists it with its owner kind, opening token and
// last use, and a touch moves only that row's last use.
func testWorkspacePrincipalOwnedRowsAreNeverSwept(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := mkWorkspace("ws_p", "acme", "principal:abc", "mine", "w1")
	p.OwnerKind, p.OpenedToken, p.OwnerAgentID = store.WorkspaceOwnerPrincipal, "otd_1", ""
	got := createWorkspace(t, s, p)
	if got.OwnerKind != store.WorkspaceOwnerPrincipal || got.OpenedToken != "otd_1" || got.LastUsedAt.IsZero() {
		t.Fatalf("read back %+v, want the principal kind, its token and a last use", got)
	}
	// A run's row whose run is gone: swept, as before.
	r := createWorkspace(t, s, mkWorkspace("ws_r", "acme", "run_gone", "theirs", "w1"))
	if r.OwnerKind != store.WorkspaceOwnerRun {
		t.Errorf("a row written with no kind reads back %q, want run", r.OwnerKind)
	}
	cands, err := s.WorkspaceSweepCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ids := workspaceIDs(cands); !sameIDs(ids, []string{"ws_r"}) {
		t.Errorf("sweep candidates = %v, want only the run's row", ids)
	}
	own, err := s.WorkspaceListPrincipalOwned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ids := workspaceIDs(own); !sameIDs(ids, []string{"ws_p"}) {
		t.Errorf("principal owned = %v, want only ws_p", ids)
	}
	at := got.LastUsedAt.Add(time.Hour)
	if err := s.WorkspaceTouchUsed(ctx, "acme", "ws_p", at); err != nil {
		t.Fatal(err)
	}
	// Another tenant's touch reaches nothing.
	if err := s.WorkspaceTouchUsed(ctx, "other", "ws_p", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	back, err := s.WorkspaceGet(ctx, "acme", "ws_p")
	if err != nil {
		t.Fatal(err)
	}
	if !back.LastUsedAt.Equal(at.UTC().Truncate(time.Microsecond)) && !back.LastUsedAt.Equal(at.UTC()) {
		t.Errorf("last used = %v, want %v", back.LastUsedAt, at)
	}
	bad := mkWorkspace("ws_x", "acme", "run_x", "x", "w1")
	bad.OwnerKind = "agent"
	if _, err := s.WorkspaceCreate(ctx, bad); err == nil {
		t.Error("an unknown owner kind was written")
	}
}
