package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run waiting on a person — held for review, or parked for its next
// message — is already at a clean boundary: nothing is in flight. A runtime
// pause must count it as paused and record it so, or it is exactly the run
// that a snapshot or a restart loses. It used to block in its park, never
// reach the loop's pause gate, hold the pause until its timeout and stay
// pause_state 'running'.
func TestPause_CountsAHeldRunAsPaused(t *testing.T) {
	h := newReviewHarness(t)
	mgr := pause.NewManager(h.st, time.Second)
	h.srv.SetPauseManager(mgr)

	runID, _, frames, stop := h.start(reviewRunBody)
	defer stop()
	h.waitFrame(frames, "awaiting_review")
	h.waitHeld(runID, 1)

	res, err := mgr.Pause(context.Background(), 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.PausedRunsCount != 1 || len(res.Warnings) != 0 {
		t.Fatalf("pause = %+v, want the held run counted paused with no warning", res)
	}
	if run, _ := h.st.GetRun(context.Background(), runID); run.PauseState != store.PauseStatePaused {
		t.Errorf("row pause_state = %q, want paused", run.PauseState)
	}

	// Resumed in place: the run is running again, still held, and takes a verdict.
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if run, _ := h.st.GetRun(context.Background(), runID); run.PauseState == store.PauseStateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("verdict after resume = %d %s", code, body)
	}
	h.waitStatus(runID, store.RunCompleted)
}

// The same for an interactive run parked for its next message.
func TestPause_CountsAParkedInteractiveRunAsPaused(t *testing.T) {
	h := newReviewHarness(t)
	mgr := pause.NewManager(h.st, time.Second)
	h.srv.SetPauseManager(mgr)

	runID, _, frames, stop := h.start(`{"agent":"writer","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)
	defer stop()
	h.waitFrame(frames, "awaiting_input")

	res, err := mgr.Pause(context.Background(), 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.PausedRunsCount != 1 || len(res.Warnings) != 0 {
		t.Fatalf("pause = %+v, want the parked run counted paused with no warning", res)
	}
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Still parked and steerable after the resume.
	resp, err := http.Post(h.ts.URL+"/v1/runs/"+runID+"/input", "application/json", strings.NewReader(`{"text":"go on"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("steer after resume = %d", resp.StatusCode)
	}
	h.waitFrame(frames, "awaiting_input")
}

// heldRunFixture is a paused run that was HELD for review when it paused: its
// conversation ends on the answer under review, followed by the hold. The
// shape a pause records and a restart has to bring back.
func heldRunFixture(t *testing.T, interactive bool) (*Server, *httptest.Server, *recordingScriptedProvider, store.Run) {
	t.Helper()
	return heldRunFixtureWith(t, interactive, runConfigRecord{Review: reviewRecord(true)})
}

func heldRunFixtureWith(t *testing.T, interactive bool, rc runConfigRecord) (*Server, *httptest.Server, *recordingScriptedProvider, store.Run) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"writer": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you write"},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "held.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_held", UserID: "alice", Model: "stub-model", Interactive: interactive,
		RunConfig: rc.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "write the plan"}}},
	})
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "the held plan"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{
		Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{},
	})
	appendResumeEvent(t, srv, run.ID, "awaiting_review", providers.Event{
		Type: providers.EventAwaitingReview, AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: 0, Round: 1},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	return srv, ts, prov, run
}

func postReview(t *testing.T, ts *httptest.Server, runID, body string) int {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs/"+runID+"/review", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A run held for review when it paused comes back HELD — non-interactive
// included, because it is waiting for a verdict a person owes it. It used to be
// refused as "idle awaiting input" and marked failed, losing the answer under
// review. Approving it after the resume completes it on that answer, with no
// model call.
func TestResume_AHeldRunIsHeldAgain(t *testing.T) {
	srv, ts, prov, run := heldRunFixture(t, false)
	ctx := context.Background()
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v) — the held run was refused", n, warns)
	}
	waitFor(t, "the resumed run to be held again", func() bool {
		return strings.Count(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_review") >= 2
	})
	if got := prov.requests(); len(got) != 0 {
		t.Fatalf("the restored hold called the provider %d time(s)", len(got))
	}
	if code := postReview(t, ts, run.ID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve after resume = %d", code)
	}
	waitFor(t, "the approved run to complete", func() bool {
		r, _ := srv.store.GetRun(ctx, run.ID)
		return r.Status == store.RunCompleted
	})
	if r, _ := srv.store.GetRun(ctx, run.ID); !strings.Contains(string(r.Result), "the held plan") {
		t.Errorf("result = %s, want the answer that was under review", r.Result)
	}
	if len(prov.requests()) != 0 {
		t.Error("approving a restored hold called the provider")
	}
}

// Feedback on a restored hold reaches the model with the reviewed answer still
// in the conversation.
func TestResume_FeedbackOnARestoredHoldRevises(t *testing.T) {
	srv, ts, prov, run := heldRunFixture(t, true)
	if n, warns := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("resumed %d (warnings: %v)", n, warns)
	}
	waitFor(t, "the resumed run to be held again", func() bool {
		return strings.Count(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_review") >= 2
	})
	if code := postReview(t, ts, run.ID, `{"decision":"reject","feedback":"cover the rollback"}`); code != http.StatusOK {
		t.Fatalf("reject = %d", code)
	}
	waitFor(t, "the revision to reach the model", func() bool { return len(prov.requests()) > 0 })
	req := prov.requests()[0]
	if last := req.Messages[len(req.Messages)-1]; !strings.Contains(contentText(last), "cover the rollback") {
		t.Errorf("last message = %+v, want the feedback", last)
	}
	if !strings.Contains(messagesText(req.Messages), "the held plan") {
		t.Error("the revision lost the answer it was revising")
	}
}

// Through the real server: a run started with a review deadline that nobody
// rules on ends rejected, with the reason on the row.
func TestReview_AnUnreviewedHoldExpiresRejected(t *testing.T) {
	h := newReviewHarness(t)
	runID, _, frames, stop := h.start(`{"agent":"writer","review":true,"review_ttl_seconds":1,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
	defer stop()
	h.waitFrame(frames, "awaiting_review")
	deadline := time.Now().Add(4 * time.Second)
	var run store.Run
	for time.Now().Before(deadline) {
		run, _ = h.st.GetRun(context.Background(), runID)
		if run.Status != store.RunRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != store.RunRejected || run.StopReason != "review_expired" {
		t.Errorf("row = %s / %q, want rejected / review_expired", run.Status, run.StopReason)
	}
	if rec, _ := decodeRunConfig(run.RunConfig); rec.ReviewTTLSeconds != 1 {
		t.Errorf("run_config review_ttl_seconds = %d, want the deadline kept for resume", rec.ReviewTTLSeconds)
	}
}

// A restored hold keeps the deadline it had: its expiry runs from when the hold
// began, not from the restart.
func TestResume_ARestoredHoldKeepsItsDeadline(t *testing.T) {
	srv, _, _, run := heldRunFixtureWith(t, false, runConfigRecord{Review: reviewRecord(true), ReviewTTLSeconds: 3600})
	events, err := srv.store.GetTranscript(context.Background(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var heldAt time.Time
	for _, e := range events {
		if e.Type == "awaiting_review" {
			heldAt = e.Timestamp
		}
	}
	time.Sleep(1100 * time.Millisecond) // a restart some time later
	if n, warns := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("resumed %d (warnings: %v)", n, warns)
	}
	want := heldAt.Add(time.Hour).UTC().Format(time.RFC3339)
	waitFor(t, "the restored hold to announce its deadline", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), `"expires_at":"`+want+`"`)
	})
}
