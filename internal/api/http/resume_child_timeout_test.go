package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// A bounded poll-mode child of a paused parent times out at the deadline its
// live clock would have reached — its run's start plus timeout_ms plus the
// time it spent held for review — not timeout_ms after the parent comes back,
// and not never. The pause counts: it does not move the deadline. Its run is
// cancelled as timed out, wherever it runs, and the parent wakes to it.
func TestResumePausedRuns_ARestoredChildTimesOutAtItsOriginalDeadline(t *testing.T) {
	const bound, hold, downtime = 2000 * time.Millisecond, 400 * time.Millisecond, 1400 * time.Millisecond
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
	time.Sleep(time.Until(started.StartedAt.Add(downtime)))

	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want the lead (warnings: %v)", n, warns)
	}
	waitWalkRunStatus(t, srv.store, lead.ID, store.RunCompleted)
	row, at, ok := childResultRow(t, srv, lead, child.ID)
	if !ok || row.Ended != "timeout" || row.Status != "timeout" || !strings.Contains(row.Error, "timed out: timeout_ms=2000") {
		t.Fatalf("the child's recorded ending = %+v (found %v), want a timeout", row, ok)
	}
	// The live clock would have run out at start + bound + hold.
	want := started.StartedAt.Add(bound + hold)
	if early, late := at.Sub(want), at.Sub(want.Add(500*time.Millisecond)); early < -100*time.Millisecond || late > 0 {
		t.Errorf("the child timed out %v after its run started, want about %v (bound + its review hold)", at.Sub(started.StartedAt), bound+hold)
	}
	if reason, ok := cluster.reason("a_remote"); !ok || !strings.Contains(reason, "timed out: timeout_ms=2000") {
		t.Errorf("the child's run was cancelled with %q (sent %v), want the timeout", reason, ok)
	}
	if calls := prov.leadCalls(); len(calls) != 1 || !strings.Contains(lastText(calls[0]), child.ID+" (worker): timeout") {
		t.Errorf("the lead was woken by %d calls, want one naming the timeout", len(calls))
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
