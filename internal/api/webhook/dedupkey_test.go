package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// headRunner starts runs as rows in a real store, carrying the delivery keys
// the receiver gives them, so a test can end a run failed or delete it.
type headRunner struct {
	st   store.Store
	mu   sync.Mutex
	runs []string
	err  error // returned before a run registers
	// admitted are the runs that passed the caller's Admit and would have
	// made their first model call.
	admitted []string
	// One-shot knobs for the next RunOnce:
	//   gate               it waits on gate before creating its row (a slow
	//                      setup);
	//   crash              it creates its row and returns without registering
	//                      (the process died there);
	//   failBeforeRegister it creates its row, ends it failed and returns an
	//                      error, never registering (as an agent_id collision).
	gate               chan struct{}
	atGate             chan struct{} // closed once it waits on gate
	crash              bool
	failBeforeRegister bool
}

var errSetupFailed = errors.New("setup failed before the run registered")

func (r *headRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	r.mu.Lock()
	err := r.err
	gate, atGate, crash, failEarly := r.gate, r.atGate, r.crash, r.failBeforeRegister
	r.gate, r.atGate, r.crash, r.failBeforeRegister = nil, nil, false, false
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if gate != nil {
		if atGate != nil {
			close(atGate)
		}
		<-gate
	}
	r.mu.Lock()
	n := len(r.runs)
	r.mu.Unlock()
	sess, err := r.st.CreateSession(ctx, in.TenantID, in.Agent, "u")
	if err != nil {
		return err
	}
	run, err := r.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: fmt.Sprintf("ag_%d", n), TenantID: in.TenantID,
		IdempotencyKey: in.IdempotencyKey, DeliveryAltKey: in.DeliveryAltKey,
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.runs = append(r.runs, run.ID)
	r.mu.Unlock()
	switch {
	case crash:
		return nil
	case failEarly:
		if err := r.st.FinishRun(ctx, run.ID, store.RunFailed, "", store.Usage{}, errSetupFailed.Error()); err != nil {
			return err
		}
		return errSetupFailed
	}
	if cb.OnRegistered != nil {
		cb.OnRegistered(fmt.Sprintf("ag_%d", n), run.ID, sess.ID, "")
	}
	// As the server does: a refusal fails the run before its first call.
	if cb.Admit != nil {
		if aerr := cb.Admit(run.ID); aerr != nil {
			if err := r.st.FinishRun(ctx, run.ID, store.RunFailed, "", store.Usage{}, aerr.Error()); err != nil {
				return err
			}
			return fmt.Errorf("run not admitted by its caller: %w", aerr)
		}
	}
	r.mu.Lock()
	r.admitted = append(r.admitted, run.ID)
	r.mu.Unlock()
	return nil
}

func (r *headRunner) runsCreated() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runs...)
}

func (r *headRunner) admittedRuns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.admitted...)
}

func (r *headRunner) started() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func (r *headRunner) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// githubHeadHook is the RFC's verification definition: github push events,
// one run per pushed head.
func githubHeadHook() config.Webhook {
	w := githubPushHook()
	w.DedupKey = []string{"$.repository.id", "$.ref", "$.after"}
	return w
}

// headPush is a push of head after; pad changes the body's bytes and nothing
// the dedup_key reads, as two deliveries of one push can differ.
func headPush(after, pad string) string {
	b, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/main", "before": "aaa", "after": after,
		"repository": map[string]any{"id": 42, "full_name": "acme/app"},
		"pad":        pad,
	})
	return string(b)
}

func newHeadReceiver(st store.Store, r runner.Runner, c *testClock, hooks map[string]config.Webhook) *Receiver {
	return New(Deps{
		Cfg:          &config.Config{Webhooks: hooks},
		Store:        st,
		Runner:       r,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          c.now,
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": scopeSecret}),
	})
}

func pushHead(t *testing.T, label string, rec *Receiver, body, id string, wantCode int) map[string]string {
	t.Helper()
	return answer(t, label, rec, "/v1/_webhooks/gh", body, githubDelivery(body, id, "push"), wantCode)
}

func finishRun(t *testing.T, st store.Store, runID string, status store.RunStatus) {
	t.Helper()
	if err := st.FinishRun(context.Background(), runID, status, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
}

// One pushed head is one run, whatever the delivery id and however the bytes
// differ, on a receiver that has never seen it (a restart, another replica).
func TestReceiver_DedupKey_OneEventIsOneRun(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st}
	c := &testClock{t: time.Unix(1_700_000_000, 0)}
	hooks := map[string]config.Webhook{"gh": githubHeadHook()}
	rec := newHeadReceiver(st, sr, c, hooks)

	first := pushHead(t, "the push", rec, headPush("bbb", "1"), "d1", http.StatusAccepted)["run_id"]
	if first == "" {
		t.Fatal("the push started no run")
	}
	got := pushHead(t, "the same head, a new id and new bytes", rec, headPush("bbb", "2"), "d2", http.StatusOK)
	if got["deduped"] != "true" || got["run_id"] != first {
		t.Errorf("answered %v, want a duplicate of %s", got, first)
	}
	if pushHead(t, "another head", rec, headPush("ccc", "1"), "d3", http.StatusAccepted)["run_id"] == "" {
		t.Error("another head started no run")
	}
	c.advance(time.Hour)
	restarted := newHeadReceiver(st, sr, c, hooks)
	if got := pushHead(t, "the first head after a restart", restarted, headPush("bbb", "3"), "d4", http.StatusOK); got["run_id"] != first {
		t.Errorf("after a restart: %v, want a duplicate of %s", got, first)
	}
	if n := sr.started(); n != 2 {
		t.Errorf("%d runs started, want 2", n)
	}
}

// The owner's rule: a head whose run failed is handled again when it is
// redelivered, byte for byte; one whose run completed and was deleted, or was
// cancelled, is not.
func TestReceiver_DedupKey_AFailedRunsEventIsHandledAgain(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st}
	rec := newHeadReceiver(st, sr, &testClock{t: time.Unix(1_700_000_000, 0)}, map[string]config.Webhook{"gh": githubHeadHook()})
	body := headPush("bbb", "1")

	r1 := pushHead(t, "the push", rec, body, "d1", http.StatusAccepted)["run_id"]
	finishRun(t, st, r1, store.RunFailed)
	r2 := pushHead(t, "its redelivery after the run failed", rec, body, "d1", http.StatusAccepted)["run_id"]
	if r2 == "" || r2 == r1 {
		t.Fatalf("the redelivery after a failed run started %q, want a new run", r2)
	}
	if got := pushHead(t, "again", rec, body, "d1", http.StatusOK); got["run_id"] != r2 {
		t.Errorf("a redelivery while the new run holds the head: %v, want a duplicate of %s", got, r2)
	}

	finishRun(t, st, r2, store.RunCompleted)
	if _, err := st.DeleteRunSet(context.Background(), []string{r2}, nil); err != nil {
		t.Fatal(err)
	}
	pushHead(t, "after the run completed and was deleted", rec, headPush("bbb", "2"), "d2", http.StatusOK)

	cancelled := headPush("ccc", "1")
	r3 := pushHead(t, "another head", rec, cancelled, "d3", http.StatusAccepted)["run_id"]
	finishRun(t, st, r3, store.RunCancelled)
	pushHead(t, "after its run was cancelled", rec, cancelled, "d3", http.StatusOK)

	if n := sr.started(); n != 3 {
		t.Errorf("%d runs started, want 3", n)
	}
}

// runClaimCountingStore counts the dedup_key claims that reach the store, each
// a write transaction.
type runClaimCountingStore struct {
	store.Store
	mu     sync.Mutex
	claims int
}

func (s *runClaimCountingStore) WebhookDeliveryClaimRun(ctx context.Context, c store.WebhookRunClaim) (store.WebhookRunClaimResult, error) {
	s.mu.Lock()
	s.claims++
	s.mu.Unlock()
	return s.Store.WebhookDeliveryClaimRun(ctx, c)
}

func (s *runClaimCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

// A duplicate of an event a live run holds was answered by a write
// transaction in the store every time, before the rate limit — so a captured
// signed body replayed in a loop was one write per request. A duplicate of an
// event a live run holds is answered after a read instead; once that run has
// failed, the next delivery goes to the claim and takes the event over.
func TestReceiver_DedupKey_ARepeatedDuplicateDoesNotWriteToTheStore(t *testing.T) {
	st := &runClaimCountingStore{Store: openScopeStore(t)}
	sr := &headRunner{st: st.Store}
	rec := newHeadReceiver(st, sr, &testClock{t: time.Unix(1_700_000_000, 0)}, map[string]config.Webhook{"gh": githubHeadHook()})

	r1 := pushHead(t, "the push", rec, headPush("bbb", "0"), "d0", http.StatusAccepted)["run_id"]
	for i := 1; i <= 10; i++ {
		got := pushHead(t, "a duplicate", rec, headPush("bbb", fmt.Sprint(i)), fmt.Sprintf("d%d", i), http.StatusOK)
		if got["deduped"] != "true" || got["run_id"] != r1 {
			t.Fatalf("duplicate %d: %v, want a duplicate of %s", i, got, r1)
		}
	}
	if n := st.count(); n != 1 {
		t.Errorf("the push and 10 duplicates made %d claims in the store, want 1 (the push)", n)
	}

	finishRun(t, st.Store, r1, store.RunFailed)
	r2 := pushHead(t, "a redelivery once the run failed", rec, headPush("bbb", "x"), "dx", http.StatusAccepted)["run_id"]
	if r2 == "" || r2 == r1 {
		t.Fatalf("the redelivery after a failed run started %q, want a new run", r2)
	}
	if n := sr.started(); n != 2 {
		t.Errorf("%d runs started, want 2", n)
	}
}

// headCommitHook names the event by a field the push shape does not require,
// so a body without it gets past the shape and must be refused here.
func headCommitHook() config.Webhook {
	w := githubPushHook()
	w.DedupKey = []string{"$.repository.id", "$.head_commit.id"}
	return w
}

// A body that lacks one of the fields, or holds null there, cannot name its
// event: refused, nothing started and nothing held.
func TestReceiver_DedupKey_ABodyWithoutItsFieldsIsRefused(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st}
	rec := newHeadReceiver(st, sr, &testClock{t: time.Unix(1_700_000_000, 0)}, map[string]config.Webhook{"gh": headCommitHook()})
	for label, body := range map[string]string{
		"no head_commit":   `{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42}}`,
		"null head_commit": `{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42},"head_commit":null}`,
		"null id":          `{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42},"head_commit":{"id":null}}`,
	} {
		w := post(rec, "/v1/_webhooks/gh", []byte(body), githubDelivery(body, "d1", "push"))
		if w.Code != http.StatusBadRequest || !jsonHas(w.Body.Bytes(), "error", "dedup_key_unresolvable") || !strings.Contains(w.Body.String(), "$.head_commit.id") {
			t.Errorf("%s: %d %s, want 400 dedup_key_unresolvable naming $.head_commit.id", label, w.Code, w.Body.String())
		}
	}
	if sr.started() != 0 {
		t.Fatal("a body without its dedup_key started a run")
	}
	whole := `{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42},"head_commit":{"id":"bbb"}}`
	pushHead(t, "the whole push, same id", rec, whole, "d1", http.StatusAccepted)
}

// The claim comes before the rate limit, so a duplicate costs no token, and
// every refusal after it gives the keys back: the sender's retry is handled.
func TestReceiver_DedupKey_ARefusedDeliveryHoldsNothing(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st}
	c := &testClock{t: time.Unix(1_700_000_000, 0)}
	hook := githubHeadHook()
	hook.RateLimit = config.WebhookRateLimit{RequestsPerMinute: 1, Burst: 1}
	rec := newHeadReceiver(st, sr, c, map[string]config.Webhook{"gh": hook})

	pushHead(t, "head A", rec, headPush("aaa", "1"), "d1", http.StatusAccepted)
	pushHead(t, "head A again, with the bucket empty", rec, headPush("aaa", "2"), "d2", http.StatusOK)
	pushHead(t, "head B, with the bucket empty", rec, headPush("bbb", "1"), "d3", http.StatusTooManyRequests)
	c.advance(time.Minute)
	if got := pushHead(t, "head B's retry", rec, headPush("bbb", "1"), "d3", http.StatusAccepted); got["deduped"] != "" {
		t.Errorf("the retry of a rate-limited delivery: %v, want a new run", got)
	}

	c.advance(time.Minute)
	sr.setErr(runner.ErrBackpressure)
	pushHead(t, "head C, refused at setup", rec, headPush("ccc", "1"), "d4", http.StatusServiceUnavailable)
	sr.setErr(nil)
	c.advance(time.Minute)
	if got := pushHead(t, "head C's retry", rec, headPush("ccc", "1"), "d4", http.StatusAccepted); got["deduped"] != "" {
		t.Errorf("the retry of a delivery refused at setup: %v, want a new run", got)
	}
	if n := sr.started(); n != 3 {
		t.Errorf("%d runs started, want 3", n)
	}
}

// Past dedup_ttl_seconds a head is handled again, but the body the first run
// was started by is still refused: the run's own keys last as long as it.
func TestReceiver_DedupKey_ExpiresButTheRunKeepsItsBody(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st}
	c := &testClock{t: time.Unix(1_700_000_000, 0)}
	hook := githubHeadHook()
	hook.DedupTTLSeconds = 3600
	rec := newHeadReceiver(st, sr, c, map[string]config.Webhook{"gh": hook})
	captured := headPush("bbb", "1")

	r1 := pushHead(t, "the push", rec, captured, "d1", http.StatusAccepted)["run_id"]
	finishRun(t, st, r1, store.RunCompleted)
	c.advance(time.Hour - time.Second)
	pushHead(t, "the head, just inside the lifetime", rec, headPush("bbb", "2"), "d2", http.StatusOK)
	c.advance(time.Second)
	if got := pushHead(t, "the head, past the lifetime", rec, headPush("bbb", "3"), "d3", http.StatusAccepted); got["deduped"] != "" {
		t.Errorf("past the lifetime: %v, want a new run", got)
	}
	c.advance(time.Hour)
	if got := pushHead(t, "the captured body, past the lifetime", rec, captured, "d4", http.StatusAccepted); got["deduped"] != "true" || got["run_id"] != r1 {
		t.Errorf("a byte-identical capture past the lifetime: %v, want a duplicate of %s", got, r1)
	}
	if n := sr.started(); n != 2 {
		t.Errorf("%d runs started, want 2", n)
	}
}

// A channel delivery holds the key too: one publish per head.
func TestReceiver_DedupKey_OnAChannelPublishesOncePerEvent(t *testing.T) {
	st := openScopeStore(t)
	pub := &countingPublisher{}
	hook := channelHook(githubChannel)
	hook.DedupKey = []string{"$.ref", "$.after"}
	rec := channelReplica(st, pub, channelT0, map[string]config.Webhook{"gh": hook})
	wantStatus(t, "the push", deliver(rec, "gh", githubChannel, []byte(headPush("bbb", "1")), "e1"), http.StatusAccepted)
	wantStatus(t, "the same head, new bytes", deliver(rec, "gh", githubChannel, []byte(headPush("bbb", "2")), "e2"), http.StatusOK)
	if n := pub.count(); n != 1 {
		t.Errorf("%d publishes, want 1", n)
	}
}

// A channel delivery's keys were held a day before dedup_key existed; a short
// dedup_ttl_seconds does not shorten that.
func TestReceiver_DedupKey_OnAChannelHoldsADayAtLeast(t *testing.T) {
	st := openScopeStore(t)
	pub := &countingPublisher{}
	hook := channelHook(githubChannel)
	hook.DedupKey = []string{"$.ref", "$.after"}
	hook.DedupTTLSeconds = 60
	hooks := map[string]config.Webhook{"gh": hook}
	captured := []byte(headPush("bbb", "1"))
	wantStatus(t, "the push", deliver(channelReplica(st, pub, channelT0, hooks), "gh", githubChannel, captured, "e1"), http.StatusAccepted)
	later := channelReplica(st, pub, channelT0.Add(time.Hour), hooks)
	wantStatus(t, "the same body an hour later", deliver(later, "gh", githubChannel, captured, "e2"), http.StatusOK)
	if n := pub.count(); n != 1 {
		t.Errorf("%d publishes, want 1", n)
	}
}

// A team delivery holds the key, recording the walk it started.
func TestReceiver_DedupKey_OnATeamStartsOneWalkPerEvent(t *testing.T) {
	st := openScopeStore(t)
	teams := &fakeTeams{}
	hook := teamHook()
	hook.DedupKey = []string{"$.repository.full_name", "$.pull_request.number"}
	rec, _ := newTeamReceiver(t, hook, teams, st)
	if code, resp := signedTeamPost(rec, []byte(`{"repository":{"full_name":"acme/app"},"pull_request":{"number":7},"pad":1}`), "d1"); code != http.StatusAccepted || resp["run_id"] != "r_walk" {
		t.Fatalf("the first delivery: %d %v", code, resp)
	}
	code, resp := signedTeamPost(rec, []byte(`{"repository":{"full_name":"acme/app"},"pull_request":{"number":7},"pad":2}`), "d2")
	if code != http.StatusOK || resp["deduped"] != "true" || resp["run_id"] != "r_walk" {
		t.Errorf("the same pull request again: %d %v, want a duplicate of r_walk", code, resp)
	}
	if n := len(teams.started()); n != 1 {
		t.Errorf("%d walks started, want 1", n)
	}
}

// The event's identity is its values as signed: numbers past 2^53 stay
// apart, and an object's key order does not matter.
func TestDedupFieldsID_IsTheSignedValues(t *testing.T) {
	id := func(body string) string {
		t.Helper()
		got, missing, err := dedupFieldsID([]string{"$.n", "$.o"}, []byte(body))
		if err != nil || missing >= 0 {
			t.Fatalf("%s: %v, missing %d", body, err, missing)
		}
		return got
	}
	if id(`{"n":9007199254740993,"o":1}`) == id(`{"n":9007199254740992,"o":1}`) {
		t.Error("two numbers past 2^53 share an identity")
	}
	if id(`{"n":1,"o":{"a":1,"b":2}}`) != id(`{"o":{"b":2,"a":1},"n":1}`) {
		t.Error("an object's key order changed the identity")
	}
	if id(`{"n":1,"o":"1"}`) == id(`{"n":1,"o":1}`) {
		t.Error("a string and a number share an identity")
	}
	if _, missing, _ := dedupFieldsID([]string{"$.n", "$.o"}, []byte(`{"n":1}`)); missing != 1 {
		t.Errorf("missing = %d, want 1", missing)
	}
	if _, _, err := dedupFieldsID([]string{"$.n"}, []byte(`n=1`)); err == nil {
		t.Error("a body that is not JSON named an event")
	}
	if strings.HasPrefix(eventKey("w", "x"), "webhook:") {
		t.Error("an event key shares the delivery keys' namespace, which a sender's delivery id can spell")
	}
}

// The dry-run refuses a body that cannot name its event.
func TestReceiver_Test_ReportsAnUnresolvableDedupKey(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st}
	rec := newHeadReceiver(st, sr, &testClock{t: time.Unix(1_700_000_000, 0)}, map[string]config.Webhook{"gh": headCommitHook()})
	srv := triageServer(t, rec, triageTokens)
	for body, want := range map[string]string{
		`{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42}}`:                            verdictRejectedDedupKey,
		`{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42},"head_commit":{"id":"bbb"}}`: verdictAccepted,
	} {
		w := triageDo(srv, http.MethodPost, "/v1/_webhooks/gh/test", triageAdminBearer, []byte(body), githubDelivery(body, "", "push"))
		var res testResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if res.Verdict != want {
			t.Errorf("%s: verdict %q, want %q", body, res.Verdict, want)
		}
	}
	if sr.started() != 0 {
		t.Fatal("the dry-run started a run")
	}
}

// The body a dedup_key reads is held to the shape limits every other decode of
// a delivery is: for a webhook with no events or filters it is the first
// decode, ahead of the payload mapping's.
func TestDedupFieldsID_RefusesABodyPastTheShapeLimits(t *testing.T) {
	deep := func(n int) []byte {
		return []byte(`{"n":1,"d":` + strings.Repeat(`[`, n) + strings.Repeat(`]`, n) + `}`)
	}
	if _, missing, err := dedupFieldsID([]string{"$.n"}, deep(maxJSONDepth-1)); err != nil || missing >= 0 {
		t.Errorf("a body at the depth limit: missing %d, %v", missing, err)
	}
	if _, _, err := dedupFieldsID([]string{"$.n"}, deep(maxJSONDepth+1)); err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Errorf("a body past the depth limit: %v, want it refused before decoding", err)
	}
}
