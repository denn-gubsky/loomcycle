package http

import (
	"context"
	"errors"
	"sync"
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

// cancelOnTranscriptRead ends the run (an operator's cancel landing) after a
// resume has claimed it, then fails the transcript read, so the resume stops
// short and gives the row back.
type cancelOnTranscriptRead struct {
	store.Store
	runID string
}

func (c *cancelOnTranscriptRead) GetTranscript(ctx context.Context, sessionID string) ([]store.Event, error) {
	_ = c.Store.FinishRun(ctx, c.runID, store.RunCancelled, "operator stop", store.Usage{}, "")
	return nil, errors.New("transcript read failed")
}

// A resume that stops short gives the row back as paused — but not one a
// cancel ended in between: that run stays cancelled and off the paused list,
// or a later pass would resurrect it.
func TestResumePausedRun_ACancelBetweenClaimAndUnclaimStays(t *testing.T) {
	srv, cfg, prov, run := relistFixture(t)
	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	ctx := context.Background()
	srv.store = &cancelOnTranscriptRead{Store: srv.store, runID: run.ID}

	if n, _ := srv.ResumePausedRuns(ctx); n != 0 {
		t.Fatalf("resumed %d, want 0", n)
	}
	got, _ := srv.store.GetRun(ctx, run.ID)
	if got.Status != store.RunCancelled || got.PauseState == store.PauseStatePaused {
		t.Errorf("run = status %s pause_state %q, want cancelled and not paused", got.Status, got.PauseState)
	}
	if listed, _ := srv.store.ListPausedRuns(ctx); len(listed) != 0 {
		t.Errorf("ListPausedRuns lists %d row(s), want none", len(listed))
	}
	if n, _ := srv.ResumePausedRuns(ctx); n != 0 || prov.calls.Load() != 0 {
		t.Errorf("a later pass resumed %d, model called %d time(s): the cancelled run was resurrected", n, prov.calls.Load())
	}
}

// quotaGateCounts is a cluster per-user counter that records what is done to
// it. Its methods match the semaphore's gate.
type quotaGateCounts struct {
	mu       sync.Mutex
	admitted int
	released int
}

func (g *quotaGateCounts) TryAcquire(context.Context, string, string, int) (bool, error) {
	return true, nil
}
func (g *quotaGateCounts) CountAdmitted(context.Context, string, string) error {
	g.mu.Lock()
	g.admitted++
	g.mu.Unlock()
	return nil
}
func (g *quotaGateCounts) Release(context.Context, string, string) {
	g.mu.Lock()
	g.released++
	g.mu.Unlock()
}
func (g *quotaGateCounts) Snapshot(context.Context) (map[string]int, error) { return nil, nil }
func (g *quotaGateCounts) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.admitted, g.released
}

// stampReplica reports every paused row as parked by a replica that is gone:
// the sqlite store keeps no replica column (cluster mode is Postgres-only).
type stampReplica struct {
	store.Store
	replica string
}

func (s stampReplica) ListPausedRuns(ctx context.Context) ([]store.Run, error) {
	runs, err := s.Store.ListPausedRuns(ctx)
	for i := range runs {
		runs[i].ReplicaID = s.replica
	}
	return runs, err
}

// In a cluster, a run a stopped replica parked still holds the per-user slot
// that replica took for it. Its resume gives that slot back — once, however
// many passes list it — and the resumed run takes its own, counted like any
// run's and given back when it ends.
func TestResumePausedRun_MovesTheUsersSlotFromTheStoppedReplica(t *testing.T) {
	srv, cfg, _, run := relistFixture(t)
	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	gate := &quotaGateCounts{}
	srv.sem.WithPerUserCap(1).WithUserQuotaStore(gate)
	srv.store = stampReplica{Store: srv.store, replica: "gone-replica"}
	ctx := context.Background()
	// The stopped replica counted it against its user at admission.
	if err := srv.store.MarkRunUserSlot(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	if n, w := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d (warnings %v), want 1", n, w)
	}
	waitWalkRunStatus(t, srv.store, run.ID, store.RunCompleted)
	if n, _ := srv.ResumePausedRuns(ctx); n != 0 {
		t.Fatalf("second pass resumed %d, want 0", n)
	}
	// The resumed run's own count is on its row, for a later resume to give
	// back should this process stop too.
	if got, err := srv.store.TakeRunUserSlot(ctx, run.ID); err != nil || got != store.RunUserSlotHeld {
		t.Errorf("resumed run's row = %v (%v), want held: its new process counted it", got, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		admitted, released := gate.counts()
		if admitted == 1 && released == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("per-user counter: %d counted, %d released; want the resumed run counted once and two releases (the stopped replica's slot, then the run's own)", admitted, released)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A database from before FinishRun cleared the pause can hold rows that ended
// while reading 'paused' — a run the old resume refused and marked failed, or
// one ended by a writer that does not go through FinishRun. The resume pass,
// periodic in a cluster, must neither list nor resume one, even once its agent
// resolves: it would run a run its row reports ended.
func TestResumePausedRuns_AnEndedRowStillMarkedPausedIsNeitherListedNorResumed(t *testing.T) {
	srv, cfg, prov, run := relistFixture(t)
	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	ctx := context.Background()
	// As an old database has it: ended, and still paused.
	if err := srv.store.FinishRun(ctx, run.ID, store.RunFailed, "", store.Usage{}, "resume failed: agent gone"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	if listed, _ := srv.store.ListPausedRuns(ctx); len(listed) != 0 {
		t.Errorf("ListPausedRuns lists %d ended row(s), want none", len(listed))
	}
	if n, w := srv.ResumePausedRuns(ctx); n != 0 || len(w) != 0 {
		t.Errorf("pass = resumed %d, warnings %v; want nothing", n, w)
	}
	time.Sleep(200 * time.Millisecond)
	if c := prov.calls.Load(); c != 0 {
		t.Errorf("model called %d time(s): an ended run was resumed", c)
	}
	if got, _ := srv.store.GetRun(ctx, run.ID); got.Status != store.RunFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
	// A pass that listed the row before it ended cannot claim it either.
	if ok, _ := srv.store.ClaimPausedRun(ctx, run.ID); ok {
		t.Error("ClaimPausedRun claimed an ended row")
	}
}

// A run admitted while the per-user cap was off took no per-user slot, so its
// resume has none of the stopped replica's to give back: a release there would
// take back another run's slot (RFC ES C4). Only the resumed run's own count
// comes and goes.
func TestResumePausedRun_GivesBackNoSlotTheStoppedReplicaNeverTook(t *testing.T) {
	srv, cfg, _, run := relistFixture(t) // admitted with no per-user slot
	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	gate := &quotaGateCounts{}
	srv.sem.WithPerUserCap(1).WithUserQuotaStore(gate)
	srv.store = stampReplica{Store: srv.store, replica: "gone-replica"}
	ctx := context.Background()

	if n, w := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d (warnings %v), want 1", n, w)
	}
	waitWalkRunStatus(t, srv.store, run.ID, store.RunCompleted)
	deadline := time.Now().Add(2 * time.Second)
	for {
		admitted, released := gate.counts()
		if admitted == 1 && released == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("per-user counter: %d counted, %d released; want the resumed run counted once and released once, and nothing given back for the stopped replica", admitted, released)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if admitted, released := gate.counts(); admitted != 1 || released != 1 {
		t.Fatalf("per-user counter settled at %d counted, %d released; want 1 and 1", admitted, released)
	}
}

// A row from before runs.user_slot (parked by an older binary) records
// nothing; its resume keeps the earlier rule and gives the stopped replica's
// slot back for a top-level run that replica admitted — once.
func TestResumePausedRun_ARowThatRecordsNoSlotKeepsTheEarlierRule(t *testing.T) {
	srv, cfg, _, _ := relistFixture(t)
	cfg.Agents["ghost"] = config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "x", Tools: []string{}}
	ctx := context.Background()
	// Only the legacy row is paused; the fixture's own run is ended.
	if listed, _ := srv.store.ListPausedRuns(ctx); len(listed) == 1 {
		_ = srv.store.FinishRun(ctx, listed[0].ID, store.RunCompleted, "end_turn", store.Usage{}, "")
	}
	sess, _ := srv.store.CreateSession(ctx, "", "ghost", "alice")
	if _, err := srv.store.SnapshotRestoreRun(ctx, store.Run{ID: "r_legacy_slot", SessionID: sess.ID, AgentID: "a_legacy_slot",
		UserID: "alice", Model: "stub-model", Status: store.RunRunning, PauseState: store.PauseStatePaused, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, "r_legacy_slot", "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	gate := &quotaGateCounts{}
	srv.sem.WithPerUserCap(1).WithUserQuotaStore(gate)
	srv.store = stampReplica{Store: srv.store, replica: "gone-replica"}

	if n, w := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d (warnings %v), want 1", n, w)
	}
	waitWalkRunStatus(t, srv.store, "r_legacy_slot", store.RunCompleted)
	deadline := time.Now().Add(2 * time.Second)
	for {
		admitted, released := gate.counts()
		if admitted == 1 && released == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("per-user counter: %d counted, %d released; want the stopped replica's slot and the run's own given back", admitted, released)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
