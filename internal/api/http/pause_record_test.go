package http

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A bounded child that is inside a tool call for the whole of a runtime pause
// never parks for it, yet its live clock stopped for it. Its pause is on its
// record all the same — open once the pause is declared, so a snapshot taken
// then carries it, and ended at the resume — and a parent that re-arms its
// timeout_ms from the record afterwards (after a restart or a restore) gives
// it the time it had left when the runtime paused.
func TestResumeFanout_AChildBusyThroughAPauseIsNotChargedIt(t *testing.T) {
	const bound, pauseFor = 600 * time.Millisecond, 900 * time.Millisecond
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"breeder": {Provider: "scripted", Model: "stub-model", Tools: []string{"Agent"}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = true
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	mgr := pause.NewManager(srv.store, time.Second)
	srv.SetPauseManager(mgr)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()

	childSess, _ := srv.store.CreateSession(ctx, "", "solver", "alice")
	child, err := srv.store.CreateRun(ctx, childSess.ID, store.RunIdentity{AgentID: "a_child_busy", UserID: "alice", Model: "stub-model", RunConfig: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: child.AgentID, RunID: child.ID, SessionID: childSess.ID, UserID: "alice", StartedAt: time.Now()},
		func(error) {}); err != nil {
		t.Fatal(err)
	}
	// Live here, as a running loop is, and never at a boundary: it is in a
	// tool call from before the pause to after the resume.
	_, deregister := srv.newPauseGate(child.ID)
	defer deregister()

	pausedAt := time.Now()
	if _, err := mgr.Pause(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if rec := runRecord(t, srv, child.ID); len(rec.Pauses) != 1 || !rec.pauseOpen() {
		t.Fatalf("pauses once the pause returned = %+v, want one open: a snapshot now carries none", rec.Pauses)
	}
	time.Sleep(pauseFor)
	resumedAt := time.Now()
	if _, err := mgr.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := runRecord(t, srv, child.ID); len(rec.Pauses) != 1 || rec.pauseOpen() {
		t.Fatalf("pauses after the resume = %+v, want the one ended", rec.Pauses)
	}

	// Past start + bound already; not past it once the pause is left out.
	payload, _ := json.Marshal(providers.Event{
		Type:       providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: child.ID, Agent: "solver", TimeoutMs: int(bound / time.Millisecond)},
	})
	events := []store.Event{{Type: string(providers.EventSpawnChildStarted), Payload: payload}}
	msg, err := srv.reconcileFanoutParent(ctx, store.Run{ID: "r_parent"}, events,
		fanoutParkInfo{toolUseID: "tu_fan", input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"x","timeout_ms":600}]}`)},
		func(providers.Event) {})
	at := time.Now()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(msg.Content[0].Text), &env); err != nil || len(env.Results) != 1 {
		t.Fatalf("envelope = %s (%v)", msg.Content[0].Text, err)
	}
	if r := env.Results[0]; r.Ok || r.Status != "timeout" {
		t.Fatalf("row = %+v, want the child timed out once its time ran out", r)
	}
	want := child.StartedAt.Add(bound + resumedAt.Sub(pausedAt))
	if early, late := at.Sub(want), at.Sub(want.Add(time.Second)); early < -150*time.Millisecond || late > 0 {
		t.Errorf("the child was cut %v after the resume, want about %v (what it had left when the runtime paused)",
			at.Sub(resumedAt), want.Sub(resumedAt))
	}
}

// runRecord reads the run's configuration record.
func runRecord(t *testing.T, srv *Server, runID string) runConfigRecord {
	t.Helper()
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		t.Fatalf("run %s has no readable record: %s", runID, run.RunConfig)
	}
	return rec
}
