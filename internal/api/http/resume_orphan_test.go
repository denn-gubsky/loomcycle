package http

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
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
// member whose walk's starter has ended still resumes.
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
