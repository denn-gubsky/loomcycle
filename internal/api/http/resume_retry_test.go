package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stillPaused reports whether the run's row is paused and unclaimed.
func stillPaused(t *testing.T, srv *Server, run store.Run) bool {
	t.Helper()
	r, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status == store.RunRunning && r.PauseState == store.PauseStatePaused
}

// A busy-session retry does not resume a run while the runtime is paused — a
// shutdown is one — where the run would only start and be cut off: it claims
// nothing and starts no loop until the pause ends. (A runtime resume on one
// replica flips every paused row to running itself, pause.Manager.Resume, so
// the pause's end is not exercised here.)
func TestResume_BusyRetryClaimsNothingWhileTheRuntimeIsPaused(t *testing.T) {
	srv := resumeAlignServer(t, &recordingScriptedProvider{defaultS: endTurn()})
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	srv.SetLifetime(life)
	mgr := pause.NewManager(srv.store, 50*time.Millisecond)
	srv.SetPauseManager(mgr)
	run := pausedRunRow(t, srv, "worker", "a_retry_pause", false, oneUserSeg("go"))
	hold, ok := srv.holdSession(run.SessionID, false)
	if !ok {
		t.Fatal("fixture: could not hold the session")
	}
	if n, _ := srv.ResumePausedRuns(context.Background()); n != 0 {
		t.Fatalf("resumed %d with the session held, want 0", n)
	}
	if _, err := mgr.Pause(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	hold.release()
	time.Sleep(2 * time.Second) // past the retry's first two tries
	// A run started now would park at the pause gate and read paused again:
	// what matters is that no loop was started for it.
	if _, live := srv.cancelReg.Get(run.AgentID); live || !stillPaused(t, srv, run) {
		t.Fatal("the busy retry started the run while the runtime was paused")
	}
}

// A busy-session retry ends with the server: once its lifetime ends it claims
// nothing, so a stopping process starts no run it will not finish.
func TestResume_BusyRetryStopsWithTheServer(t *testing.T) {
	srv := resumeAlignServer(t, &recordingScriptedProvider{defaultS: endTurn()})
	life, stop := context.WithCancel(context.Background())
	srv.SetLifetime(life)
	run := pausedRunRow(t, srv, "worker", "a_retry_stop", false, oneUserSeg("go"))
	hold, ok := srv.holdSession(run.SessionID, false)
	if !ok {
		t.Fatal("fixture: could not hold the session")
	}
	if n, _ := srv.ResumePausedRuns(context.Background()); n != 0 {
		t.Fatalf("resumed %d with the session held, want 0", n)
	}
	stop()
	hold.release()
	time.Sleep(2 * time.Second)
	if !stillPaused(t, srv, run) {
		t.Fatal("the busy retry resumed the run after the server's lifetime ended")
	}
}
