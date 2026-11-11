package http

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A resumed top-level run in cluster mode holds its session's advisory lock
// (fakeClusterLocker stands in for Postgres), lets it go only while it waits,
// and is never failed — only put back to paused — when it cannot take it back.

func clusterResumeServer(t *testing.T, prov providers.Provider, staleAfter time.Duration) (*Server, *fakeClusterLocker) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"worker": {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:worker."}}
	cfg.Env.HeartbeatStaleAfter = staleAfter
	srv, _ := makeServer(t, prov, cfg)
	locker := newFakeClusterLocker()
	srv.sessionLockPG = locker
	return srv, locker
}

func runStatusIs(srv *Server, runID string, want store.RunStatus) bool {
	r, err := srv.store.GetRun(context.Background(), runID)
	return err == nil && r.Status == want
}

// With a slot free the run never waits, so it never lets the lock go: one
// take, no release and retake a contender could slip into.
func TestResumeCluster_AFreeSlotKeepsTheSessionLockThroughout(t *testing.T) {
	srv, locker := clusterResumeServer(t, &recordingScriptedProvider{defaultS: endTurn()}, 0)
	run := pausedRunRow(t, srv, "worker", "a_cluster_free", false, oneUserSeg("go"))
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)
	if n := locker.taken(); n != 1 {
		t.Errorf("cluster lock takes = %d, want 1: the run let its lock go with a slot free", n)
	}
}

// A run that waits for its slot lets the lock go and beats meanwhile; if
// another replica holds the session when it is admitted, it goes back to
// paused — never failed — and is resumed once the session is free.
func TestResumeCluster_ASessionTakenWhileItWaitedPutsTheRunBackToPaused(t *testing.T) {
	srv, locker := clusterResumeServer(t, &recordingScriptedProvider{defaultS: endTurn()}, 300*time.Millisecond)
	srv.sem = concurrency.New(1, 8, 0)
	held, err := srv.sem.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := pausedRunRow(t, srv, "worker", "a_cluster_wait", false, oneUserSeg("go"))
	resumeOne(t, srv)
	waitFor(t, "the run to queue with its lock let go", func() bool {
		return srv.sem.Stats().Queued == 1 && locker.holding() == 0
	})
	// It beats while it waits, so another replica still counts it live.
	time.Sleep(700 * time.Millisecond)
	r, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(r.LastHeartbeatAt); age > 300*time.Millisecond {
		t.Errorf("the waiting run's heartbeat is %s old, past the 300ms stale bound: another replica would see no live run", age)
	}
	other, ok := locker.TryLock(context.Background(), run.SessionID) // another replica's request
	if !ok {
		t.Fatal("fixture: the session lock is held while the run waits")
	}
	held()
	waitFor(t, "the run to go back to paused", func() bool {
		r, err := srv.store.GetRun(context.Background(), run.ID)
		return err == nil && r.Status == store.RunRunning && r.PauseState == store.PauseStatePaused
	})
	if runStatusIs(srv, run.ID, store.RunFailed) {
		t.Fatal("the run was failed for a busy session")
	}
	other()
	waitFor(t, "the run to be resumed once the session is free", func() bool { return runStatusIs(srv, run.ID, store.RunCompleted) })
}

// overlapProvider records how many of its calls ran at once.
type overlapProvider struct {
	recordingScriptedProvider
	mu         sync.Mutex
	active, mx int
}

func (p *overlapProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.mx {
		p.mx = p.active
	}
	p.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return p.recordingScriptedProvider.Call(ctx, req)
}

// Two paused runs of one session: neither is a live turn, so they must not
// refuse each other on every pass. One resumes, the other after it, and the
// session never runs two turns at once.
func TestResumeCluster_TwoPausedRunsOfOneSessionBothResumeOneAtATime(t *testing.T) {
	prov := &overlapProvider{recordingScriptedProvider: recordingScriptedProvider{defaultS: endTurn()}}
	srv, _ := clusterResumeServer(t, prov, 0)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	var runs []store.Run
	for _, id := range []string{"a_sibling_1", "a_sibling_2"} {
		run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: id, UserID: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		appendResumeEvent(t, srv, run.ID, "user_input", oneUserSeg("go"))
		if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d (warnings %v), want 1 now and the other once the session is free", n, warnings)
	}
	for _, run := range runs {
		waitFor(t, "both siblings to complete", func() bool { return runStatusIs(srv, run.ID, store.RunCompleted) })
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if prov.mx != 1 {
		t.Errorf("model calls of the session at once = %d, want 1", prov.mx)
	}
}

// A sibling that is pausing is still running its turn: a resume of another
// paused row of the session is refused until it has parked, or the session
// would run two turns at once.
func TestResumeCluster_APausingSiblingKeepsTheSessionBusy(t *testing.T) {
	srv, _ := clusterResumeServer(t, &recordingScriptedProvider{defaultS: endTurn()}, 0)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	pausing, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_pausing_sibling", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetRunPauseState(ctx, pausing.ID, store.PauseStatePausing); err != nil {
		t.Fatal(err)
	}
	paused, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_paused_sibling", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, paused.ID, "user_input", oneUserSeg("go"))
	if err := srv.store.SetRunPauseState(ctx, paused.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if paused, err = srv.store.GetRun(ctx, paused.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.resumePausedRun(paused); !errors.Is(err, runner.ErrSessionBusy) {
		t.Fatalf("resuming beside a pausing sibling = %v, want %v", err, runner.ErrSessionBusy)
	}
}

// The busy-session retry is scheduled once the run's loop has let go of
// everything: scheduled before, its first try finds the run still registered
// (errRunAlreadyLive) and gives up, leaving the row paused on one replica.
func TestResumeCluster_TheBusyRetryIsScheduledAfterTheTeardown(t *testing.T) {
	srv, locker := clusterResumeServer(t, &recordingScriptedProvider{defaultS: endTurn()}, 0)
	srv.sem = concurrency.New(1, 8, 0)
	held, err := srv.sem.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := pausedRunRow(t, srv, "worker", "a_requeue_order", false, oneUserSeg("go"))
	var mu sync.Mutex
	requeued, registeredThen := false, false
	srv.runPhaseHook = func(phase string) {
		if phase != "resume.requeue" {
			return
		}
		_, live := srv.cancelReg.Get(run.AgentID)
		mu.Lock()
		requeued, registeredThen = true, live
		mu.Unlock()
	}
	resumeOne(t, srv)
	waitFor(t, "the run to queue with its lock let go", func() bool {
		return srv.sem.Stats().Queued == 1 && locker.holding() == 0
	})
	other, ok := locker.TryLock(context.Background(), run.SessionID)
	if !ok {
		t.Fatal("fixture: the session lock is held while the run waits")
	}
	held()
	waitFor(t, "the busy retry to be scheduled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return requeued
	})
	mu.Lock()
	if registeredThen {
		t.Error("the busy retry was scheduled while the run was still registered: its first try finds it live and gives up")
	}
	mu.Unlock()
	other()
	waitFor(t, "the run to be resumed once the session is free", func() bool { return runStatusIs(srv, run.ID, store.RunCompleted) })
}
