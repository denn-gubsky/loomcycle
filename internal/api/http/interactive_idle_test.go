package http

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A top-level interactive run parked for its operator longer than the idle
// timeout ends: completed, stop reason idle_timeout, its done relayed. What it
// held is let go — with one run slot, the next turn on its session gets it —
// and the session is kept, so a continuation carries on with its history. A
// message sent to the ended run is refused, telling the client to continue the
// session, rather than accepted and lost.
func TestHandleRuns_AParkedInteractiveRunEndsAfterItsIdleTimeout(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{maxRuns: 1})
	g.srv.cfg().Env.InteractiveIdleTimeout = 200 * time.Millisecond

	r := g.post("/v1/runs", goldenRunBody("term", "first turn", `"interactive":true`))
	_, runID, sid := r.ids(t)
	r.until(t, "awaiting_input", 1)
	if done := r.until(t, "done", 1); !strings.Contains(done, loop.StopReasonIdleTimeout) {
		t.Errorf("done frame %s, want stop reason %q", done, loop.StopReasonIdleTimeout)
	}
	r.drain(t)
	run := waitRunStatus(t, g.st, runID, store.RunCompleted)
	if run.StopReason != loop.StopReasonIdleTimeout {
		t.Errorf("run stop reason = %q, want %q", run.StopReason, loop.StopReasonIdleTimeout)
	}

	late := g.post("/v1/runs/"+runID+"/input", `{"text":"are you there?"}`)
	late.drain(t)
	if late.status != http.StatusNotFound || !strings.Contains(late.body, "/messages") {
		t.Errorf("input to the ended run: %d %q, want 404 pointing at a session continuation", late.status, late.body)
	}

	next := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("second turn", ""))
	next.drain(t)
	if next.status != http.StatusOK {
		t.Fatalf("continuing the session after the idle end: %d %s", next.status, next.body)
	}
	msgs, ok := g.prov.sawCall("second turn")
	if !ok || !strings.Contains(msgs, "first turn") {
		t.Errorf("the continuation did not carry the session's history: %q", msgs)
	}
}

// A detached interactive continuation (POST /v1/sessions/{id}/messages,
// interactive) whose client has left ends on the idle timeout too, and lets
// its session go: the next turn on the session is not refused as busy.
func TestHandleMessages_ADetachedInteractiveContinuationEndsAfterItsIdleTimeout(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	g.srv.cfg().Env.InteractiveIdleTimeout = 300 * time.Millisecond

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	_, runID := g.parkedThenClientGone(t, r)
	run := waitRunStatus(t, g.st, runID, store.RunCompleted)
	if run.StopReason != loop.StopReasonIdleTimeout {
		t.Errorf("run stop reason = %q, want %q", run.StopReason, loop.StopReasonIdleTimeout)
	}

	next := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("after the idle end", ""))
	next.drain(t)
	if next.status != http.StatusOK {
		t.Errorf("a turn after the continuation idled out: %d %s, want 200", next.status, next.body)
	}
}

// An idle timeout of 0 keeps a parked run waiting for as long as it lives.
func TestHandleRuns_ZeroIdleTimeoutKeepsAParkedRunWaiting(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	g.srv.cfg().Env.InteractiveIdleTimeout = 0

	r := g.post("/v1/runs", goldenRunBody("term", "first turn", `"interactive":true`))
	agentID, runID, _ := r.ids(t)
	r.until(t, "awaiting_input", 1)
	time.Sleep(400 * time.Millisecond)
	if run, err := g.st.GetRun(context.Background(), runID); err != nil || run.Status != store.RunRunning {
		t.Fatalf("a parked run with no idle timeout is %q (%v), want still running", run.Status, err)
	}
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
	waitRunStatus(t, g.st, runID, store.RunCancelled)
}

// A top-level chat restored after a restart (or a snapshot) waits for its
// operator as a live one does, and ends on the idle timeout the same way.
func TestResume_ARestoredParkedRunEndsAfterItsIdleTimeout(t *testing.T) {
	srv, _, prov, run := parkedRunFixture(t, true)
	srv.cfg().Env.InteractiveIdleTimeout = 200 * time.Millisecond
	if n, warns := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	got := waitRunStatus(t, srv.store, run.ID, store.RunCompleted)
	if got.StopReason != loop.StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q", got.StopReason, loop.StopReasonIdleTimeout)
	}
	if n := len(prov.requests()); n != 0 {
		t.Errorf("the provider was called %d time(s) for a run that never had a turn to answer", n)
	}
}
