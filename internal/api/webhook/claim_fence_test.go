package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// waitRun polls a run row until cond holds.
func waitRun(t *testing.T, st store.Store, runID string, cond func(store.Run) bool) store.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := st.GetRun(context.Background(), runID)
		if err == nil && cond(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s: %+v (%v), never reached the expected state", runID, r, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitAdmitted polls until runID has passed the runner's Admit and returns
// every run that has. The receiver answers a delivery once its run registers,
// and Admit runs after that, so a run can be answered for and not yet
// admitted.
func waitAdmitted(t *testing.T, sr *headRunner, runID string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		adm := sr.admittedRuns()
		for _, id := range adm {
			if id == runID {
				return adm
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never passed Admit (admitted: %v)", runID, adm)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A delivery whose run setup outlives the claim's grace loses its event to
// the event's next delivery, which starts the one run. The slow setup, when it
// finally registers, finds its claim taken over and is refused before its
// first model call: its run ends failed and gives its keys back, and a
// redelivery of its bytes is a duplicate of the run that did start.
func TestReceiver_DedupKey_ASetupThatOutlivesItsClaimNeverRuns(t *testing.T) {
	st := openScopeStore(t)
	gate, atGate := make(chan struct{}), make(chan struct{})
	sr := &headRunner{st: st, gate: gate, atGate: atGate}
	c := &testClock{t: time.Unix(1_700_000_000, 0)}
	rec := newHeadReceiver(st, sr, c, map[string]config.Webhook{"gh": githubHeadHook()})

	slowBody := headPush("bbb", "1")
	slow := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		slow <- post(rec, "/v1/_webhooks/gh", []byte(slowBody), githubDelivery(slowBody, "d1", "push"))
	}()
	<-atGate // claimed; its setup is stuck

	c.advance(store.WebhookUnregisteredClaimGrace + time.Minute)
	winner := pushHead(t, "the redelivery past the grace", rec, headPush("bbb", "2"), "d2", http.StatusAccepted)["run_id"]
	if winner == "" {
		t.Fatal("the redelivery past the grace started no run")
	}

	close(gate)
	<-slow
	var slowRun string
	for _, id := range sr.runsCreated() {
		if id != winner {
			slowRun = id
		}
	}
	if slowRun == "" {
		t.Fatal("the slow setup created no run row")
	}
	got := waitRun(t, st, slowRun, func(r store.Run) bool { return r.Status == store.RunFailed })
	if got.IdempotencyKey != "" || got.DeliveryAltKey != "" {
		t.Errorf("the refused run kept its delivery keys (%q, %q)", got.IdempotencyKey, got.DeliveryAltKey)
	}
	if adm := waitAdmitted(t, sr, winner); len(adm) != 1 || adm[0] != winner {
		t.Errorf("runs past Admit = %v, want only %s", adm, winner)
	}
	if again := pushHead(t, "the slow delivery's bytes again", rec, slowBody, "d1", http.StatusOK); again["run_id"] != winner {
		t.Errorf("a redelivery of the slow delivery: %v, want a duplicate of %s", again, winner)
	}
}

// A run that registered but whose claim was never linked to it — the process
// died between the two — is found by its keys once the grace has passed: the
// event's next delivery is answered as a duplicate of it, not a second run.
func TestReceiver_DedupKey_ARunNeverLinkedIsAdoptedNotRepeated(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st, crash: true}
	c := &testClock{t: time.Unix(1_700_000_000, 0)}
	rec := newHeadReceiver(st, sr, c, map[string]config.Webhook{"gh": githubHeadHook()})

	post(rec, "/v1/_webhooks/gh", []byte(headPush("bbb", "1")), githubDelivery(headPush("bbb", "1"), "d1", "push"))
	created := sr.runsCreated()
	if len(created) != 1 {
		t.Fatalf("runs created = %v, want the one whose process died", created)
	}
	c.advance(store.WebhookUnregisteredClaimGrace + time.Minute)
	got := pushHead(t, "the redelivery past the grace", rec, headPush("bbb", "2"), "d2", http.StatusOK)
	if got["deduped"] != "true" || got["run_id"] != created[0] {
		t.Errorf("answered %v, want a duplicate of %s", got, created[0])
	}
	if n := sr.started(); n != 1 {
		t.Errorf("%d runs created, want 1", n)
	}
}

// linkFailingStore refuses every link of a run to its claim.
type linkFailingStore struct{ store.Store }

func (linkFailingStore) WebhookDeliveryLinkRun(context.Context, string, string) (bool, error) {
	return false, errors.New("store unavailable")
}

// A run whose link to its claim cannot be written is not left running
// unlinked: it is refused before its first model call, the sender is told to
// retry, and the retry is handled.
func TestReceiver_DedupKey_ARunThatCannotLinkFailsClosed(t *testing.T) {
	real := openScopeStore(t)
	sr := &headRunner{st: real}
	c := &testClock{t: time.Unix(1_700_000_000, 0)}
	hooks := map[string]config.Webhook{"gh": githubHeadHook()}
	body := headPush("bbb", "1")

	failing := newHeadReceiver(linkFailingStore{real}, sr, c, hooks)
	pushHead(t, "a delivery whose link fails", failing, body, "d1", http.StatusServiceUnavailable)
	if adm := sr.admittedRuns(); len(adm) != 0 {
		t.Fatalf("an unlinked run went past Admit: %v", adm)
	}
	created := sr.runsCreated()
	if len(created) != 1 {
		t.Fatalf("runs created = %v", created)
	}
	waitRun(t, real, created[0], func(r store.Run) bool { return r.Status == store.RunFailed })

	rec := newHeadReceiver(real, sr, c, hooks)
	got := pushHead(t, "the sender's retry", rec, body, "d1", http.StatusAccepted)
	if got["run_id"] == "" || got["deduped"] != "" {
		t.Fatalf("the retry: %v, want a new run", got)
	}
	if adm := waitAdmitted(t, sr, got["run_id"]); len(adm) != 1 {
		t.Errorf("runs past Admit = %v, want the retry's", adm)
	}
}

// Without a dedup_key too: a run that failed before it registered leaves no
// delivery key behind, so the sender's retry of the same bytes starts a run
// instead of being answered as a duplicate of one that never ran.
func TestReceiver_ARunThatFailedBeforeRegisteringDoesNotHoldItsDelivery(t *testing.T) {
	st := openScopeStore(t)
	sr := &headRunner{st: st, failBeforeRegister: true}
	rec := newHeadReceiver(st, sr, &testClock{t: time.Unix(1_700_000_000, 0)}, map[string]config.Webhook{"gh": githubPushHook()})
	body := headPush("bbb", "1")

	pushHead(t, "the delivery", rec, body, "d1", http.StatusServiceUnavailable)
	if got := pushHead(t, "its retry", rec, body, "d1", http.StatusAccepted); got["run_id"] == "" || got["deduped"] != "" {
		t.Errorf("the retry: %v, want a new run", got)
	}
	if n := sr.started(); n != 2 {
		t.Errorf("%d runs created, want 2", n)
	}
}
