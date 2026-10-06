package http

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A paused child whose parent was cancelled before the child was resumed is
// not resumed into a run nobody will read or cancel: it ends cancelled, saying
// whose end it followed, and leaves the paused list.
func TestResumePausedRuns_AChildOfACancelledParentIsCancelledNotResumed(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily()
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	child := workerRun(t, srv, lead, "a_c1", "hold one", true)
	spawnedInPoll(t, srv, lead, []string{child.ID})
	if err := srv.store.FinishRun(ctx, lead.ID, store.RunCancelled, "operator stop", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}

	n, warns := srv.ResumePausedRuns(ctx)
	if n != 0 || len(warns) != 1 || !strings.Contains(warns[0], "its parent run "+lead.ID+" ended (cancelled)") {
		t.Fatalf("resumed %d (warnings: %v), want none and the child refused as orphaned", n, warns)
	}
	ended, err := srv.store.GetRun(ctx, child.ID)
	if err != nil || ended.Status != store.RunCancelled || !strings.Contains(ended.StopReason, lead.ID+" ended (cancelled)") {
		t.Fatalf("the child = %+v, %v; want cancelled naming its parent's end", ended, err)
	}
	if ended.PauseState == store.PauseStatePaused {
		t.Error("the cancelled child is still listed as paused")
	}
	if _, live := srv.cancelReg.Get(child.AgentID); live {
		t.Error("the orphaned child is running")
	}
}

// endParentOnFlip ends a run's parent the moment a resume flips that run back
// to running — after the resume checked the parent and before the pass ends.
type endParentOnFlip struct {
	store.Store
	child, parent string
	once          sync.Once
}

func (e *endParentOnFlip) SetRunPauseState(ctx context.Context, runID, state string) error {
	err := e.Store.SetRunPauseState(ctx, runID, state)
	if runID == e.child && state == store.PauseStateRunning {
		e.once.Do(func() {
			_ = e.Store.FinishRun(ctx, e.parent, store.RunCancelled, "operator stop", store.Usage{}, "")
		})
	}
	return err
}

// A parent that ends after its child was resumed in the same pass — cancelled
// meanwhile — does not leave the child running for nobody: the pass checks
// again once every run is resumed, and cancels it.
func TestResumePausedRuns_AChildWhoseParentEndsDuringThePassIsCancelled(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily()
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	lead := pausedLead(t, srv) // running elsewhere: not paused here
	child := workerRun(t, srv, lead, "a_c1", "hold one", true)
	srv.store = &endParentOnFlip{Store: srv.store, child: child.ID, parent: lead.ID}

	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d (warnings: %v), want the child, its parent still running at its check", n, warns)
	}
	ended := waitWalkRunStatus(t, srv.store, child.ID, store.RunCancelled)
	if !strings.Contains(ended.StopReason, "cancelled on resume: its parent run "+lead.ID+" ended (cancelled)") {
		t.Errorf("the child ended %q, want cancelled naming its parent's end", ended.StopReason)
	}
}

// A detached walk is its own root: its members have the walk as their parent,
// not the run that started it, and outlive that starter as they did live. A
// member whose walk's starter has ended still resumes, its walk live here.
func TestResumePausedRuns_ADetachedWalksMemberWithAnEndedStarterStillResumes(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily(answer("unused"))
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	starter := pausedLead(t, srv)
	if err := srv.store.FinishRun(ctx, starter.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	wsess, err := srv.store.CreateSession(ctx, "", "team:rev", "alice")
	if err != nil {
		t.Fatal(err)
	}
	walk, err := srv.store.CreateRun(ctx, wsess.ID, store.RunIdentity{AgentID: "team:rev", UserID: "alice", ParentRunID: starter.ID,
		RunConfig: runConfigRecord{Team: &teamWalkRecord{Name: "rev", Mode: "detach"}}.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	member := memberRun(t, srv, walk, "a_member", "hold the member")
	srv.walks.add(walk.ID, wsess.ID, runStateMeta{RunID: walk.ID}, func(error) {})
	t.Cleanup(func() { srv.walks.remove(walk.ID) })

	if n, warns := srv.ResumePausedRuns(ctx); n != 1 || len(warns) != 0 {
		t.Fatalf("resumed %d (warnings: %v), want the member", n, warns)
	}
	waitFor(t, "the member to run", func() bool {
		_, live := srv.cancelReg.Get(member.AgentID)
		return live
	})
	if run, err := srv.store.GetRun(ctx, member.ID); err != nil || run.Status != store.RunRunning {
		t.Errorf("the member = %+v, %v; want running", run, err)
	}
}

// A walk runs no loop and lives only in the process that runs it: a restart on
// the same database leaves its row running with nothing behind it, whatever
// mode it ran in. Its paused members are not resumed for a walk that will
// never read them, and its row is closed as interrupted rather than reading
// running until the stale sweeper fails it.
func TestResumePausedRuns_AMemberOfAWalkARestartLeftRunningIsCancelled(t *testing.T) {
	for _, mode := range []string{"detach", "wait"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			prov := newBGFamily()
			srv, _ := makeServer(t, prov, bgResumeConfig(""))
			settle(t, srv, prov)
			starter := pausedLead(t, srv) // running elsewhere: not paused here
			walk := walkRow(t, srv, starter, mode)
			member := memberRun(t, srv, walk, "a_member", "hold the member")

			n, warns := srv.ResumePausedRuns(ctx)
			if n != 0 || len(warns) != 1 || !strings.Contains(warns[0], "its team walk "+walk.ID+" is not here") {
				t.Fatalf("resumed %d (warnings: %v), want none and the member refused, its walk gone", n, warns)
			}
			gone, err := srv.store.GetRun(ctx, member.ID)
			if err != nil || gone.Status != store.RunCancelled || gone.PauseState == store.PauseStatePaused {
				t.Fatalf("the member = %+v, %v; want cancelled and no longer paused", gone, err)
			}
			ended, err := srv.store.GetRun(ctx, walk.ID)
			if err != nil || ended.Status != store.RunFailed || !strings.Contains(ended.ErrorMsg, "the walk was interrupted") || !strings.Contains(ended.ErrorMsg, "run it again") {
				t.Errorf("the walk's row = %+v, %v; want failed as interrupted", ended, err)
			}
		})
	}
}

// A walk's members run in the walk's own process, so a member that another
// live replica holds says its walk is alive there: neither is touched.
func TestResumePausedRuns_AWalkWhoseMemberIsLiveOnAnotherReplicaIsLeftRunning(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily()
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	rs := &replicaColumnStore{Store: srv.store, replica: map[string]string{}} // sqlite keeps no replica column
	srv.store = rs
	now := time.Now()
	srv.SetCoord(nil, &fakeReplicaLister{rows: []coord.Replica{
		{ID: "rep-a", StartedAt: now, LastHeartbeatAt: now},
		{ID: "rep-b", StartedAt: now, LastHeartbeatAt: now},
	}}, "rep-a")
	starter := pausedLead(t, srv)
	walk := walkRow(t, srv, starter, "detach")
	sess, err := srv.store.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_member", UserID: "alice", Model: "stub-model", ParentRunID: walk.ID, ParentAgentID: walk.AgentID,
		ReplicaID: "rep-b", ParentContext: &store.ParentContext{WalkID: walk.ID, State: "review", StateVisit: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.SetRunReplica(ctx, member.ID, "rep-b"); err != nil {
		t.Fatal(err)
	}
	markPaused(t, srv, member)

	r := srv.resumePausedRunsReport(ctx)
	if r.Resumed != 0 || r.AlreadyLive != 1 || len(r.Warnings) != 0 {
		t.Fatalf("resume = %+v, want the member reported live elsewhere", r)
	}
	if got, err := srv.store.GetRun(ctx, walk.ID); err != nil || got.Status != store.RunRunning {
		t.Errorf("the walk's row = %+v, %v; want still running", got, err)
	}
	if got, err := srv.store.GetRun(ctx, member.ID); err != nil || got.Status != store.RunRunning || got.PauseState != store.PauseStatePaused {
		t.Errorf("the member = %+v, %v; want still paused", got, err)
	}
}

// A run cancelled on resume because its parent ended goes no further, and the
// walks it started in poll mode go with it: one still running is closed as
// interrupted, as its own resume would have closed it, not left running for
// nobody.
func TestResumePausedRuns_AnOrphanedRunsPollWalkIsClosedAsInterrupted(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily()
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	grand := pausedLead(t, srv)
	if err := srv.store.FinishRun(ctx, grand.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	lead := workerRun(t, srv, grand, "a_mid", "unused", false)
	walk := walkRow(t, srv, lead, "poll")
	appendResumeEvent(t, srv, lead.ID, "tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "TeamDef", Input: json.RawMessage(`{"op":"run","name":"rev","mode":"poll"}`)}})
	appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_1", RunID: walk.ID, Agent: "team:rev", Mode: "poll", Kind: "team", Team: "rev"}})
	appendResumeEvent(t, srv, lead.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}})
	appendResumeEvent(t, srv, lead.ID, "tool_result", providers.Event{Type: providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "TeamDef"}, Text: `{"run_id":"` + walk.ID + `"}`})
	markPaused(t, srv, lead)

	n, warns := srv.ResumePausedRuns(ctx)
	if n != 0 || len(warns) != 1 || !strings.Contains(warns[0], "its parent run "+grand.ID+" ended (completed)") {
		t.Fatalf("resumed %d (warnings: %v), want none and the run refused as orphaned", n, warns)
	}
	ended, err := srv.store.GetRun(ctx, walk.ID)
	if err != nil || ended.Status != store.RunFailed || !strings.Contains(ended.ErrorMsg, "the walk was interrupted") {
		t.Errorf("the walk's row = %+v, %v; want failed as interrupted", ended, err)
	}
}

// walkRow is a team walk's run row as a walk opens it, in the given mode,
// started by starter and left running: no walk behind it in this process.
func walkRow(t *testing.T, srv *Server, starter store.Run, mode string) store.Run {
	t.Helper()
	ctx := context.Background()
	wsess, err := srv.store.CreateSession(ctx, "", "team:rev", "alice")
	if err != nil {
		t.Fatal(err)
	}
	walk, err := srv.store.CreateRun(ctx, wsess.ID, store.RunIdentity{AgentID: "team:rev", UserID: "alice", ParentRunID: starter.ID,
		RunConfig: runConfigRecord{Team: &teamWalkRecord{Name: "rev", Mode: mode}}.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	return walk
}

// memberRun is a paused member run of a walk, as a walk spawns it: the walk is
// its parent and its walk.
func memberRun(t *testing.T, srv *Server, walk store.Run, agentID, prompt string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: "alice", Model: "stub-model", ParentRunID: walk.ID, ParentAgentID: walk.AgentID,
		ParentContext: &store.ParentContext{WalkID: walk.ID, State: "review", StateVisit: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: prompt}}},
	})
	markPaused(t, srv, run)
	return run
}
