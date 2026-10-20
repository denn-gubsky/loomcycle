package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
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

// POST /v1/runs with session_id puts a FRESH run in the session: nothing of the
// session is replayed to it, and it runs the agent the request names, not the
// session's. (A continuation is POST /v1/sessions/{id}/messages.)
func TestHandleRuns_SessionIDAppendsAFreshRunWithoutReplay(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")
	r := g.post("/v1/runs", goldenRunBody("other", "APPENDED turn", `"session_id":"`+sid+`"`))
	_, runID, gotSession := r.ids(t)
	r.drain(t)
	if gotSession != sid {
		t.Fatalf("the run went into session %q, want %q", gotSession, sid)
	}
	waitRunStatus(t, g.st, runID, store.RunCompleted)
	msgs, ok := g.prov.sawCall("APPENDED turn")
	if !ok {
		t.Fatal("the appended run never reached the model")
	}
	if strings.Contains(msgs, "first turn") {
		t.Errorf("the appended run was given the session's history:\n%s", msgs)
	}
	ev, err := g.st.GetRunEventsSince(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ev {
		if e.Type == "system_prompt" && !strings.Contains(string(e.Payload), `"other"`) {
			t.Errorf("the appended run ran with system prompt %s, want agent other's", e.Payload)
		}
	}
}
