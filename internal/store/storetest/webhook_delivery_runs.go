package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A webhook dedup_key's claim records the run it started. The rule the owner
// set: the next delivery of the event takes the keys over only when that run
// ended failed; one that completed, was cancelled or rejected, or whose row is
// gone, keeps them held.

type deliveryRuns struct {
	t     *testing.T
	s     store.Store
	nonce string
	sess  string
}

func newDeliveryRuns(t *testing.T, s store.Store) *deliveryRuns {
	t.Helper()
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	sess, err := s.CreateSession(context.Background(), "whd_"+nonce, "x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &deliveryRuns{t: t, s: s, nonce: nonce, sess: sess.ID}
}

func (d *deliveryRuns) key(k string) string { return "webhook-event:" + d.nonce + ":" + k }

func (d *deliveryRuns) keys(ks ...string) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = d.key(k)
	}
	return out
}

// run opens a running run carrying idemKey ("" for none) and, unless status
// is running, finishes it so.
func (d *deliveryRuns) run(status store.RunStatus, idemKey string) string {
	d.t.Helper()
	ctx := context.Background()
	r, err := d.s.CreateRun(ctx, d.sess, store.RunIdentity{AgentID: "a_" + d.nonce + "_" + string(status) + idemKey, UserID: "alice", TenantID: "whd_" + d.nonce, IdempotencyKey: idemKey})
	if err != nil {
		d.t.Fatalf("CreateRun: %v", err)
	}
	if status != store.RunRunning {
		if err := d.s.FinishRun(ctx, r.ID, status, "", store.Usage{}, ""); err != nil {
			d.t.Fatalf("FinishRun(%s): %v", status, err)
		}
	}
	return r.ID
}

func (d *deliveryRuns) claim(now time.Time, ks ...string) (bool, string) {
	d.t.Helper()
	ok, holder, err := d.s.WebhookDeliveryClaimRun(context.Background(), d.keys(ks...), now, now.Add(time.Hour))
	if err != nil {
		d.t.Fatalf("WebhookDeliveryClaimRun(%v): %v", ks, err)
	}
	return ok, holder
}

func (d *deliveryRuns) setRun(runID string, ks ...string) {
	d.t.Helper()
	if err := d.s.WebhookDeliverySetRun(context.Background(), d.keys(ks...), runID); err != nil {
		d.t.Fatalf("WebhookDeliverySetRun: %v", err)
	}
}

// The claim is all or none, names the run that holds a refused key, and is
// taken over from a run that failed — all of that run's keys at once.
func testWebhookDeliveryClaimRunRetakesOnlyAFailedRunsKeys(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	if ok, _ := d.claim(armT0, "event", "body"); !ok {
		t.Fatal("the first claim was refused")
	}
	if ok, holder := d.claim(armT0, "other", "body"); ok || holder != "" {
		t.Fatalf("a claim sharing a key whose run has not registered: granted %t, holder %q; want refused, no holder", ok, holder)
	}
	running := d.run(store.RunRunning, d.key("run-body"))
	d.setRun(running, "event", "body")
	if ok, holder := d.claim(armT0, "other", "body"); ok || holder != running {
		t.Fatalf("a claim sharing a running run's key: granted %t, holder %q; want refused, holder %s", ok, holder, running)
	}
	if ok, _ := d.claim(armT0, "other"); !ok {
		t.Fatal("a refused claim wrote its other key")
	}

	if err := s.FinishRun(context.Background(), running, store.RunFailed, "", store.Usage{}, "lost"); err != nil {
		t.Fatal(err)
	}
	if ok, holder := d.claim(armT0, "event"); !ok {
		t.Fatalf("a failed run's key was not taken over (holder %q)", holder)
	}
	// Taking one key over drops every key of the failed run, and clears the
	// run's own, so the run that retakes the event can carry them.
	if ok, _ := d.claim(armT0, "body"); !ok {
		t.Error("the failed run's other key stayed held")
	}
	if _, found, err := s.RunByIdempotencyKey(context.Background(), d.key("run-body")); err != nil || found {
		t.Errorf("the failed run kept its idempotency key (found %t, %v)", found, err)
	}

	for _, st := range []store.RunStatus{store.RunCompleted, store.RunCancelled, store.RunRejected} {
		k := "held-" + string(st)
		id := d.run(st, d.key("run-"+k))
		if ok, _ := d.claim(armT0, k); !ok {
			t.Fatal(k)
		}
		d.setRun(id, k)
		if ok, holder := d.claim(armT0, k); ok || holder != id {
			t.Errorf("a %s run's key: granted %t, holder %q; want held by %s", st, ok, holder, id)
		}
		if _, found, err := s.RunByIdempotencyKey(context.Background(), d.key("run-"+k)); err != nil || !found {
			t.Errorf("a %s run lost its idempotency key (found %t, %v)", st, found, err)
		}
	}
	if ok, _ := d.claim(armT0, "gone"); !ok {
		t.Fatal("gone")
	}
	d.setRun("run_never_existed_"+d.nonce, "gone")
	if ok, _ := d.claim(armT0, "gone"); ok {
		t.Error("a key whose run row is gone was taken over")
	}
	if ok, _ := d.claim(armT0.Add(time.Hour), "gone"); !ok {
		t.Error("a key past its expiry stayed held")
	}
}

// Of many concurrent claims taking over one failed run's key, exactly one is
// granted.
func testWebhookDeliveryClaimRunRetakeRaceGrantsOne(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	failed := d.run(store.RunFailed, "")
	if ok, _ := d.claim(armT0, "event"); !ok {
		t.Fatal("the first claim was refused")
	}
	d.setRun(failed, "event")
	const racers = 8
	var wg sync.WaitGroup
	granted := make(chan bool, racers)
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := s.WebhookDeliveryClaimRun(context.Background(), d.keys("event"), armT0, armT0.Add(time.Hour))
			if err != nil {
				errs <- err
				return
			}
			granted <- ok
		}()
	}
	wg.Wait()
	close(granted)
	close(errs)
	for err := range errs {
		t.Errorf("a racing claim failed: %v", err)
	}
	n := 0
	for ok := range granted {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d of %d racing claims took the failed run's key over, want exactly 1", n, racers)
	}
}
