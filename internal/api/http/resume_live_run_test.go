package http

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A row says pause_state='paused' both when it is restored data and when its
// loop is alive and parked at a runtime pause. The resume must re-dispatch the
// first and leave the second exactly as it is: rewriting a live run's row (to
// running, a fresh heartbeat, this replica, or failed) corrupts what its own
// loop and the next snapshot read, and re-dispatching another replica's live
// run runs the conversation twice.

// resumerAgents is a resolvable agent for the paused run below.
var resumerAgents = map[string]config.AgentDef{
	"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}},
}

// pausedRunOnReplica builds a server whose store holds one paused run, created
// by replicaID, whose transcript ends on a pending tool_result — a run the
// resume would otherwise re-dispatch.
func pausedRunOnReplica(t *testing.T, agents map[string]config.AgentDef, replicaID string) (*Server, store.Run) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      agents,
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	srv, _ := makeServer(t, &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}, cfg)
	rs := &replicaColumnStore{Store: srv.store, replica: map[string]string{}}
	srv.store = rs
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_live", UserID: "alice", Model: "stub-model", ReplicaID: replicaID})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.SetRunReplica(ctx, run.ID, replicaID); err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}})
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read", Input: json.RawMessage(`{}`)}})
	appendResumeEvent(t, srv, run.ID, "tool_result", providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read"}, Text: "r"})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	got, err := srv.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return srv, got
}

// replicaColumnStore gives the sqlite test store the runs.replica_id column
// postgres has. sqlite is the single-replica backend and does not keep it, so
// without this the cluster cases would not be reachable here.
type replicaColumnStore struct {
	store.Store
	mu      sync.Mutex
	replica map[string]string // run id -> replica id
}

func (s *replicaColumnStore) withReplica(r store.Run) store.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ReplicaID = s.replica[r.ID]
	return r
}

func (s *replicaColumnStore) SetRunReplica(_ context.Context, runID, replicaID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replica[runID] = replicaID
	return nil
}

func (s *replicaColumnStore) GetRun(ctx context.Context, runID string) (store.Run, error) {
	r, err := s.Store.GetRun(ctx, runID)
	if err != nil {
		return r, err
	}
	return s.withReplica(r), nil
}

func (s *replicaColumnStore) ListPausedRuns(ctx context.Context) ([]store.Run, error) {
	runs, err := s.Store.ListPausedRuns(ctx)
	for i := range runs {
		runs[i] = s.withReplica(runs[i])
	}
	return runs, err
}

// holdInCancelRegistry is what a live loop on this replica looks like to the
// resume: registered at run start and still there while it is parked.
func holdInCancelRegistry(t *testing.T, srv *Server, run store.Run) {
	t.Helper()
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: run.AgentID, RunID: run.ID, SessionID: run.SessionID, UserID: run.UserID, StartedAt: time.Now()}, func(error) {}); err != nil {
		t.Fatal(err)
	}
}

// resumeEntry is one of the two ways a paused row is resumed. Both go through
// the same rule.
type resumeEntry struct {
	name string
	// resume runs the pass and returns (resumed, already live, warnings).
	resume func(srv *Server) (int, int, []string)
}

var resumeEntries = []resumeEntry{
	{"restore", func(srv *Server) (int, int, []string) {
		var result snapshot.RestoreResult
		srv.finishRestore(context.Background(), &result)
		return result.PausedRunsResumed, result.PausedRunsAlreadyLive, result.Warnings
	}},
	{"boot", func(srv *Server) (int, int, []string) {
		// main.go's boot pass calls ResumePausedRuns, which returns this
		// report's resumed count and warnings; the already-live count is read
		// from the report itself.
		r := srv.resumePausedRunsReport(context.Background())
		return r.Resumed, r.AlreadyLive, r.Warnings
	}},
}

// assertRowUntouched fails if the resume wrote anything about the run: its
// row fields or a transcript event.
func assertRowUntouched(t *testing.T, srv *Server, before store.Run) {
	t.Helper()
	ctx := context.Background()
	got, err := srv.store.GetRun(ctx, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != before.Status || got.PauseState != before.PauseState ||
		!got.LastHeartbeatAt.Equal(before.LastHeartbeatAt) || got.ReplicaID != before.ReplicaID || got.ErrorMsg != before.ErrorMsg {
		t.Errorf("a live run's row was rewritten by the resume:\n before status=%s pause_state=%s heartbeat=%v replica=%q err=%q\n after  status=%s pause_state=%s heartbeat=%v replica=%q err=%q",
			before.Status, before.PauseState, before.LastHeartbeatAt, before.ReplicaID, before.ErrorMsg,
			got.Status, got.PauseState, got.LastHeartbeatAt, got.ReplicaID, got.ErrorMsg)
	}
	evs, err := srv.store.GetRunEventsSince(ctx, before.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Errorf("a live run's transcript has %d events after the resume, want the 3 it had", len(evs))
	}
}

func assertReportedAlreadyLive(t *testing.T, resumed, live int, warnings []string) {
	t.Helper()
	if resumed != 0 || live != 1 || len(warnings) != 0 {
		t.Errorf("resume reported resumed=%d already_live=%d warnings=%v; want 0, 1 and no warnings", resumed, live, warnings)
	}
}

func TestResumePausedRuns_ARunLiveParkedOnThisReplicaIsLeftUntouchedAndReportedLive(t *testing.T) {
	for _, e := range resumeEntries {
		t.Run(e.name, func(t *testing.T) {
			srv, run := pausedRunOnReplica(t, resumerAgents, "")
			holdInCancelRegistry(t, srv, run)
			resumed, live, warnings := e.resume(srv)
			assertReportedAlreadyLive(t, resumed, live, warnings)
			assertRowUntouched(t, srv, run)
		})
	}
}

// A resolve failure used to mark the live run failed before the resume
// noticed it was live. The liveness check now comes before the resolve.
func TestResumePausedRuns_ALiveParkedRunWhoseAgentNoLongerResolvesIsNotFailed(t *testing.T) {
	for _, e := range resumeEntries {
		t.Run(e.name, func(t *testing.T) {
			srv, run := pausedRunOnReplica(t, map[string]config.AgentDef{}, "")
			holdInCancelRegistry(t, srv, run)
			resumed, live, warnings := e.resume(srv)
			assertReportedAlreadyLive(t, resumed, live, warnings)
			assertRowUntouched(t, srv, run)
		})
	}
}

// A different run holding the paused row's agent_id is not this run being
// live: the row cannot be resumed under that id, and it is refused with a
// warning before anything about it is written.
func TestResumePausedRuns_ARunWhoseAgentIDAnotherRunHoldsIsLeftUntouchedWithAWarning(t *testing.T) {
	for _, e := range resumeEntries {
		t.Run(e.name, func(t *testing.T) {
			srv, run := pausedRunOnReplica(t, resumerAgents, "")
			other := run
			other.ID = "r_other"
			holdInCancelRegistry(t, srv, other)
			resumed, live, warnings := e.resume(srv)
			if resumed != 0 || live != 0 || len(warnings) != 1 {
				t.Errorf("resume reported resumed=%d already_live=%d warnings=%v; want 0, 0 and one warning", resumed, live, warnings)
			}
			assertRowUntouched(t, srv, run)
		})
	}
}

// The pause manager holds every live loop from entry to exit; a run it holds
// is live even when the cancel registry does not have it.
func TestResumePausedRuns_ARunHeldByThePauseManagerIsLeftUntouchedAndReportedLive(t *testing.T) {
	for _, e := range resumeEntries {
		t.Run(e.name, func(t *testing.T) {
			srv, run := pausedRunOnReplica(t, resumerAgents, "")
			mgr := pause.NewManager(srv.store, 0)
			srv.SetPauseManager(mgr)
			mgr.RegisterRun(run.ID)
			resumed, live, warnings := e.resume(srv)
			assertReportedAlreadyLive(t, resumed, live, warnings)
			assertRowUntouched(t, srv, run)
		})
	}
}

// Another replica, alive by its replicas-table heartbeat, owns the run's loop
// parked at a cluster pause. This replica must not start a second copy.
func TestResumePausedRuns_ARunOwnedByAnotherLiveReplicaIsLeftUntouchedAndReportedLive(t *testing.T) {
	for _, e := range resumeEntries {
		t.Run(e.name, func(t *testing.T) {
			srv, run := pausedRunOnReplica(t, resumerAgents, "rep-b")
			now := time.Now()
			srv.SetCoord(nil, &fakeReplicaLister{rows: []coord.Replica{
				{ID: "rep-a", StartedAt: now, LastHeartbeatAt: now},
				{ID: "rep-b", StartedAt: now, LastHeartbeatAt: now},
			}}, "rep-a")
			resumed, live, warnings := e.resume(srv)
			assertReportedAlreadyLive(t, resumed, live, warnings)
			assertRowUntouched(t, srv, run)
		})
	}
}

// A replicas table that cannot be read gives no answer about the owner.
// Resuming on a guess could start a second copy, so the run stays as it is
// and the pass says why.
func TestResumePausedRuns_AnUnreadableOwnerLivenessLeavesTheRunUntouchedWithAWarning(t *testing.T) {
	for _, e := range resumeEntries {
		t.Run(e.name, func(t *testing.T) {
			srv, run := pausedRunOnReplica(t, resumerAgents, "rep-b")
			srv.SetCoord(nil, &fakeReplicaLister{err: errors.New("db unreachable")}, "rep-a")
			resumed, live, warnings := e.resume(srv)
			if resumed != 0 || live != 0 || len(warnings) != 1 {
				t.Errorf("resume reported resumed=%d already_live=%d warnings=%v; want 0, 0 and one warning", resumed, live, warnings)
			}
			assertRowUntouched(t, srv, run)
		})
	}
}

// The crash-recovery case: nothing live owns the run. Its owner is stale,
// gone from the replicas table, this replica's own earlier process, or not
// recorded. Each is resumed here, and the row now names this replica.
func TestResumePausedRuns_ARunWhoseOwnerIsNotLiveIsResumedHere(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name    string
		replica string
		rows    []coord.Replica
	}{
		{"stale owner", "rep-b", []coord.Replica{{ID: "rep-b", StartedAt: now, LastHeartbeatAt: now.Add(-10 * time.Minute)}}},
		{"absent owner", "rep-b", nil},
		{"this replica's earlier process", "rep-a", nil},
		{"no owner recorded", "", nil},
	} {
		for _, e := range resumeEntries {
			t.Run(c.name+"/"+e.name, func(t *testing.T) {
				srv, run := pausedRunOnReplica(t, resumerAgents, c.replica)
				srv.SetCoord(nil, &fakeReplicaLister{rows: c.rows}, "rep-a")
				resumed, live, warnings := e.resume(srv)
				if resumed != 1 || live != 0 || len(warnings) != 0 {
					t.Fatalf("resume reported resumed=%d already_live=%d warnings=%v; want 1, 0 and no warnings", resumed, live, warnings)
				}
				waitRunCompleted(t, srv, run.ID)
				got, err := srv.store.GetRun(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.ReplicaID != "rep-a" {
					t.Errorf("resumed run's replica_id = %q, want rep-a (this replica owns it now)", got.ReplicaID)
				}
			})
		}
	}
}

// waitRunCompleted polls until the detached resumed loop finishes, so the test
// does not close the store under it.
func waitRunCompleted(t *testing.T, srv *Server, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := srv.store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		switch got.Status {
		case store.RunCompleted:
			return
		case store.RunFailed:
			t.Fatalf("resumed run failed: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed run did not complete (status=%q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
