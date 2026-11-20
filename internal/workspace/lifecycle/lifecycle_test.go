package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

type fixture struct {
	st   store.Store
	h    *workspacetest.Harness
	m    *Manager
	sess string
}

func newFixture(t *testing.T, l Limits, opt worker.Options) *fixture {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := workspacetest.Start(t, st, "acme", opt)
	sess, err := st.CreateSession(context.Background(), "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, h: h, m: New(h.D, st, l, nil), sess: sess.ID}
}

// run makes a run row and returns the Owner it is.
func (f *fixture) run(t *testing.T, user string) Owner {
	t.Helper()
	r, err := f.st.CreateRun(context.Background(), f.sess, store.RunIdentity{AgentID: "a_" + user, UserID: user, TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	return Owner{TenantID: "acme", UserID: user, RunID: r.ID, AgentID: "a_" + user, RootRunID: r.ID}
}

func (f *fixture) open(t *testing.T, o Owner, name string) store.WorkspaceRow {
	t.Helper()
	r, err := f.m.Open(context.Background(), o, OpenRequest{Name: name, Toolchains: []string{"python", "node"}})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	return r
}

func (f *fixture) finish(t *testing.T, runID string, st store.RunStatus) {
	t.Helper()
	if err := f.st.FinishRun(context.Background(), runID, st, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
}

func onDisk(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestOpen_MakesTheDirectoryAndAReadyRowOwnedByTheRun(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "work")
	if r.State != store.WorkspaceReady || r.OwnerRunID != o.RunID || r.WorkerID != f.h.WorkerID || r.Engine != workspace.EngineHost {
		t.Errorf("row = %+v", r)
	}
	if !onDisk(f.h.Dir(r.ID)) {
		t.Error("no directory on the worker")
	}
	again := f.open(t, o, "work")
	if again.ID != r.ID {
		t.Errorf("a repeated open made another workspace: %s then %s", r.ID, again.ID)
	}
	if _, err := f.m.Open(context.Background(), o, OpenRequest{Name: "../x"}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("a bad name: %v", err)
	}
}

// The pin: only the run that opened a workspace finds it. A sibling run of
// the same tree, a run of another user in the tenant, and a run of another
// tenant all get not found, the answer for a name nobody uses.
func TestGet_OnlyTheOwnerRunFindsItsWorkspace(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	ctx := context.Background()
	owner := f.run(t, "alice")
	r := f.open(t, owner, "work")
	sibling := f.run(t, "alice")
	sibling.RootRunID = owner.RootRunID
	other := f.run(t, "bob")
	foreign := owner
	foreign.TenantID = "other"
	for name, o := range map[string]Owner{"sibling": sibling, "another user": other, "another tenant": foreign} {
		if _, err := f.m.Get(ctx, o, "work"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Get = %v, want not found", name, err)
		}
		if rows, err := f.m.List(ctx, o); err != nil || len(rows) != 0 {
			t.Errorf("%s: List = %v, %v; want none", name, rows, err)
		}
	}
	if got, err := f.m.Get(ctx, owner, "work"); err != nil || got.ID != r.ID {
		t.Errorf("owner: Get = %+v, %v", got, err)
	}
}

func TestOpen_RefusesPastMaxPerRunAndMaxPerTenant(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 2, MaxPerTenant: 3}, worker.Options{Capacity: 10})
	ctx := context.Background()
	a := f.run(t, "alice")
	f.open(t, a, "one")
	f.open(t, a, "two")
	if _, err := f.m.Open(ctx, a, OpenRequest{Name: "three"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("a third workspace in a run: %v, want ErrLimit", err)
	}
	b := f.run(t, "bob")
	f.open(t, b, "one")
	if _, err := f.m.Open(ctx, b, OpenRequest{Name: "two"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("a fourth workspace in the tenant: %v, want ErrLimit", err)
	}
	if n, _ := f.st.WorkspaceCountActiveByTenant(ctx, "acme"); n != 3 {
		t.Errorf("the refused opens left rows: %d active", n)
	}
	ents, _ := os.ReadDir(f.h.Root)
	if len(ents) != 3 {
		t.Errorf("the refused opens left directories: %d", len(ents))
	}
}

func TestDo_RunsAnOpInTheWorkspace(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "work")
	res, err := f.m.Do(context.Background(), r, dispatch.Request{Op: workspace.OpExec, Args: []byte(`{"command":"echo hi > f; cat f","timeout_ms":5000}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"exit_code":0,"output":"hi\n"}` {
		t.Errorf("output = %s", res.Output)
	}
	if !onDisk(filepath.Join(f.h.Dir(r.ID), "f")) {
		t.Error("the command did not run in the workspace")
	}
}

// When its owner ends, however it ends, the workspace is gone from the
// worker's disk and its row from the store; a paused or pausing owner keeps
// its own.
func TestReleaseRun_RemovesAnEndedRunsWorkspacesAndKeepsAPausedOnes(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	ctx := context.Background()
	for _, st := range []store.RunStatus{store.RunCompleted, store.RunFailed, store.RunCancelled} {
		o := f.run(t, "alice")
		r := f.open(t, o, "work")
		f.finish(t, o.RunID, st)
		if err := f.m.ReleaseRun(ctx, o.RunID); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		if onDisk(f.h.Dir(r.ID)) {
			t.Errorf("%s: the directory is still on the worker", st)
		}
		if _, err := f.st.WorkspaceGet(ctx, "acme", r.ID); err == nil {
			t.Errorf("%s: the row is still there", st)
		}
	}
	for _, ps := range []string{store.PauseStatePaused, store.PauseStatePausing} {
		o := f.run(t, "alice")
		r := f.open(t, o, "work")
		if err := f.st.SetRunPauseState(ctx, o.RunID, ps); err != nil {
			t.Fatal(err)
		}
		if err := f.m.ReleaseRun(ctx, o.RunID); err != nil {
			t.Fatal(err)
		}
		if !onDisk(f.h.Dir(r.ID)) {
			t.Errorf("%s: a parked run's workspace was removed", ps)
		}
	}
}

// A worker away at the release leaves the row releasing; its next attach
// removes the directory and the row.
func TestRelease_AWorkerAwayFinishesAtItsNextAttach(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	ctx := context.Background()
	o := f.run(t, "alice")
	r := f.open(t, o, "work")
	f.h.StopWorker()
	f.finish(t, o.RunID, store.RunCompleted)
	if err := f.m.ReleaseRun(ctx, o.RunID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.st.WorkspaceGet(ctx, "acme", r.ID); err != nil || got.State != store.WorkspaceReleasing {
		t.Fatalf("row = %+v, %v; want releasing", got, err)
	}
	f.h.StartWorker()
	waitGone(t, f, r.ID)
}

func waitGone(t *testing.T, f *fixture, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := f.st.WorkspaceGet(context.Background(), "acme", id)
		if err != nil && !onDisk(f.h.Dir(id)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace %s: row err %v, on disk %v", id, err, onDisk(f.h.Dir(id)))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// What a core killed mid-run left (an ended owner, no release) and a
// workspace whose owner run row is gone are swept; a paused run's is not.
func TestSweep_ReleasesWhatEndedRunsLeftAndKeepsAPausedRuns(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	ctx := context.Background()
	ended := f.run(t, "alice")
	endedWS := f.open(t, ended, "work")
	f.finish(t, ended.RunID, store.RunFailed)
	paused := f.run(t, "alice")
	pausedWS := f.open(t, paused, "work")
	f.finish(t, paused.RunID, store.RunCompleted)
	if err := f.st.SetRunPauseState(ctx, paused.RunID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	live := f.run(t, "alice")
	liveWS := f.open(t, live, "work")
	orphan := Owner{TenantID: "acme", UserID: "alice", RunID: "run-deleted", RootRunID: "run-deleted"}
	orphanWS := f.open(t, orphan, "work")

	n, err := f.m.Sweep(ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want the ended run's and the orphan's", n, err)
	}
	waitGone(t, f, endedWS.ID)
	waitGone(t, f, orphanWS.ID)
	for _, r := range []store.WorkspaceRow{pausedWS, liveWS} {
		if !onDisk(f.h.Dir(r.ID)) {
			t.Errorf("workspace of run %s was swept", r.OwnerRunID)
		}
	}
}

// A worker no replica has touched for lost_after is marked lost; the
// workspaces of one attached here are not.
func TestMarkLost_MarksOnlyAWorkerNobodySees(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, LostAfter: time.Minute}, worker.Options{})
	ctx := context.Background()
	o := f.run(t, "alice")
	r := f.open(t, o, "work")
	f.m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, err := f.m.MarkLost(ctx); err != nil || n != 0 {
		t.Fatalf("with the worker attached: %d, %v; want none", n, err)
	}
	f.h.StopWorker()
	if n, err := f.m.MarkLost(ctx); err != nil || n != 1 {
		t.Fatalf("with the worker gone: %d, %v; want 1", n, err)
	}
	if got, _ := f.st.WorkspaceGet(ctx, "acme", r.ID); got.State != store.WorkspaceLost {
		t.Errorf("state = %s, want lost", got.State)
	}
	f.m.now = time.Now
	o2 := f.run(t, "bob")
	f.h.StartWorker()
	r2 := f.open(t, o2, "fresh")
	f.h.StopWorker()
	if n, _ := f.m.MarkLost(ctx); n != 0 {
		t.Errorf("a workspace touched within lost_after was marked lost (%s)", r2.ID)
	}
}
