package storetest

import (
	"context"
	"errors"
	"testing"

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
	if got != want {
		t.Errorf("create returned %+v, want %+v", got, want)
	}
	byID, err := s.WorkspaceGet(ctx, "acme", "ws_1")
	if err != nil || byID != got {
		t.Errorf("get = %+v, %v; want %+v", byID, err, got)
	}
	byName, err := s.WorkspaceGetByName(ctx, "acme", "run-1", "work")
	if err != nil || byName != got {
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

	cands, err := s.WorkspaceSweepCandidates(ctx)
	if err != nil {
		t.Fatalf("sweep candidates: %v", err)
	}
	got := map[string]store.WorkspaceRow{}
	for _, c := range cands {
		got[c.OwnerRunID] = c
	}
	for owner, want := range map[string]bool{
		done: true, endedSub: true,
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
