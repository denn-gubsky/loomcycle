package http

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY: a run that was still QUEUED when loomcycle stopped is started again
// at the next boot (RestartQueuedRuns) with no caller on its ctx. It used to
// start on its agent's full grants, and without asking `run agent:` for its
// caller. It now starts under its caller's CURRENT set, recorded when it
// queued, and a deny bound while it waited refuses it.

// queueAsDev leaves a queued row of agent as dev's live intake does when the
// process stops mid-wait: the row, and its stored request recorded under
// dev's principal and the http transport.
func (e *aclResumeEnv) queueAsDev(t *testing.T, agent, agentID string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := e.st.CreateSession(ctx, "acme", agent, "dev")
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.st.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, TenantID: "acme", UserID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := e.srv.ACLResolver().Resolve(ctx, auth.Principal{TenantID: "acme", Subject: "dev", TokenDefID: "def_worker", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	if err != nil || dev.Effective == nil {
		t.Fatalf("resolve dev: %v", err)
	}
	live := auth.WithPrincipal(authz.WithTransport(authz.WithMode(ctx, authz.ModeCompat), "http"), dev)
	e.srv.recordQueuedInput(live, run.ID, "acme", "", runner.RunInput{Agent: agent, TenantID: "acme", UserID: "dev", Segments: oneUserSeg("queued")})
	return run
}

func (e *aclResumeEnv) restart(t *testing.T) (restarted, failed int) {
	t.Helper()
	restarted, failed, err := e.srv.RestartQueuedRuns(context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return restarted, failed
}

// A run queued by a live POST records whose rules it was accepted under, for
// the restart that may start it.
func TestQueuedRun_RecordsItsCallersACLOrigin(t *testing.T) {
	release, holding := make(chan struct{}), make(chan struct{}, 1)
	var releaseOnce sync.Once
	// Released on every exit, a failing one too: a holder left blocked keeps
	// the test server's Close waiting forever.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {endTurn()}, "child": {endTurn()}})
	e.prov.onCall = func(role string) {
		if role == "worker" {
			holding <- struct{}{}
			<-release
		}
	}
	e.srv.sem = concurrency.New(1, 8, 0)
	holder := make(chan struct{})
	go func() {
		defer close(holder)
		body := `{"agent":"worker","agent_id":"a_holder","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		_, _ = bearerCall(t, e.ts, workerBearer, "POST", "/v1/runs", body)
	}()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder never took the slot")
	}
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		body := `{"agent":"child","agent_id":"a_waiter","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		_, _ = bearerCall(t, e.ts, workerBearer, "POST", "/v1/runs", body)
	}()
	var run store.Run
	waitFor(t, "the second run to queue", func() bool {
		r, err := e.st.GetRunByAgentID(context.Background(), "a_waiter")
		run = r
		return err == nil && r.Status == store.RunQueued
	})
	var rec queuedRecord
	if err := json.Unmarshal(queuedRowInput(t, e.srv, run.ID), &rec); err != nil {
		t.Fatal(err)
	}
	want := aclOriginRecord{TenantID: "acme", Subject: "dev", TokenDefID: "def_worker", Class: "member", Transport: "http"}
	if rec.ACLOrigin == nil || *rec.ACLOrigin != want {
		t.Errorf("queued record acl_origin = %+v, want %+v", rec.ACLOrigin, want)
	}
	releaseOnce.Do(func() { close(release) })
	<-holder
	<-queued
}

// Started after a restart, the queued run is narrowed by its caller's set as
// a live start is — Bash withheld, scratch read-only, the tenant keyspace
// dropped, the set on its policy — and its own record names the caller, for
// a later resume.
func TestRestartQueuedRuns_StartsTheRunUnderItsCallersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_q"), endTurn()}})
	run := e.queueAsDev(t, "worker", "a_q_acl")
	if restarted, failed := e.restart(t); restarted != 1 || failed != 0 {
		t.Fatalf("restarted %d, failed %d; want 1/0", restarted, failed)
	}
	done := waitRunStatus(t, e.st, run.ID, store.RunCompleted)
	seen := e.waitProbes(t, 1)
	got := aclNarrowingOf(seen[0])
	if seen[0].ACL == nil || slices.Contains(got.Tools, "Bash") || !slices.Equal(got.Removed, []string{"Bash"}) ||
		len(got.Volumes) != 1 || !got.Volumes[0].ReadOnly || slices.Contains(got.Memory, "tenant") {
		t.Errorf("the restarted queued run is not narrowed by dev's list: %+v", got)
	}
	if offered := e.prov.toolsOf("worker"); len(offered) == 0 || slices.Contains(offered[0], "Bash") {
		t.Errorf("the restarted run offered the model %v; Bash must be withheld", offered)
	}
	if rec, ok := decodeRunConfig(done.RunConfig); !ok || rec.ACLOrigin == nil || rec.ACLOrigin.Subject != "dev" || rec.ACLOrigin.Transport != "http" {
		t.Errorf("the started run's record does not name its caller: %s", done.RunConfig)
	}
}

// A deny on running the agent bound while the run waited refuses it at the
// restart, as the live intake would have: it fails with acl_denied and its
// agent never makes a model call.
func TestRestartQueuedRuns_ADenyBoundDuringTheWaitRefusesTheRun(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	run := e.queueAsDev(t, "worker", "a_q_denied")
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-worker",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["run"],"resources":["agent:worker"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-worker")
	e.restart(t)
	failed := waitRunStatus(t, e.st, run.ID, store.RunFailed)
	if !strings.Contains(failed.ErrorMsg, "acl_denied") {
		t.Errorf("the run failed with %q; want acl_denied", failed.ErrorMsg)
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the denied run made %d model call(s)", n)
	}
}

// A queued run whose caller's set cannot be read at the restart is not
// started without it: it stays queued, the refusal is audited, and a later
// restart with the set readable starts it narrowed.
func TestRestartQueuedRuns_UnreadableCallerACLLeavesTheRunQueued(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_q"), endTurn()}})
	run := e.queueAsDev(t, "worker", "a_q_held")
	working := e.srv.ACLResolver()
	e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))
	if restarted, failed := e.restart(t); restarted != 0 || failed != 0 {
		t.Fatalf("restarted %d, failed %d; want neither", restarted, failed)
	}
	if got, err := e.st.GetRun(context.Background(), run.ID); err != nil || got.Status != store.RunQueued {
		t.Fatalf("run is %q (%v); want it still queued", got.Status, err)
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the held run made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_unavailable" && ev.ActorSubject == "dev"
	}) {
		t.Errorf("no acl_unavailable audit event for dev; events: %+v", e.audit.events)
	}

	e.srv.SetACLResolver(working)
	if restarted, _ := e.restart(t); restarted != 1 {
		t.Fatalf("the second restart started %d; want 1", restarted)
	}
	waitRunStatus(t, e.st, run.ID, store.RunCompleted)
	if seen := e.waitProbes(t, 1); seen[0].ACL == nil || slices.Contains(seen[0].Tools, "Bash") {
		t.Errorf("started without dev's set: %+v", aclNarrowingOf(seen[0]))
	}
}

// A queued run recorded with no caller's set starts as before: unnarrowed.
func TestRestartQueuedRuns_RunWithNoACLOriginStartsAsBefore(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_q"), endTurn()}})
	ctx := context.Background()
	sess, err := e.st.CreateSession(ctx, "acme", "worker", "dev")
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.st.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_q_none", TenantID: "acme", UserID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.recordQueuedInput(ctx, run.ID, "acme", "", runner.RunInput{Agent: "worker", Segments: oneUserSeg("queued")})
	e.restart(t)
	waitRunStatus(t, e.st, run.ID, store.RunCompleted)
	if seen := e.waitProbes(t, 1); seen[0].ACL != nil || !slices.Contains(seen[0].Tools, "Bash") {
		t.Errorf("a run queued with no caller's set started narrowed: %+v", aclNarrowingOf(seen[0]))
	}
}
