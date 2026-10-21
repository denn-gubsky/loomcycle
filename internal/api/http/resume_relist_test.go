package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func relistFixture(t *testing.T) (*Server, *config.Config, *scriptedProvider, store.Run) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "ran"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()
	sess, _ := srv.store.CreateSession(ctx, "", "ghost", "alice")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ghost", UserID: "alice", Model: "stub-model"})
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	return srv, cfg, prov, run
}

// A run a resume pass refused and marked failed is not found by the next pass
// (in a cluster that pass runs every interval): it would be refused again —
// another refusal on its transcript, its failure republished — or, once its
// agent resolved again, re-dispatched while its row reads failed.
func TestResumePausedRuns_ARunMarkedFailedIsNotResumedByALaterPass(t *testing.T) {
	srv, cfg, prov, run := relistFixture(t)
	ctx := context.Background()

	if n, w := srv.ResumePausedRuns(ctx); n != 0 || len(w) != 1 {
		t.Fatalf("first pass = resumed %d, %d warning(s); want the run refused once", n, len(w))
	}
	got, _ := srv.store.GetRun(ctx, run.ID)
	if got.Status != store.RunFailed || got.PauseState == store.PauseStatePaused {
		t.Fatalf("after the refusal: status %s pause_state %q, want failed and no longer paused", got.Status, got.PauseState)
	}
	before := len(runEventsOf(t, srv, run))

	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	if n, w := srv.ResumePausedRuns(ctx); n != 0 || len(w) != 0 {
		t.Errorf("second pass = resumed %d, warnings %v; want nothing", n, w)
	}
	time.Sleep(200 * time.Millisecond)
	if c := prov.calls.Load(); c != 0 {
		t.Errorf("model called %d time(s): a run already reported failed was re-dispatched", c)
	}
	if after := len(runEventsOf(t, srv, run)); after != before {
		t.Errorf("the second pass wrote %d event(s) to a run it should not have touched", after-before)
	}
}

// Two passes can list the same paused row — a snapshot restore and another
// replica's periodic pass — and only the one that claims it may write to it.
// The other finds it claimed and leaves it exactly as it is.
func TestResumePausedRun_ARowAnotherPassClaimedIsLeftAlone(t *testing.T) {
	srv, cfg, prov, run := relistFixture(t)
	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	ctx := context.Background()
	listed, err := srv.store.ListPausedRuns(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListPausedRuns = %d, %v", len(listed), err)
	}
	// Another pass claims it between this pass's list and its resume.
	if ok, err := srv.store.ClaimPausedRun(ctx, run.ID); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	before := len(runEventsOf(t, srv, run))

	if err := srv.resumePausedRun(listed[0]); !errors.Is(err, errRunAlreadyLive) {
		t.Errorf("resume of a row another pass claimed = %v, want errRunAlreadyLive", err)
	}
	time.Sleep(200 * time.Millisecond)
	if c := prov.calls.Load(); c != 0 {
		t.Errorf("model called %d time(s): the run was dispatched twice", c)
	}
	if after := len(runEventsOf(t, srv, run)); after != before {
		t.Errorf("the losing pass wrote %d event(s)", after-before)
	}
}

func runEventsOf(t *testing.T, srv *Server, run store.Run) []store.Event {
	t.Helper()
	evs, err := srv.store.GetRunEventsSince(context.Background(), run.ID, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}
