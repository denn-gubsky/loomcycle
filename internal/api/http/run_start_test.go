package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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
	// Finished before this one is admitted: a turn still running there would
	// make the session busy, not part of its history.
	if err := g.st.FinishRun(ctx, other.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
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

// A parked continuation holds no database connection: the cluster half of its
// session lock (a Postgres advisory lock, which pins a pool connection) is let
// go while it waits for its operator and taken back for its next turn. A
// parked run holds no run slot either, so a lock kept through the park would
// let a user pin one connection per parked session, outside every cap.
func TestHandleMessages_AParkedContinuationHoldsNoClusterLock(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	agentID, runID, _ := r.ids(t)
	r.until(t, "awaiting_input", 1)
	waitFor(t, "the cluster lock to be let go at the park", func() bool { return locker.holding() == 0 })

	// The next turn takes it back, for as long as it runs.
	g.post("/v1/runs/"+runID+"/input", `{"text":"HOLD this turn"}`).drain(t)
	r.until(t, "text", 2)
	if n := locker.holding(); n != 1 {
		t.Errorf("during the turn after the park, %d cluster locks are held, want 1", n)
	}
	g.gate <- struct{}{}
	r.until(t, "awaiting_input", 2)
	waitFor(t, "the cluster lock to be let go at the second park", func() bool { return locker.holding() == 0 })

	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// One live turn per session holds across replicas without a lock held: with
// the continuation parked (its cluster lock let go), another replica can take
// the lock, and must still refuse a second turn on the session.
func TestHandleMessages_AParkedContinuationsSessionIsBusyOnAnotherReplica(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	agentID, _, _ := r.ids(t)
	r.until(t, "awaiting_input", 1)
	waitFor(t, "the cluster lock to be let go at the park", func() bool { return locker.holding() == 0 })
	if release, ok := locker.TryLock(context.Background(), sid); !ok {
		t.Fatal("another replica could not take the parked session's cluster lock: it is still held")
	} else {
		release()
	}

	other := g.otherReplica(locker)
	second := other.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("me too", ""))
	second.drain(t)
	if second.status != http.StatusConflict || !strings.Contains(second.body, "session_busy") {
		t.Errorf("a second turn on another replica while the continuation is parked: %d %s, want 409 session_busy", second.status, second.body)
	}

	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// A parked continuation that cannot take its session's cluster lock back —
// another replica holds it — does not run its turn unguarded: the turn ends
// with the busy reason and the run parks again for its operator.
func TestHandleMessages_AParkedContinuationThatCannotRetakeItsLockEndsTheTurn(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	agentID, runID, _ := r.ids(t)
	r.until(t, "awaiting_input", 1)
	waitFor(t, "the cluster lock to be let go at the park", func() bool { return locker.holding() == 0 })
	release, ok := locker.TryLock(context.Background(), sid) // another replica, mid-request
	if !ok {
		t.Fatal("could not take the parked session's cluster lock")
	}

	g.post("/v1/runs/"+runID+"/input", `{"text":"keep going"}`).drain(t)
	cancelled := r.until(t, "turn_cancelled", 1)
	if !strings.Contains(cancelled, "another request is in flight") {
		t.Errorf("turn_cancelled = %s, want the busy reason", cancelled)
	}
	r.until(t, "awaiting_input", 2)
	release()

	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// otherReplica is a second server on the same store and the same cluster
// locker: another replica of this one.
func (g *goldenServer) otherReplica(locker clusterLocker) *goldenServer {
	g.t.Helper()
	srv := New(g.srv.cfg(), &stubResolver{p: g.prov}, []tools.Tool{}, concurrency.New(4, 8, 0), g.st)
	srv.sessionLockPG = locker
	ts := httptest.NewServer(srv.Mux())
	g.t.Cleanup(ts.Close)
	return &goldenServer{t: g.t, srv: srv, ts: ts, st: g.st, gate: g.gate, prov: g.prov, bearer: g.bearer}
}

// A continuation held for review is parked too: it holds no cluster lock
// while it waits for the verdict.
func TestHandleMessages_AHeldContinuationHoldsNoClusterLockWhileHeld(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("revise it", `"review":true`))
	_, runID, _ := r.ids(t)
	r.until(t, "awaiting_review", 1)
	// Not fatal: the held run keeps its request — and the test server — open
	// until it gets a verdict, so the verdict must be given whatever this finds.
	if !eventually(func() bool { return locker.holding() == 0 }) {
		t.Errorf("while the continuation is held for review, %d cluster locks are held, want 0", locker.holding())
	}
	g.approve(t, runID)
	r.drain(t)
}

// eventually polls cond for up to five seconds.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// A continuation that waited for its slot is refused as busy if, while it
// waited, another replica started a turn on the session that is still live:
// that turn is parked or running there with its cluster lock let go, and the
// session takes one live turn at a time.
func TestHandleMessages_QueuedContinuationIsBusyIfAnotherReplicasTurnIsLive(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{maxRuns: 1})
	sid := g.completedSession("plain")
	g.srv.sessionLockPG = newFakeClusterLocker() // replicas: a cluster
	holder := g.holdSlot()
	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("queued turn", ""))
	r.until(t, "queued", 1)

	// Another replica's turn on the session, live while this one waits.
	if _, err := g.st.CreateRun(context.Background(), sid, store.RunIdentity{AgentID: "a_other_replica_live"}); err != nil {
		t.Fatal(err)
	}
	g.gate <- struct{}{}
	holder.drain(t)
	frame := r.until(t, "error", 1)
	r.drain(t)
	if !strings.Contains(frame, "another request is in flight") {
		t.Errorf("the queued continuation's end = %s, want the busy reason", frame)
	}
}

// session_id on POST /v1/runs tells a caller nothing about a session it may
// not use: an isolated member probing another user's session gets the same
// answer as for a session that does not exist, and a token limited to some
// agents is never told the name of an agent outside its list.
func TestHandleRuns_SessionIDRevealsNothingTheCallerMayNotSee(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{authed: true})
	r := g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("other", "first turn", ""))
	_, _, sid := r.ids(t)
	r.drain(t)

	// An isolated member: another user's session reads as a missing one.
	probe := func(id string) *goldenResp {
		resp := g.postAs(goldenIsolatedBearer, "/v1/runs", goldenRunBody("plain", "hi", `"session_id":"`+id+`"`))
		resp.drain(t)
		return resp
	}
	seen, missing := probe(sid), probe("s_0123456789abcdef")
	if seen.status != http.StatusNotFound || seen.status != missing.status ||
		strings.ReplaceAll(seen.body, sid, "X") != strings.ReplaceAll(missing.body, "s_0123456789abcdef", "X") {
		t.Errorf("isolated member: another user's session = %d %q, a missing one = %d %q; want the same 404",
			seen.status, seen.body, missing.status, missing.body)
	}
	if strings.Contains(seen.body, "other") {
		t.Errorf("isolated member learned the session's agent: %q", seen.body)
	}

	// A token limited to "plain", on a session of "other": refused without
	// the name.
	limited := g.postAs(goldenRestrictedBearer, "/v1/runs", goldenRunBody("plain", "hi", `"session_id":"`+sid+`"`))
	limited.drain(t)
	if limited.status < 400 || strings.Contains(limited.body, `"other"`) {
		t.Errorf("restricted token: %d %q, want a refusal that does not name agent \"other\"", limited.status, limited.body)
	}
}

// An approved run held for review is not re-locked: approving starts no turn.
// An interactive run approved while another replica briefly holds its
// session's lock parks for its operator's next message, as it would without
// the contention; it used to be ended.
func TestHandleMessages_AnApprovedHoldTakesNoLockAndParks(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true,"review":true`))
	agentID, runID, _ := r.ids(t)
	r.until(t, "awaiting_review", 1)
	if !eventually(func() bool { return locker.holding() == 0 }) {
		t.Errorf("held for review with %d cluster locks", locker.holding())
	}
	release, ok := locker.TryLock(context.Background(), sid) // another replica, mid-request
	if !ok {
		t.Fatal("could not take the held session's cluster lock")
	}
	g.post("/v1/runs/"+runID+"/review", `{"decision":"approve"}`).drain(t)
	r.until(t, "awaiting_input", 1)
	if run, err := g.st.GetRun(context.Background(), runID); err != nil || run.Status != store.RunRunning {
		t.Errorf("after the approval the run is %q (%v), want running, parked for input", run.Status, err)
	}
	release()
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// A held run sent back with feedback takes its lock back for the revision.
// When another replica holds it throughout, the run ends cancelled with the
// busy reason recorded — not as a client that went away.
func TestHandleMessages_ASendBackThatCannotRetakeItsLockRecordsTheBusyReason(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("revise it", `"review":true`))
	_, runID, _ := r.ids(t)
	r.until(t, "awaiting_review", 1)
	if !eventually(func() bool { return locker.holding() == 0 }) {
		t.Errorf("held for review with %d cluster locks", locker.holding())
	}
	release, ok := locker.TryLock(context.Background(), sid)
	if !ok {
		t.Fatal("could not take the held session's cluster lock")
	}
	defer release()
	v := g.post("/v1/runs/"+runID+"/review", `{"decision":"reject","feedback":"shorter please"}`)
	v.drain(t)
	r.drain(t)
	run := waitRunStatus(t, g.st, runID, store.RunCancelled)
	if !strings.Contains(run.StopReason+" "+run.ErrorMsg, "another request is in flight") {
		t.Errorf("run ended stop_reason=%q error=%q, want the busy reason", run.StopReason, run.ErrorMsg)
	}
}

// lockProbeProvider records, at each model call, how many cluster session
// locks are held. Its "HOLD" answer waits for gate; a stateful agent's step
// ends its turn with a state patch.
type lockProbeProvider struct {
	locker *fakeClusterLocker
	gate   chan struct{}

	mu   sync.Mutex
	held []int
	n    int
}

func (p *lockProbeProvider) ID() string                  { return "stub" }
func (p *lockProbeProvider) Probe(context.Context) error { return nil }
func (p *lockProbeProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *lockProbeProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *lockProbeProvider) heldAtCalls() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.held...)
}

func (p *lockProbeProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.held = append(p.held, p.locker.holding())
	p.n++
	n := p.n
	p.mu.Unlock()
	var b []byte
	if n := len(req.Messages); n > 0 {
		b, _ = json.Marshal(req.Messages[n-1]) // the newest message decides
	}
	ch := make(chan providers.Event, 4)
	done := providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	switch {
	case len(req.Tools) > 0 && req.Tools[0].Name == "emit_state":
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: fmt.Sprintf("t%d", n), Name: "emit_state",
			Input: json.RawMessage(fmt.Sprintf(`{"patch":{"turn%d":"seen"},"done":true,"final":"answer %d"}`, n, n))}}
		done.StopReason = "tool_use"
		ch <- done
	case strings.Contains(string(b), "HOLD"):
		go func() {
			defer close(ch)
			ch <- providers.Event{Type: providers.EventText, Text: "held"}
			select {
			case <-p.gate:
				ch <- done
			case <-ctx.Done():
			}
		}()
		return ch, nil
	default:
		ch <- providers.Event{Type: providers.EventText, Text: "hello"}
		ch <- done
	}
	close(ch)
	return ch, nil
}

// lockProbeServer is a server whose model records the cluster locks held at
// each of its calls.
func lockProbeServer(t *testing.T) (*goldenServer, *lockProbeProvider) {
	t.Helper()
	mode := config.ContextModeStateful
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"plain":    {Model: "stub-model", SystemPrompt: "be brief"},
			"stateful": {Model: "stub-model", SystemPrompt: "be brief", Context: &config.Context{Mode: &mode}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 8},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "lockprobe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	locker := newFakeClusterLocker()
	prov := &lockProbeProvider{locker: locker, gate: make(chan struct{}, 8)}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 8, 0), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	srv.sessionLockPG = locker
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &goldenServer{t: t, srv: srv, ts: ts, st: st, gate: prov.gate}, prov
}

// A stateful chat has no per-turn cancel hook; its turn after a park still
// runs holding its session's cluster lock.
func TestHandleMessages_AStatefulContinuationRetakesItsLockAfterAPark(t *testing.T) {
	g, prov := lockProbeServer(t)
	sid := g.completedSession("stateful")
	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	agentID, runID, _ := r.ids(t)
	r.until(t, "awaiting_input", 1)
	g.post("/v1/runs/"+runID+"/input", `{"text":"next"}`).drain(t)
	r.until(t, "awaiting_input", 2)
	held := prov.heldAtCalls()
	if len(held) < 3 || held[len(held)-1] != 1 {
		t.Errorf("cluster locks held at each model call = %v, want 1 at the turn after the park", held)
	}
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// A run made interactive while it ran parks, and its turn after the park runs
// holding its session's cluster lock, though it started with no per-turn
// cancel to re-take it on.
func TestHandleMessages_AContinuationPromotedToInteractiveRetakesItsLockAfterAPark(t *testing.T) {
	g, prov := lockProbeServer(t)
	sid := g.completedSession("plain")
	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("HOLD while promoted", ""))
	agentID, runID, _ := r.ids(t)
	r.until(t, "text", 1)
	if code, b := postRetune(t, g.ts, runID, `{"interactive":true}`); code != http.StatusOK {
		t.Fatalf("retune: %d %s", code, b)
	}
	g.gate <- struct{}{}
	r.until(t, "awaiting_input", 1)
	g.post("/v1/runs/"+runID+"/input", `{"text":"next"}`).drain(t)
	r.until(t, "awaiting_input", 2)
	held := prov.heldAtCalls()
	if len(held) < 3 || held[len(held)-1] != 1 {
		t.Errorf("cluster locks held at each model call = %v, want 1 at the turn after the park", held)
	}
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// A re-take that meets the lock held briefly — another replica refusing a
// request on the session, which holds it for one query — waits it out: the
// operator's turn runs, and is not cancelled as busy.
func TestHandleMessages_AParkedContinuationWaitsOutABriefHoldOnItsLock(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	locker := newFakeClusterLocker()
	g.srv.sessionLockPG = locker

	r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("hi again", `"interactive":true`))
	agentID, runID, _ := r.ids(t)
	r.until(t, "awaiting_input", 1)
	if !eventually(func() bool { return locker.holding() == 0 }) {
		t.Fatal("the cluster lock was not let go at the park")
	}
	release, ok := locker.TryLock(context.Background(), sid)
	if !ok {
		t.Fatal("could not take the parked session's cluster lock")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	g.post("/v1/runs/"+runID+"/input", `{"text":"keep going"}`).drain(t)
	r.until(t, "awaiting_input", 2)
	for _, ev := range r.events() {
		if ev == "turn_cancelled" {
			t.Errorf("the turn was cancelled as busy over a lock held for 300ms; frames: %v", r.events())
		}
	}
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
}

// A run row orphaned by a crash (killed mid-run: status running, heartbeat
// stopped) does not block its session. Across replicas a running row is a
// live turn only while its heartbeat is fresh, by the sweeper's own rule; on a
// single replica there is no row check at all — the in-process lock is the
// rule there, and a crash takes it with it.
func TestHandleMessages_AnOrphanedRunRowDoesNotBlockItsSession(t *testing.T) {
	for _, cluster := range []bool{true, false} {
		name := "single replica"
		if cluster {
			name = "across replicas"
		}
		t.Run(name, func(t *testing.T) {
			g := newGoldenServer(t, goldenOpts{})
			sid := g.completedSession("plain")
			if cluster {
				g.srv.sessionLockPG = newFakeClusterLocker()
			}
			cfg := *g.srv.cfg()
			cfg.Env.HeartbeatStaleAfter = 50 * time.Millisecond
			g.srv.cfgHolder.Store(&cfg)
			// The crashed process's run: running, never heartbeated again.
			if _, err := g.st.CreateRun(context.Background(), sid, store.RunIdentity{AgentID: "a_crashed"}); err != nil {
				t.Fatal(err)
			}
			// Across replicas, past the staleness window; on one replica even
			// a fresh orphan must not block.
			if cluster {
				time.Sleep(100 * time.Millisecond)
			}
			r := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("after the crash", ""))
			r.drain(t)
			if r.status != http.StatusOK {
				t.Errorf("a continuation of a session with an orphaned run row: %d %s, want 200", r.status, r.body)
			}
		})
	}
}

// A message right after the cancel of the session's live run continues the
// session: the bridges end the parked run and continue at once, and the
// cancelled run lets its session go only once it has finished, a moment later.
// The continuation waits that out — only while the session's live run is one
// whose cancel is in flight — instead of answering 409.
func TestHandleMessages_AContinuationAfterACancelWaitsForTheCancelledRun(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		name := "single replica"
		if cluster {
			name = "across replicas"
		}
		t.Run(name, func(t *testing.T) {
			g := newGoldenServer(t, goldenOpts{})
			g.prov.cancelDelay = 500 * time.Millisecond
			sid := g.completedSession("term")
			if cluster {
				g.srv.sessionLockPG = newFakeClusterLocker()
			}
			live := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("HOLD this turn", `"interactive":true`))
			agentID, _, _ := live.ids(t)
			live.until(t, "text", 1)

			g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
			next := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("next message", `"interactive":true`))
			if next.status != http.StatusOK {
				next.drain(t)
				t.Fatalf("the continuation right after the cancel: %d %s, want the stream", next.status, next.body)
			}
			nextAgent, _, _ := next.ids(t)
			next.until(t, "awaiting_input", 1)
			g.post("/v1/agents/"+nextAgent+"/cancel", "").drain(t)
			next.drain(t)
			live.drain(t)
		})
	}
}

// ...but a session whose live run is not being cancelled is busy at once.
func TestHandleMessages_AContinuationOfALiveSessionIsStillRefusedAtOnce(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("term")
	live := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("HOLD this turn", `"interactive":true`))
	agentID, _, _ := live.ids(t)
	live.until(t, "text", 1)
	start := time.Now()
	second := g.post("/v1/sessions/"+sid+"/messages", goldenMsgBody("me too", ""))
	second.drain(t)
	if second.status != http.StatusConflict || time.Since(start) > time.Second {
		t.Errorf("a continuation of a session whose run is live: %d after %s, want 409 at once", second.status, time.Since(start))
	}
	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	live.drain(t)
}
