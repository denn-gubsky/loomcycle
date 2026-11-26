// Package clustertest is the several-replica suite of RFC GW P12 (plan
// §3.5): two core replicas over one store, one worker attached to those a
// case names. Each store's tests run it (SQLite in the ordinary suite,
// Postgres in the Postgres job), as storetest runs the store contract.
package clustertest

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

// Factory opens a fresh store for one case; the store closes with the test.
type Factory func(t *testing.T) store.Store

// Run runs every case against stores the factory opens.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	for name, fn := range map[string]func(*testing.T, store.Store){
		"ReleaseOnAReplicaTheWorkerIsNotOnIsFinishedByAnother": testReleaseFinishedByAnother,
		"OpenOnAOpsOnB":                        testOpenOnAOpsOnB,
		"DurablePinRaceHasOneWinner":           testDurablePinRace,
		"WorkerRestartIsReconciledOnBoth":      testWorkerRestart,
		"AnOpOnAReplicaTheWorkerIsNotOnIsLost": testOpOnAReplicaWithoutTheWorker,
	} {
		t.Run(name, func(t *testing.T) { fn(t, factory(t)) })
	}
}

type replicas struct {
	c    *workspacetest.Cluster
	m    []*lifecycle.Manager
	st   store.Store
	sess string
}

func start(t *testing.T, st store.Store, attach []int) *replicas {
	t.Helper()
	c := workspacetest.StartCluster(t, st, "acme", workspacetest.ClusterOptions{Replicas: 2, Attach: attach, RecheckEvery: 100 * time.Millisecond})
	sess, err := st.CreateSession(context.Background(), "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	r := &replicas{c: c, st: st, sess: sess.ID}
	for _, d := range c.D {
		m := lifecycle.New(d, st, lifecycle.Limits{MaxPerRun: 4, LostAfter: time.Minute}, nil)
		m.SetDurable(lifecycle.DurableLimits{Enabled: true, MaxPerUser: 8})
		r.m = append(r.m, m)
	}
	return r
}

func (r *replicas) run(t *testing.T, agent string) lifecycle.Owner {
	t.Helper()
	run, err := r.st.CreateRun(context.Background(), r.sess, store.RunIdentity{AgentID: agent, UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle.Owner{TenantID: "acme", UserID: "alice", RunID: run.ID, AgentID: agent, RootRunID: run.ID}
}

func open(t *testing.T, m *lifecycle.Manager, o lifecycle.Owner, name, lifetime string) store.WorkspaceRow {
	t.Helper()
	row, err := m.Open(context.Background(), o, lifecycle.OpenRequest{Name: name, Toolchains: []string{"python", "node"}, Lifetime: lifetime})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	return row
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// G-A: a release on a replica the worker is not attached to leaves the row
// releasing; the replica the worker is on finishes it on its next recheck,
// not only at the worker's next attach.
func testReleaseFinishedByAnother(t *testing.T, st store.Store) {
	r := start(t, st, []int{1})
	o := r.run(t, "a1")
	row := open(t, r.m[1], o, "work", "")
	if err := r.m[0].Release(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if got, err := st.WorkspaceGet(context.Background(), "acme", row.ID); err != nil || got.State != store.WorkspaceReleasing {
		t.Fatalf("after a release on a replica without the worker: %+v, %v; want releasing", got, err)
	}
	eventually(t, "the worker's replica to finish the release", func() bool {
		_, err := st.WorkspaceGet(context.Background(), "acme", row.ID)
		var nf *store.ErrNotFound
		_, statErr := os.Stat(r.c.Dir(row.ID))
		return errors.As(err, &nf) && os.IsNotExist(statErr)
	})
}

// A workspace opened through one replica is used through the other.
func testOpenOnAOpsOnB(t *testing.T, st store.Store) {
	r := start(t, st, nil)
	o := r.run(t, "a1")
	row := open(t, r.m[0], o, "work", "")
	got, err := r.m[1].Get(context.Background(), o, "work")
	if err != nil || got.ID != row.ID {
		t.Fatalf("B's view of A's workspace: %+v, %v", got, err)
	}
	if exists, err := r.m[1].Status(context.Background(), got); err != nil || !exists {
		t.Errorf("status through B: %v, %v", exists, err)
	}
}

// Two runs racing to pin one parked durable workspace through two replicas:
// one wins, the other is told it is held, in every round.
func testDurablePinRace(t *testing.T, st store.Store) {
	r := start(t, st, nil)
	first := r.run(t, "a0")
	open(t, r.m[0], first, "work", store.WorkspaceLifetimeDurable)
	if err := r.m[0].ReleaseRun(context.Background(), first.RunID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(context.Background(), first.RunID, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.m[0].ReleaseRun(context.Background(), first.RunID); err != nil {
		t.Fatal(err)
	}
	for round := range 10 {
		a, b := r.run(t, "ra"), r.run(t, "rb")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, o := range []lifecycle.Owner{a, b} {
			wg.Add(1)
			safego.Go(context.Background(), "clustertest.pin_race", func() {
				defer wg.Done()
				_, errs[i] = r.m[i].Open(context.Background(), o, lifecycle.OpenRequest{Name: "work", Toolchains: []string{"python", "node"}, Lifetime: store.WorkspaceLifetimeDurable})
			})
		}
		wg.Wait()
		won, held := 0, 0
		var winner lifecycle.Owner
		for i, err := range errs {
			var h *lifecycle.HeldError
			switch {
			case err == nil:
				won++
				winner = []lifecycle.Owner{a, b}[i]
			case errors.As(err, &h):
				held++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if won != 1 || held != 1 {
			t.Fatalf("round %d: %d won, %d held; want one each", round, won, held)
		}
		// The winner releases it for the next round.
		row, err := r.m[0].Get(context.Background(), winner, "work")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.m[0].Release(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
}

// A worker that restarts is reconciled by each replica it attaches to: its
// workspace is ready through both again.
func testWorkerRestart(t *testing.T, st store.Store) {
	r := start(t, st, nil)
	o := r.run(t, "a1")
	row := open(t, r.m[0], o, "work", store.WorkspaceLifetimeDurable)
	r.c.StopWorker()
	r.c.StartWorker()
	for i, m := range r.m {
		got, err := m.Get(context.Background(), o, "work")
		if err != nil || got.ID != row.ID || got.State != store.WorkspaceReady {
			t.Fatalf("replica %d after the restart: %+v, %v", i, got, err)
		}
		if exists, err := m.Status(context.Background(), got); err != nil || !exists {
			t.Errorf("replica %d's status: %v, %v", i, exists, err)
		}
	}
}

// An op through a replica the worker is not attached to fails worker_lost
// (no forwarding between replicas, D-P12-11); through its own, it runs.
func testOpOnAReplicaWithoutTheWorker(t *testing.T, st store.Store) {
	r := start(t, st, []int{1})
	o := r.run(t, "a1")
	row := open(t, r.m[1], o, "work", "")
	if _, err := r.m[0].Status(context.Background(), row); dispatch.ClassOf(err) != workspace.ClassWorkerLost {
		t.Errorf("status through the replica without the worker: %v, want worker_lost", err)
	}
	if exists, err := r.m[1].Status(context.Background(), row); err != nil || !exists {
		t.Errorf("status through its own replica: %v, %v", exists, err)
	}
}
