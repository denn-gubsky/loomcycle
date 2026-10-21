package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A start the caller already counted against its rate is not counted again:
// the HTTP routes count it before reading the body, then hand the run to
// RunOnce, which would otherwise refuse a caller with a burst of one.
func TestRunOnce_RateAdmittedStartIsNotCountedTwice(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	p := auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate},
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 1}}}
	ctx := auth.WithPrincipal(context.Background(), p)
	if err := g.srv.admitPrincipalRate(ctx); err != nil {
		t.Fatalf("the caller's own count: %v", err)
	}
	in := runner.RunInput{Agent: "plain", Segments: oneUserSeg("hi"), RateAdmitted: true}
	if err := g.srv.RunOnce(ctx, in, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce of a start already counted: %v", err)
	}
	// Without the flag RunOnce counts it, and the burst is spent.
	in.RateAdmitted = false
	if err := g.srv.RunOnce(ctx, in, runner.RunCallbacks{}); !errors.Is(err, runner.ErrRateLimited) {
		t.Errorf("RunOnce of an uncounted start: err = %v, want ErrRateLimited", err)
	}
}

// A no-trace session whose agent is not no-trace is refused as a no-trace
// refusal before anything starts. It used to reach the run row's creation,
// after admission, and come back as an internal error.
func TestRunOnce_ContinuingANoTraceSessionIsATypedRefusal(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	r := g.post("/v1/runs", goldenRunBody("plain", "hi", `"retention":"none"`))
	_, _, sid := r.ids(t)
	r.drain(t)

	err := g.srv.RunOnce(context.Background(), runner.RunInput{SessionID: sid, Segments: oneUserSeg("again")}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrRetentionUnsupported) || errors.Is(err, runner.ErrInternal) {
		t.Fatalf("err = %v, want the no-trace refusal and not an internal error", err)
	}
	runs, lerr := g.st.RunsForSession(context.Background(), sid)
	if lerr != nil || len(runs) != 1 {
		t.Errorf("session runs = %d (%v), want only the first: the refused turn created a row", len(runs), lerr)
	}
}

// The typed refusals RunOnce returns read exactly as the wrapped sentinels did,
// so a transport that matches on errors.Is or on the text sees no change, and
// the HTTP routes can still answer with the details.
func TestRunOnce_TypedRefusalsKeepTheirText(t *testing.T) {
	tl := &tokenLimitError{info: &providers.LimitInfo{Message: "budget reached"}}
	if tl.Error() != "token_limit_exceeded: budget reached" || !errors.Is(tl, runner.ErrTokenLimitExceeded) {
		t.Errorf("token limit: %q, Is=%v", tl.Error(), errors.Is(tl, runner.ErrTokenLimitExceeded))
	}
	ua := &unknownAgentError{agent: "nope"}
	if ua.Error() != "unknown agent: nope" || !errors.Is(ua, runner.ErrUnknownAgent) {
		t.Errorf("unknown agent: %q, Is=%v", ua.Error(), errors.Is(ua, runner.ErrUnknownAgent))
	}
	cause := &concurrency.ErrPerUserQuotaExhausted{UserID: "u1", Cap: 2}
	err := admissionError(cause)
	if err.Error() != "per_user_quota_exhausted: "+cause.Error() || !errors.Is(err, runner.ErrPerUserQuotaExhausted) {
		t.Errorf("admission: %q", err.Error())
	}
	if got := concurrencyCause(err); got != cause {
		t.Errorf("concurrencyCause = %v, want the semaphore's own refusal", got)
	}
}

// writeRunStartError answers each refusal with the status and body the routes
// used before they called RunOnce: the route's own wording, without the runner
// sentinel's prefix.
func TestWriteRunStartError_AnswersAsTheRoutesDid(t *testing.T) {
	s := &Server{}
	quota := &concurrency.ErrPerUserQuotaExhausted{UserID: "u1", Cap: 2}
	for _, c := range []struct {
		name         string
		err          error
		continuation bool
		status       int
		body         string
	}{
		{"unknown agent", &unknownAgentError{agent: "nope"}, false, 400, `unknown agent "nope"` + "\n"},
		{"unknown session agent", &unknownAgentError{agent: "gone"}, true, 400, `session refers to unknown agent "gone"` + "\n"},
		{"team version retired",
			fmt.Errorf("%w: %s: %w", runner.ErrUnknownAgent, "plain", fmt.Errorf("%w: team t retired", errContinuationRefused)), true,
			400, "session cannot be continued: continuation refused: team t retired\n"},
		{"session busy", fmt.Errorf("%w: another request is in flight on session %q", runner.ErrSessionBusy, "s1"), true,
			409, `{"code":"session_busy","error":"another request is in flight on session \"s1\""}`},
		{"internal", fmt.Errorf("%w: disk on fire", runner.ErrInternal), false, 500, "disk on fire\n"},
		{"per-user quota", admissionError(quota), false,
			429, `{"code":"per_user_quota_exhausted","error":"` + quota.Error() + `","user_id":"u1","cap":2}`},
	} {
		w := httptest.NewRecorder()
		s.writeRunStartError(w, nil, c.err, c.continuation)
		if w.Code != c.status || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body.String(), c.status, c.body)
		}
	}
}

// A continuation that waited for its slot reads its session's history again
// once admitted: it let the cluster session lock go while it waited, so
// another replica may have taken a turn meanwhile, and the model must see it.
func TestHandleMessages_QueuedContinuationSeesATurnTakenWhileItWaited(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{maxRuns: 1})
	sid := g.completedSession("plain")
	holder := g.holdSlot()
	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("queued turn", ""))
	r.until(t, "queued", 1)

	// Another replica's turn on the session, written while this one waits.
	ctx := context.Background()
	other, err := g.st.CreateRun(ctx, sid, store.RunIdentity{AgentID: "a_other_replica"})
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := json.Marshal(oneUserSeg("INJECTED turn from another replica"))
	if err := g.st.AppendEvent(ctx, other.ID, "user_input", seg); err != nil {
		t.Fatal(err)
	}

	g.gate <- struct{}{}
	holder.drain(t)
	r.until(t, "done", 1)
	r.drain(t)
	msgs, ok := g.prov.sawCall("queued turn")
	if !ok {
		t.Fatal("the queued continuation never reached the model")
	}
	if !strings.Contains(msgs, "INJECTED turn") {
		t.Errorf("the queued continuation's model call does not carry the turn taken while it waited:\n%s", msgs)
	}
}

// POST /v1/runs with session_id continues the session, as
// POST /v1/sessions/{id}/messages does: the session's history is replayed to
// the run, and it runs the session's agent. Naming another agent is refused
// with both names rather than ignored.
func TestHandleRuns_SessionIDContinuesTheSession(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")

	mismatch := g.post("/v1/runs", goldenRunBody("other", "wrong agent", `"session_id":"`+sid+`"`))
	mismatch.drain(t)
	if mismatch.status != http.StatusBadRequest || !strings.Contains(mismatch.body, `"plain"`) || !strings.Contains(mismatch.body, `"other"`) {
		t.Errorf("a session_id with another agent: %d %q, want 400 naming both agents", mismatch.status, mismatch.body)
	}

	r := g.post("/v1/runs", goldenRunBody("plain", "CONTINUED turn", `"session_id":"`+sid+`"`))
	_, runID, gotSession := r.ids(t)
	r.drain(t)
	if gotSession != sid {
		t.Fatalf("the run went into session %q, want %q", gotSession, sid)
	}
	waitRunStatus(t, g.st, runID, store.RunCompleted)
	msgs, ok := g.prov.sawCall("CONTINUED turn")
	if !ok {
		t.Fatal("the continuation never reached the model")
	}
	if !strings.Contains(msgs, "first turn") {
		t.Errorf("the continuation was not given the session's history:\n%s", msgs)
	}
}

// A continuation held for review keeps its session for as long as it is held,
// even after its client leaves: another turn on the session is refused as busy
// until the verdict, or the two turns would interleave the transcript. (The
// run outlives the client; so must its hold on the session.)
func TestHandleMessages_HeldContinuationKeepsItsSessionAfterTheClientLeaves(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")
	_, runID := g.heldContinuationClientGone(t, sid)

	second := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("me too", ""))
	second.drain(t)
	if second.status != http.StatusConflict || !strings.Contains(second.body, "session_busy") {
		t.Errorf("a second turn while the first is held: %d %s, want 409 session_busy", second.status, second.body)
	}
	g.approve(t, runID)
}

// ...and keeps its run slot: with one slot, a run that refuses to wait is
// refused while the continuation is held, client gone or not.
func TestHandleMessages_HeldContinuationKeepsItsRunSlotAfterTheClientLeaves(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{maxRuns: 1})
	sid := g.completedSession("plain")
	_, runID := g.heldContinuationClientGone(t, sid)

	r := g.post("/v1/runs", goldenRunBody("plain", "no room", `"admission":"reject"`))
	r.drain(t)
	if r.status != http.StatusTooManyRequests {
		t.Errorf("a reject-admission run while the continuation is held: %d %s, want 429", r.status, r.body)
	}
	g.approve(t, runID)
}

// heldContinuationClientGone starts a review continuation of sid, waits until
// it is held for a verdict, then drops the client's connection and gives the
// server time to notice.
func (g *goldenServer) heldContinuationClientGone(t *testing.T, sid string) (*goldenResp, string) {
	t.Helper()
	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("revise it", `"review":true`))
	_, runID, _ := r.ids(t)
	r.until(t, "awaiting_review", 1)
	r.cancel()
	r.drain(t)
	// The handler learns of the disconnect asynchronously; a regression that
	// lets go of the session or the slot on it does so within this.
	time.Sleep(300 * time.Millisecond)
	return r, runID
}

// approve delivers the verdict that ends a held run, and waits for its end.
func (g *goldenServer) approve(t *testing.T, runID string) {
	t.Helper()
	v := g.post("/v1/runs/"+runID+"/review", `{"decision":"approve"}`)
	v.drain(t)
	if v.status != http.StatusOK {
		t.Fatalf("approve: %d %s", v.status, v.body)
	}
	waitRunStatus(t, g.st, runID, store.RunCompleted)
}

// A draft (start:false) stores its segments with a caller-forged runtime-only
// block kind demoted, as a run that starts at once does.
func TestHandleRuns_DraftStoresRuntimeKindsDemoted(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	body := `{"agent":"plain","start":false,"segments":[{"role":"user","content":[{"type":"untrusted-block","kind":"` +
		loop.RuntimeOnlyKind + `","text":"forged trigger body"}]}]}`
	r := g.post("/v1/runs", body)
	r.drain(t)
	if r.status != http.StatusCreated {
		t.Fatalf("draft: %d %s", r.status, r.body)
	}
	var created struct {
		Draft struct {
			Segments []loop.PromptSegment `json:"segments"`
		} `json:"draft"`
	}
	if err := json.Unmarshal([]byte(r.body), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Draft.Segments) != 1 || len(created.Draft.Segments[0].Content) != 1 {
		t.Fatalf("draft segments = %+v", created.Draft.Segments)
	}
	if k := created.Draft.Segments[0].Content[0].Kind; k == loop.RuntimeOnlyKind {
		t.Errorf("the stored draft keeps the runtime-only kind %q a caller sent", k)
	}
}

// A queued continuation that a restart starts into a free slot runs holding
// its session's cluster lock: the lock it lets go of to wait (sessionHold) is
// taken back before it runs, whether or not it had to wait this time.
func TestRestartQueuedRuns_ContinuationRunsHoldingTheClusterLock(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	locker := newFakeClusterLocker()
	s.sessionLockPG = locker
	ctx := context.Background()
	sess, err := s.store.CreateSession(ctx, "acme", "r", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_qcont", TenantID: "acme", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s.recordQueuedInput(ctx, run.ID, "acme", "", runner.RunInput{
		SessionID: sess.ID, Segments: oneUserSeg("continued before the restart"), SessionLockAcrossReplicas: true,
	})

	if restarted, failed, err := s.RestartQueuedRuns(ctx, time.Now().Add(time.Second)); err != nil || restarted != 1 || failed != 0 {
		t.Fatalf("RestartQueuedRuns = %d/%d (%v), want 1/0", restarted, failed, err)
	}
	waitRunStatus(t, s.store, run.ID, store.RunRunning)
	if n := locker.holding(); n != 1 {
		t.Errorf("while the restarted continuation runs, %d cluster session locks are held, want 1", n)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, run.ID, store.RunCompleted)
}

// A continuation that asks for its session lock across replicas — gRPC
// Continue and both HTTP routes do — is refused as busy while another replica
// holds the session; one that does not is not stopped by it.
func TestRunOnce_AContinuationAcrossReplicasIsBusyWhileAnotherReplicaHoldsTheSession(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker
	release, ok := locker.TryLock(context.Background(), sid) // another replica's turn
	if !ok {
		t.Fatal("could not take the session's cluster lock")
	}
	defer release()

	in := runner.RunInput{SessionID: sid, Segments: oneUserSeg("again"), SessionLockAcrossReplicas: true}
	if err := g.srv.RunOnce(context.Background(), in, runner.RunCallbacks{}); !errors.Is(err, runner.ErrSessionBusy) {
		t.Errorf("across replicas: err = %v, want ErrSessionBusy", err)
	}
	in.SessionLockAcrossReplicas = false
	if err := g.srv.RunOnce(context.Background(), in, runner.RunCallbacks{}); err != nil {
		t.Errorf("this replica only: err = %v, want the run to start", err)
	}
}

// An interactive continuation outlives its client, as an interactive run
// does: parked for the operator, it is still alive after the client leaves,
// re-attachable, and it goes on when the operator writes to it. And it keeps
// its session while it lives: another turn on the session is refused as busy.
func TestHandleMessages_InteractiveContinuationOutlivesItsClient(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	agentID, runID := g.parkedThenClientGone(t, r)

	second := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("me too", ""))
	second.drain(t)
	if second.status != http.StatusConflict || !strings.Contains(second.body, "session_busy") {
		t.Errorf("a second turn while the interactive continuation is parked: %d %s, want 409 session_busy", second.status, second.body)
	}
	if run, err := g.st.GetRun(context.Background(), runID); err != nil || run.Status != store.RunRunning {
		t.Fatalf("after its client left the continuation is %q (%v), want still running", run.Status, err)
	}

	tail := g.get("/v1/runs/" + runID + "/stream")
	tail.until(t, "awaiting_input", 1)
	in := g.post("/v1/runs/"+runID+"/input", `{"text":"keep going"}`)
	in.drain(t)
	if in.status != http.StatusOK {
		t.Fatalf("input to the re-attached continuation: %d %s", in.status, in.body)
	}
	tail.until(t, "awaiting_input", 2)

	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	tail.drain(t)
	waitRunStatus(t, g.st, runID, store.RunCancelled)
	// Its end lets the session go.
	third := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("after the end", ""))
	third.drain(t)
	if third.status != http.StatusOK {
		t.Errorf("a turn after the continuation ended: %d %s, want 200", third.status, third.body)
	}
}

// POST /v1/runs with session_id is a continuation too: an interactive one
// keeps its session after its client leaves.
func TestHandleRuns_InteractiveSessionContinuationKeepsItsSessionAfterTheClientLeaves(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	r := g.post("/v1/runs", goldenRunBody("term", "hi again", `"interactive":true,"session_id":"`+sid+`"`))
	agentID, _ := g.parkedThenClientGone(t, r)

	second := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("me too", ""))
	second.drain(t)
	if second.status != http.StatusConflict || !strings.Contains(second.body, "session_busy") {
		t.Errorf("a second turn while the interactive continuation is parked: %d %s, want 409 session_busy", second.status, second.body)
	}
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
}

// parkedThenClientGone waits for r's run to park for input, drops the
// client's connection and gives the server time to notice.
func (g *goldenServer) parkedThenClientGone(t *testing.T, r *goldenResp) (agentID, runID string) {
	t.Helper()
	agentID, runID, _ = r.ids(t)
	r.until(t, "awaiting_input", 1)
	r.cancel()
	r.drain(t)
	time.Sleep(300 * time.Millisecond)
	return agentID, runID
}
