package http

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EI P3 — a queued run is started again after a restart.

const testQueueBearer = "bearer-for-the-queued-run-0001"

func testSealer(t *testing.T, seed byte) *credential.Sealer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	s, err := credential.NewSealer(base64.StdEncoding.EncodeToString(key), "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// queuedFixture makes a queued row as RunOnce would have left it when the
// process stopped mid-wait: the row, queued, and its stored request.
func queuedFixture(t *testing.T, s *Server, agentID, idemKey string, in runner.RunInput, retention string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := s.store.CreateSession(ctx, "acme", "r", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, TenantID: "acme", UserID: "alice", IdempotencyKey: idemKey, Retention: retention,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.recordQueuedInput(ctx, run.ID, "acme", retention, in)
	return run
}

func queuedRowInput(t *testing.T, s *Server, runID string) json.RawMessage {
	t.Helper()
	rows, err := s.store.ListQueuedRuns(context.Background(), "", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range rows {
		if q.Run.ID == runID {
			return q.Input
		}
	}
	t.Fatalf("run %s is not listed as queued", runID)
	return nil
}

// A run that queues stores what a restart needs: its request, with its
// per-run secrets sealed — never in plaintext — and bound to the run. The
// restart rebuilds the request, the secrets and the row's identity from it.
func TestQueuedRun_StoresItsRequestWithTheSecretsSealed(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	s.SetQueueSealer(testSealer(t, 1))
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds the slot")})
	holder.ids(t)

	queued := startRunOnce(s, context.Background(), runner.RunInput{
		Agent: "r", TenantID: "acme", UserID: "alice", Segments: oneUserSeg("the queued prompt"),
		UserBearer: testQueueBearer, UserCredentials: map[string]string{"github": "ghp-the-credential-value"},
		Metadata: map[string]any{"ticket": "T-1"},
	})
	_, runID := queued.ids(t)
	raw := queuedRowInput(t, s, runID)
	if !strings.Contains(string(raw), "the queued prompt") || !strings.Contains(string(raw), "T-1") {
		t.Errorf("stored request = %s, want the prompt and the metadata", raw)
	}
	for _, secret := range []string{testQueueBearer, "ghp-the-credential-value"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("stored request holds a secret in plaintext: %s", raw)
		}
	}

	q := store.QueuedRun{Input: raw}
	q.Run, _ = s.store.GetRun(context.Background(), runID)
	in, why := s.restartInput(q)
	if why != "" {
		t.Fatalf("restartInput: %s", why)
	}
	if in.UserBearer != testQueueBearer || in.UserCredentials["github"] != "ghp-the-credential-value" {
		t.Errorf("restored secrets = %q %v, want the run's own", in.UserBearer, in.UserCredentials)
	}
	if in.QueuedRunID != runID || in.TenantID != "acme" || in.UserID != "alice" || !in.Detached {
		t.Errorf("restored identity = run %q tenant %q user %q detached %v, want the row's, detached", in.QueuedRunID, in.TenantID, in.UserID, in.Detached)
	}

	// Sealed to THIS run: the same envelope on another row does not open.
	other := q
	other.Run.ID = "some-other-run"
	if _, why := s.restartInput(other); why == "" {
		t.Error("a run's sealed secrets opened on another run's row")
	}
	// Nor under another key.
	s.SetQueueSealer(testSealer(t, 9))
	if _, why := s.restartInput(q); !strings.Contains(why, "cannot be opened") {
		t.Errorf("restart under another key = %q, want a refusal naming the secrets", why)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	_ = holder.wait(t)
	_ = queued.wait(t)
}

// A run with secrets on a deployment with no LOOMCYCLE_SECRET_KEY stores
// neither the secrets nor the request: the restart fails it, naming the key.
// A no-trace run is the same: its prompt is stored nowhere. Both release
// their idempotency keys, so a caller's retry runs the work.
func TestRestartQueuedRuns_FailsARunItCannotStartWithTheReason(t *testing.T) {
	s, _ := newGatedBatchServer(t, 1)
	s.SetQueueSealer(nil)
	withSecrets := queuedFixture(t, s, "a_qsecret", "key-secret",
		runner.RunInput{Agent: "r", Segments: oneUserSeg("x"), UserBearer: testQueueBearer}, "")
	noTrace := queuedFixture(t, s, "a_qnotrace", "key-notrace",
		runner.RunInput{Agent: "r", Segments: oneUserSeg("never stored")}, store.RetentionNone)
	if raw := queuedRowInput(t, s, withSecrets.ID); strings.Contains(string(raw), testQueueBearer) || strings.Contains(string(raw), `"input"`) {
		t.Fatalf("a run with secrets and no key stored = %s, want only the reason", raw)
	}
	if raw := queuedRowInput(t, s, noTrace.ID); strings.Contains(string(raw), "never stored") {
		t.Fatalf("a no-trace run's prompt was stored: %s", raw)
	}
	// An older build's row has no stored request at all.
	sess, _ := s.store.CreateSession(context.Background(), "acme", "r", "alice")
	bare, err := s.store.CreateQueuedRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_qbare", TenantID: "acme", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	restarted, failed, err := s.RestartQueuedRuns(context.Background(), time.Now().Add(time.Second))
	if err != nil || restarted != 0 || failed != 3 {
		t.Fatalf("RestartQueuedRuns = %d restarted, %d failed (%v), want 0/3", restarted, failed, err)
	}
	for id, want := range map[string]string{withSecrets.ID: "LOOMCYCLE_SECRET_KEY", noTrace.ID: "no-trace", bare.ID: "not stored"} {
		got, _ := s.store.GetRun(context.Background(), id)
		if got.Status != store.RunFailed || got.StopReason != "restarted" || !strings.Contains(got.ErrorMsg, want) {
			t.Errorf("run %s = %q %q %q, want failed, restarted, a reason naming %q", id, got.Status, got.StopReason, got.ErrorMsg, want)
		}
	}
	if r, found, err := s.store.RunByIdempotencyKey(context.Background(), "key-secret"); err != nil || found {
		t.Errorf("a failed queued run still holds its idempotency key (run %s, %v)", r.ID, err)
	}
}

// The restart starts the run under its own row — the same run id, agent id
// and idempotency key, no second row — runs it to its end with the prompt it
// queued with, and records that prompt on the transcript only then.
func TestRestartQueuedRuns_StartsTheRunUnderItsOwnRow(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	run := queuedFixture(t, s, "a_qrestart", "key-restart",
		runner.RunInput{Agent: "r", Segments: oneUserSeg("queued before the restart")}, "")

	restarted, failed, err := s.RestartQueuedRuns(context.Background(), time.Now().Add(time.Second))
	if err != nil || restarted != 1 || failed != 0 {
		t.Fatalf("RestartQueuedRuns = %d/%d (%v), want 1/0", restarted, failed, err)
	}
	waitRunStatus(t, s.store, run.ID, store.RunRunning)
	gate <- struct{}{}
	done := waitRunStatus(t, s.store, run.ID, store.RunCompleted)
	if done.AgentID != "a_qrestart" || done.IdempotencyKey != "key-restart" {
		t.Errorf("restarted run = agent %q key %q, want its own", done.AgentID, done.IdempotencyKey)
	}
	if byAgent, err := s.store.GetRunByAgentID(context.Background(), "a_qrestart"); err != nil || byAgent.ID != run.ID {
		t.Errorf("agent id resolves to run %q (%v), want the queued row %q — no second row", byAgent.ID, err, run.ID)
	}
	evs, err := s.store.GetRunEventsSince(context.Background(), run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !transcriptMentions(evs, "queued before the restart") {
		t.Errorf("transcript = %+v, want the prompt the run queued with", evs)
	}
	// It started at once, but it had queued: its transcript says admitted.
	admitted := false
	for _, ev := range evs {
		admitted = admitted || ev.Type == string(providers.EventAdmitted)
	}
	if !admitted {
		t.Errorf("transcript types = %v, want an admitted event", eventTypes(evs))
	}
}

func eventTypes(evs []store.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

// The restarted runs queue again in the order they first queued.
func TestRestartQueuedRuns_KeepsTheQueueOrder(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)
	first := queuedFixture(t, s, "a_qfirst", "", runner.RunInput{Agent: "r", Segments: oneUserSeg("1")}, "")
	time.Sleep(2 * time.Millisecond)
	second := queuedFixture(t, s, "a_qsecond", "", runner.RunInput{Agent: "r", Segments: oneUserSeg("2")}, "")

	if n, _, err := s.RestartQueuedRuns(context.Background(), time.Now().Add(time.Second)); err != nil || n != 2 {
		t.Fatalf("RestartQueuedRuns = %d (%v), want 2", n, err)
	}
	p1, ok1 := s.queuePositionOf("a_qfirst")
	p2, ok2 := s.queuePositionOf("a_qsecond")
	if !ok1 || !ok2 || p1.Position != 1 || p2.Position != 2 {
		t.Fatalf("positions = %+v %v, %+v %v, want first 1, second 2", p1, ok1, p2, ok2)
	}
	for range 3 {
		gate <- struct{}{}
	}
	_ = holder.wait(t)
	waitRunStatus(t, s.store, first.ID, store.RunCompleted)
	waitRunStatus(t, s.store, second.ID, store.RunCompleted)
}

// POST /v1/runs stores its request when its run queues, as RunOnce does.
func TestHandleRuns_QueuedRunStoresItsRequest(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	ts := httptest.NewServer(s.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json",
		strings.NewReader(`{"agent":"r","agent_id":"a_qpost","prompt":"posted while full"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "event: "+string(providers.EventQueued) {
			break
		}
	}
	run, err := s.store.GetRunByAgentID(context.Background(), "a_qpost")
	if err != nil {
		t.Fatal(err)
	}
	q := store.QueuedRun{Run: run, Input: queuedRowInput(t, s, run.ID)}
	in, why := s.restartInput(q)
	if why != "" || !strings.Contains(firstUserText(in.Segments), "posted while full") {
		t.Errorf("restart input from a POST = %q (%s), want its prompt", firstUserText(in.Segments), why)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	_ = holder.wait(t)
	_, _ = io.Copy(io.Discard, resp.Body)
}

func transcriptMentions(evs []store.Event, text string) bool {
	for _, ev := range evs {
		if strings.Contains(string(ev.Payload), text) {
			return true
		}
	}
	return false
}
