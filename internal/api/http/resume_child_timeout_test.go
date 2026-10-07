package http

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A restored child's clock counts its run's time from its start, less every
// review hold that ended; a hold still open stops it.
func TestRestoredClock_DeadlineIsStartPlusBoundPlusEndedHolds(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	clk := &restoredClock{resumedChildClock: resumedChildClock{bound: 10 * time.Second}, start: t0}
	clk.observe([]store.Event{
		{Seq: 1, Type: "user_input", Timestamp: at(0)},
		{Seq: 2, Type: string(providers.EventAwaitingReview), Timestamp: at(2)},
		{Seq: 3, Type: "limit", Timestamp: at(3)}, // written to a held run; does not end the hold
		{Seq: 4, Type: "user_input", Timestamp: at(5)},
		{Seq: 5, Type: string(providers.EventAwaitingReview), Timestamp: at(7)},
	})
	if deadline, held := clk.deadline(clk.start); !held || !deadline.Equal(at(13)) {
		t.Fatalf("deadline = %v held %v; want %v, held (the second hold is open)", deadline, held, at(13))
	}
	clk.observe([]store.Event{{Seq: 6, Type: "done", Timestamp: at(8)}})
	if deadline, held := clk.deadline(clk.start); held || !deadline.Equal(at(14)) || clk.seq != 6 {
		t.Fatalf("deadline = %v held %v seq %d; want %v, not held, seq 6", deadline, held, clk.seq, at(14))
	}
}

// boundedInPoll records the lead spawning one child in poll mode with
// timeout_ms, as the live run records it.
func boundedInPoll(t *testing.T, srv *Server, lead store.Run, id string, timeoutMs int) {
	t.Helper()
	appendResumeEvent(t, srv, lead.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
	})
	appendResumeEvent(t, srv, lead.ID, "tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"op":"spawn","mode":"poll"}`)}})
	appendResumeEvent(t, srv, lead.ID, string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_1", RunID: id, Agent: "worker", Mode: "poll", TimeoutMs: timeoutMs}})
	appendResumeEvent(t, srv, lead.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}})
	appendResumeEvent(t, srv, lead.ID, "tool_result", providers.Event{Type: providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent"}, Text: `{"child_run_id":"` + id + `"}`})
}

// childResultRow is the lead's recorded ending of child, and when it was written.
func childResultRow(t *testing.T, srv *Server, lead store.Run, child string) (providers.SpawnChildEventInfo, time.Time, bool) {
	t.Helper()
	events, err := srv.store.GetTranscript(context.Background(), lead.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		var pe providers.Event
		if e.RunID == lead.ID && e.Type == string(providers.EventSpawnChildResult) && json.Unmarshal(e.Payload, &pe) == nil &&
			pe.SpawnChild != nil && pe.SpawnChild.RunID == child {
			return *pe.SpawnChild, e.Timestamp, true
		}
	}
	return providers.SpawnChildEventInfo{}, time.Time{}, false
}

// parkFor parks run for a runtime pause on another instance's manager, which
// is paused, through the real pause gate — so the run records its pause as a
// live one does. Cleanup ends the park as that instance going away would; the
// gate is released by mgr's resume otherwise. The run is given the empty
// configuration record every run the server starts has, which a run made
// straight in the store lacks.
func parkFor(t *testing.T, srv *Server, mgr *pause.Manager, run store.Run) {
	t.Helper()
	if cur, err := srv.store.GetRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	} else if len(cur.RunConfig) == 0 {
		if ok, err := srv.store.SetRunConfigCAS(context.Background(), run.ID, nil, json.RawMessage(`{}`)); err != nil || !ok {
			t.Fatalf("give run %s a record: %v (written %v)", run.ID, err, ok)
		}
	}
	gone, leave := context.WithCancel(context.Background())
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		_ = (&pauseGate{mgr: mgr, store: srv.store, runID: run.ID}).Park(gone)
	}()
	t.Cleanup(func() {
		leave()
		select {
		case <-parked:
		case <-time.After(10 * time.Second):
			t.Error("the parked gate never returned")
		}
	})
	waitFor(t, "run "+run.ID+" to park", func() bool {
		r, err := srv.store.GetRun(context.Background(), run.ID)
		return err == nil && r.PauseState == store.PauseStatePaused
	})
}

// A bounded poll-mode child of a paused parent times out at the deadline its
// live clock would have reached — its run's start plus timeout_ms plus the
// time it spent held for review plus the time it spent parked for a runtime
// pause on the instance it runs on — not timeout_ms after the parent comes
// back, not while it is parked, and not never. Its run is cancelled as timed
// out, wherever it runs, and the parent wakes to it.
func TestResumePausedRuns_ARestoredChildLeavesOutItsHoldAndItsPause(t *testing.T) {
	const bound, hold, downtime = 1200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond
	ctx := context.Background()
	prov := newBGFamily(answer("done"))
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	cluster := &clusterCancels{sent: map[string]string{}}
	srv.cancelReg.SetClusterCanceller(cluster)
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	child := workerRun(t, srv, lead, "a_remote", "elsewhere", false)
	boundedInPoll(t, srv, lead, child.ID, int(bound/time.Millisecond))
	// The child was held for review for a while before its parent paused.
	appendResumeEvent(t, srv, child.ID, string(providers.EventAwaitingReview), providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{Round: 1}})
	time.Sleep(hold)
	appendResumeEvent(t, srv, child.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "revise"}}},
	})
	waitedFor(t, srv, lead, child.ID)
	markPaused(t, srv, lead)
	started, err := srv.store.GetRun(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The parent's own downtime counts: the child went on running elsewhere.
	time.Sleep(time.Until(started.StartedAt.Add(downtime)))
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want the lead (warnings: %v)", n, warns)
	}

	// Then the instance the child runs on pauses, past the deadline the hold
	// alone would have moved it to.
	elsewhere := pause.NewManager(srv.store, time.Second)
	pausedAt := time.Now()
	if _, err := elsewhere.Pause(ctx, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	parkFor(t, srv, elsewhere, child)
	time.Sleep(time.Until(started.StartedAt.Add(bound + hold + 300*time.Millisecond)))
	if row, _, ok := childResultRow(t, srv, lead, child.ID); ok {
		t.Fatalf("the child was ended %+v while it was parked for the pause", row)
	}
	resumedAt := time.Now()
	if _, err := elsewhere.Resume(ctx); err != nil {
		t.Fatal(err)
	}

	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	row, at, ok := childResultRow(t, srv, lead, child.ID)
	if !ok || row.Ended != "timeout" || row.Status != "timeout" || !strings.Contains(row.Error, "timed out: timeout_ms=1200") {
		t.Fatalf("the child's recorded ending = %+v (found %v), want a timeout", row, ok)
	}
	// The live clock would have run out at start + bound + hold + the pause.
	want := started.StartedAt.Add(bound + hold + resumedAt.Sub(pausedAt))
	if early, late := at.Sub(want), at.Sub(want.Add(700*time.Millisecond)); early < -150*time.Millisecond || late > 0 {
		t.Errorf("the child timed out %v after its run started, want about %v (bound + its review hold + its pause)",
			at.Sub(started.StartedAt), want.Sub(started.StartedAt))
	}
	if reason, ok := cluster.reason("a_remote"); !ok || !strings.Contains(reason, "timed out: timeout_ms=1200") {
		t.Errorf("the child's run was cancelled with %q (sent %v), want the timeout", reason, ok)
	}
	if calls := prov.leadCalls(); len(calls) != 1 || !strings.Contains(lastText(calls[0]), child.ID+" (worker): timeout") {
		t.Errorf("the lead was woken by %d calls, want one naming the timeout", len(calls))
	}
}

// A bounded child restored with its paused parent — snapshot and restore, or
// a restart — does not count the time it was paused: from when the runtime
// paused on the instance it parked on, through the downtime, to its
// re-dispatch here. The bound from its start passed in the downtime; it is
// still given the time it had left when the runtime paused.
func TestResumePausedRuns_ARestoredChildDoesNotCountTheDowntimeItWasPausedFor(t *testing.T) {
	const bound = 800 * time.Millisecond
	ctx := context.Background()
	prov := newBGFamily(answer("done"))
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	child := workerRun(t, srv, lead, "a_child", "hold", false)
	boundedInPoll(t, srv, lead, child.ID, int(bound/time.Millisecond))
	waitedFor(t, srv, lead, child.ID)
	markPaused(t, srv, lead)
	started, err := srv.store.GetRun(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The instance both ran on pauses, the child parks for it, and the
	// instance is gone before it resumes.
	before := pause.NewManager(srv.store, time.Second)
	pausedAt := time.Now()
	if _, err := before.Pause(ctx, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	parkFor(t, srv, before, child)
	time.Sleep(time.Until(started.StartedAt.Add(bound + 400*time.Millisecond)))

	redispatched := time.Now()
	if n, warns := srv.ResumePausedRuns(ctx); n != 2 {
		t.Fatalf("resumed %d, want the child and the lead (warnings: %v)", n, warns)
	}
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	row, at, ok := childResultRow(t, srv, lead, child.ID)
	if !ok || row.Ended != "timeout" || !strings.Contains(row.Error, "timed out: timeout_ms=800") {
		t.Fatalf("the child's recorded ending = %+v (found %v), want a timeout", row, ok)
	}
	want := started.StartedAt.Add(bound + redispatched.Sub(pausedAt))
	if early, late := at.Sub(want), at.Sub(want.Add(time.Second)); early < -150*time.Millisecond || late > 0 {
		t.Errorf("the child timed out %v after it was re-dispatched, want about %v (what it had left when the runtime paused)",
			at.Sub(redispatched), want.Sub(redispatched))
	}
}

// A bounded child whose deadline passed while its parent was paused reads
// timeout as soon as the parent comes back.
func TestResumePausedRuns_ARestoredChildPastItsDeadlineTimesOutAtOnce(t *testing.T) {
	ctx := context.Background()
	prov := newBGFamily(agentCall("tu_2", `{"op":"poll","wait":"all","wait_ms":5000}`), answer("done"))
	srv, _ := makeServer(t, prov, bgResumeConfig(""))
	srv.childRecheckFirst = time.Hour // only the re-armed clock may end it
	settle(t, srv, prov)
	lead := pausedLead(t, srv)
	child := workerRun(t, srv, lead, "a_remote", "elsewhere", false)
	boundedInPoll(t, srv, lead, child.ID, 50)
	time.Sleep(200 * time.Millisecond)
	markPaused(t, srv, lead) // paused mid-turn, not waiting

	resumed := time.Now()
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want the lead (warnings: %v)", n, warns)
	}
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	row, at, ok := childResultRow(t, srv, lead, child.ID)
	if !ok || row.Ended != "timeout" {
		t.Fatalf("the child's recorded ending = %+v (found %v), want a timeout", row, ok)
	}
	if d := at.Sub(resumed); d > 2*time.Second {
		t.Errorf("the child timed out %v after the resume, want at once", d)
	}
	if poll := lastToolText(prov.leadCalls()[1]); !strings.Contains(poll, `"state":"timeout"`) {
		t.Errorf("poll = %s, want the child timed out", poll)
	}
}

// failingEventsStore reads a bounded child's run as running since a minute ago
// — past its deadline — and fails every read of its events.
type failingEventsStore struct {
	store.Store
	events atomic.Int64
}

func (s *failingEventsStore) GetRun(_ context.Context, id string) (store.Run, error) {
	return store.Run{ID: id, Status: store.RunRunning, StartedAt: time.Now().Add(-time.Minute)}, nil
}

func (s *failingEventsStore) GetRunEventsSince(context.Context, string, int64, int) ([]store.Event, error) {
	s.events.Add(1)
	return nil, errors.New("db down")
}

// A bounded restored child past its deadline whose events cannot be read is
// read again after a backoff, not at once and again without end for as long
// as the store fails.
func TestWatchRestoredChildren_AnUnreadableChildPastItsDeadlineIsRetriedWithBackoff(t *testing.T) {
	st := &failingEventsStore{}
	srv := &Server{store: st, childRecheckFirst: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	srv.watchRestoredChildren(ctx, nil, "alice", []tools.ChildSpec{{RunID: "c1", Agent: "worker", Index: -1}},
		map[string]int{"c1": 50}, []func(){func() {}})
	// One read at once, one after the first backoff (a second) — not the tens
	// of thousands a re-arm at zero makes.
	if n := st.events.Load(); n < 2 || n > 3 {
		t.Errorf("the child's events were read %d times in 1.5s, want 2 or 3", n)
	}
}
