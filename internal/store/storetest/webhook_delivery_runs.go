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
	// ids is each granted claim's id, by its first key.
	ids map[string]string
}

func newDeliveryRuns(t *testing.T, s store.Store) *deliveryRuns {
	t.Helper()
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	sess, err := s.CreateSession(context.Background(), "whd_"+nonce, "x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &deliveryRuns{t: t, s: s, nonce: nonce, sess: sess.ID, ids: map[string]string{}}
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
	return d.runWith(status, store.RunIdentity{IdempotencyKey: idemKey})
}

// runWith is run with the delivery keys of id (the rest is filled in).
func (d *deliveryRuns) runWith(status store.RunStatus, id store.RunIdentity) string {
	d.t.Helper()
	ctx := context.Background()
	id.AgentID = "a_" + d.nonce + "_" + string(status) + id.IdempotencyKey + id.DeliveryAltKey
	id.UserID, id.TenantID = "alice", "whd_"+d.nonce
	r, err := d.s.CreateRun(ctx, d.sess, id)
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
	r := d.claimLinked(now, "", "", ks...)
	return r.Claimed, r.Holder
}

// claimLinked claims ks for a delivery whose run will carry runKey/runAltKey.
func (d *deliveryRuns) claimLinked(now time.Time, runKey, runAltKey string, ks ...string) store.WebhookRunClaimResult {
	d.t.Helper()
	r, err := d.s.WebhookDeliveryClaimRun(context.Background(), store.WebhookRunClaim{
		Keys: d.keys(ks...), Now: now, ExpiresAt: now.Add(time.Hour), RunKey: runKey, RunAltKey: runAltKey})
	if err != nil {
		d.t.Fatalf("WebhookDeliveryClaimRun(%v): %v", ks, err)
	}
	if r.Claimed {
		if r.ClaimID == "" {
			d.t.Fatalf("WebhookDeliveryClaimRun(%v): granted with no claim id", ks)
		}
		d.ids[ks[0]] = r.ClaimID
	}
	return r
}

// setRun links runID to the claim granted with first key k.
func (d *deliveryRuns) setRun(runID, k string) {
	d.t.Helper()
	if ok := d.link(d.ids[k], runID); !ok {
		d.t.Fatalf("WebhookDeliveryLinkRun(%s, %s): the claim is not current", k, runID)
	}
}

func (d *deliveryRuns) link(claimID, runID string) bool {
	d.t.Helper()
	ok, err := d.s.WebhookDeliveryLinkRun(context.Background(), claimID, runID)
	if err != nil {
		d.t.Fatalf("WebhookDeliveryLinkRun: %v", err)
	}
	return ok
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
	d.setRun(running, "event")
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

// The read that answers a repeated duplicate agrees with the claim: a key is
// held by a live run exactly when the claim would refuse it naming that run.
func testWebhookDeliveryHeldByLiveRunAgreesWithTheClaim(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	ctx := context.Background()
	heldBy := func(now time.Time, k string) (string, bool) {
		t.Helper()
		id, held, err := s.WebhookDeliveryHeldByLiveRun(ctx, d.key(k), now)
		if err != nil {
			t.Fatalf("WebhookDeliveryHeldByLiveRun(%s): %v", k, err)
		}
		return id, held
	}
	if _, held := heldBy(armT0, "none"); held {
		t.Error("a key never claimed reads as held")
	}
	if ok, _ := d.claim(armT0, "unregistered"); !ok {
		t.Fatal("unregistered")
	}
	if _, held := heldBy(armT0, "unregistered"); held {
		t.Error("a key whose run has not registered reads as held by a live run")
	}
	for _, st := range []store.RunStatus{store.RunRunning, store.RunCompleted, store.RunCancelled} {
		k := "live-" + string(st)
		id := d.run(st, "")
		if ok, _ := d.claim(armT0, k); !ok {
			t.Fatal(k)
		}
		d.setRun(id, k)
		if got, held := heldBy(armT0, k); !held || got != id {
			t.Errorf("a %s run's key: held %t by %q, want held by %s", st, held, got, id)
		}
		if _, held := heldBy(armT0.Add(time.Hour), k); held {
			t.Errorf("a %s run's key past its expiry reads as held", st)
		}
	}
	failed := d.run(store.RunFailed, "")
	if ok, _ := d.claim(armT0, "failed"); !ok {
		t.Fatal("failed")
	}
	d.setRun(failed, "failed")
	if _, held := heldBy(armT0, "failed"); held {
		t.Error("a failed run's key reads as held: the claim takes it over")
	}
	if ok, _ := d.claim(armT0, "gone"); !ok {
		t.Fatal("gone")
	}
	d.setRun("run_never_existed_"+d.nonce, "gone")
	if _, held := heldBy(armT0, "gone"); held {
		t.Error("a key whose run row is gone reads as held by a live run")
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
			r, err := s.WebhookDeliveryClaimRun(context.Background(), store.WebhookRunClaim{Keys: d.keys("event"), Now: armT0, ExpiresAt: armT0.Add(time.Hour)})
			if err != nil {
				errs <- err
				return
			}
			granted <- r.Claimed
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

// pastGrace is a time past the grace of a claim made at armT0.
var pastGrace = armT0.Add(store.WebhookUnregisteredClaimGrace + time.Second)

// A claim whose run never linked to it is its delivery's while the grace
// lasts. Past it, with no run carrying its run keys, the event's next
// delivery takes it over under a new claim id — every key of the old claim
// dropped — and the old setup, linking late, finds its claim gone: it is
// fenced out, and so is its release.
func testWebhookDeliveryClaimRunFencesOutASetupThatOutlivesTheGrace(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	first := d.claimLinked(armT0, d.key("run-a"), "", "event", "body-a")
	if !first.Claimed {
		t.Fatal("the first claim was refused")
	}
	if r := d.claimLinked(armT0.Add(store.WebhookUnregisteredClaimGrace-time.Second), d.key("run-b"), "", "event", "body-b"); r.Claimed || r.Holder != "" {
		t.Fatalf("a claim inside the grace: %+v, want refused with no holder", r)
	}
	second := d.claimLinked(pastGrace, d.key("run-b"), "", "event", "body-b")
	if !second.Claimed || second.ClaimID == first.ClaimID {
		t.Fatalf("a claim past the grace with no run carrying the first's keys: %+v, want granted under a new id", second)
	}
	if ok, _ := d.claim(pastGrace, "body-a"); !ok {
		t.Error("the stale claim's other key stayed held")
	}
	late := d.run(store.RunRunning, d.key("run-a"))
	if d.link(first.ClaimID, late) {
		t.Error("the stale setup linked its run to a claim that was taken over")
	}
	if err := s.WebhookDeliveryReleaseClaim(context.Background(), first.ClaimID); err != nil {
		t.Fatal(err)
	}
	if r := d.claimLinked(pastGrace, "", "", "event"); r.Claimed {
		t.Error("the stale setup's release dropped the claim that took it over")
	}
	winner := d.run(store.RunRunning, d.key("run-b"))
	if !d.link(second.ClaimID, winner) {
		t.Fatal("the current claim did not link its run")
	}
	if r := d.claimLinked(pastGrace, "", "", "event"); r.Claimed || r.Holder != winner {
		t.Errorf("after the link: %+v, want held by %s", r, winner)
	}
	// A claim links once: another run is refused, the same one is not.
	if d.link(second.ClaimID, late) {
		t.Error("a claim linked to one run took another")
	}
	if !d.link(second.ClaimID, winner) {
		t.Error("linking the same run again was refused")
	}
	// A claim released by its own delivery is gone.
	rel := d.claimLinked(pastGrace, "", "", "released")
	if err := s.WebhookDeliveryReleaseClaim(context.Background(), rel.ClaimID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.claim(pastGrace, "released"); !ok {
		t.Error("a released claim stayed held")
	}
}

// Past the grace, a claim whose run registered but was never linked to it is
// found by the run keys it recorded and linked to that run (adopted): the next
// delivery is a duplicate of it, not a second run. A run carrying them as its
// delivery_alt_key counts too. When that run failed, the event is taken over
// and the run's keys cleared.
func testWebhookDeliveryClaimRunAdoptsTheRunItsClaimStarted(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	first := d.claimLinked(armT0, d.key("run-a"), d.key("alt-a"), "event", "body-a")
	if !first.Claimed {
		t.Fatal("the first claim was refused")
	}
	orphan := d.runWith(store.RunRunning, store.RunIdentity{DeliveryAltKey: d.key("alt-a")})
	if r := d.claimLinked(pastGrace, d.key("run-b"), "", "event", "body-b"); r.Claimed || r.Holder != orphan {
		t.Fatalf("a claim past the grace whose run registered unlinked: %+v, want refused, held by %s", r, orphan)
	}
	if !d.link(first.ClaimID, orphan) {
		t.Error("the adopted claim refused its own run")
	}
	if ok, _ := d.claim(pastGrace, "body-a"); ok {
		t.Error("an adopted claim lost its other key")
	}

	failed := d.claimLinked(armT0, d.key("run-f"), "", "failed-event")
	if !failed.Claimed {
		t.Fatal("failed-event")
	}
	d.run(store.RunFailed, d.key("run-f"))
	if r := d.claimLinked(pastGrace, d.key("run-g"), "", "failed-event"); !r.Claimed {
		t.Fatalf("a claim past the grace whose unlinked run failed: %+v, want granted", r)
	}
	if _, found, err := s.RunByIdempotencyKey(context.Background(), d.key("run-f")); err != nil || found {
		t.Errorf("the failed run kept its idempotency key (found %t, %v)", found, err)
	}
}

// A key held with no claim id — a channel delivery's, or one written before
// claims had ids — is never looked at for its age.
func testWebhookDeliveryClaimRunNeverTakesOverAKeyWithoutAClaimID(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	if ok, err := s.WebhookDeliveryClaim(context.Background(), d.keys("chan"), armT0, armT0.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("WebhookDeliveryClaim: %t, %v", ok, err)
	}
	if r := d.claimLinked(pastGrace, "", "", "chan"); r.Claimed {
		t.Error("a key with no claim id was taken over for its age")
	}
}

// Of many deliveries taking over one stale claim at once, exactly one is
// granted; the rest are refused against the winner's claim.
func testWebhookDeliveryClaimRunStaleTakeoverRaceGrantsOne(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	if r := d.claimLinked(armT0, d.key("run-a"), "", "event"); !r.Claimed {
		t.Fatal("the first claim was refused")
	}
	const racers = 8
	var wg sync.WaitGroup
	granted := make(chan bool, racers)
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.WebhookDeliveryClaimRun(context.Background(), store.WebhookRunClaim{
				Keys: d.keys("event", fmt.Sprintf("body-%d", i)), Now: pastGrace, ExpiresAt: pastGrace.Add(time.Hour),
				RunKey: d.key(fmt.Sprintf("run-%d", i))})
			if err != nil {
				errs <- err
				return
			}
			granted <- r.Claimed
		}(i)
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
		t.Errorf("%d of %d racing claims took the stale claim over, want exactly 1", n, racers)
	}
}

// A run that failed gives its delivery keys back; a run in any other status
// keeps them.
func testRunReleaseFailedDeliveryKeysClearsOnlyAFailedRuns(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	ctx := context.Background()
	d.runWith(store.RunFailed, store.RunIdentity{IdempotencyKey: d.key("f-key"), DeliveryAltKey: d.key("f-alt")})
	running := d.runWith(store.RunRunning, store.RunIdentity{IdempotencyKey: d.key("r-key")})
	if err := s.RunReleaseFailedDeliveryKeys(ctx, []string{d.key("f-alt"), d.key("r-key")}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.RunByIdempotencyKey(ctx, d.key("f-key")); err != nil || found {
		t.Errorf("the failed run kept its idempotency key (found %t, %v)", found, err)
	}
	if _, found, err := s.RunByDeliveryKeys(ctx, []string{d.key("f-alt")}); err != nil || found {
		t.Errorf("the failed run kept its alt key (found %t, %v)", found, err)
	}
	if r, found, err := s.RunByIdempotencyKey(ctx, d.key("r-key")); err != nil || !found || r.ID != running {
		t.Errorf("the running run lost its key (found %t, %v)", found, err)
	}
	// A new run can carry the released keys.
	d.runWith(store.RunRunning, store.RunIdentity{IdempotencyKey: d.key("f-key"), DeliveryAltKey: d.key("f-alt")})
}

// A definition's lifetime can be shorter than its run's setup. A claim with
// no run linked is not swept at its expiry while its grace lasts, so the
// setup still finds it; once linked, it expires as usual.
func testWebhookDeliveryClaimRunSparesASetupFromItsExpiry(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	ctx := context.Background()
	short, err := s.WebhookDeliveryClaimRun(ctx, store.WebhookRunClaim{Keys: d.keys("event"), Now: armT0, ExpiresAt: armT0.Add(time.Second)})
	if err != nil || !short.Claimed {
		t.Fatalf("the first claim: %+v, %v", short, err)
	}
	if r := d.claimLinked(armT0.Add(time.Minute), "", "", "event"); r.Claimed {
		t.Fatal("a claim in its setup was swept at its expiry")
	}
	id := d.run(store.RunRunning, "")
	if !d.link(short.ClaimID, id) {
		t.Fatal("the setup did not find its claim")
	}
	if r := d.claimLinked(armT0.Add(2*time.Minute), "", "", "event"); !r.Claimed {
		t.Errorf("a linked claim past its expiry stayed held: %+v", r)
	}
}

// A late link and a takeover of the same stale claim race: never both win.
// Either the link lands first and the takeover meets a linked claim, or the
// takeover lands first and the link finds its claim gone.
func testWebhookDeliveryClaimRunLinkAndTakeoverNeverBothWin(t *testing.T, s store.Store) {
	d := newDeliveryRuns(t, s)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		event := fmt.Sprintf("event-%d", i)
		first := d.claimLinked(armT0, d.key("run-"+event), "", event)
		if !first.Claimed {
			t.Fatal(event)
		}
		late := d.run(store.RunRunning, "") // carries none of the claim's run keys
		var wg sync.WaitGroup
		var linked bool
		var taken store.WebhookRunClaimResult
		var linkErr, takeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			linked, linkErr = s.WebhookDeliveryLinkRun(ctx, first.ClaimID, late)
		}()
		go func() {
			defer wg.Done()
			taken, takeErr = s.WebhookDeliveryClaimRun(ctx, store.WebhookRunClaim{
				Keys: d.keys(event, "body-"+event), Now: pastGrace, ExpiresAt: pastGrace.Add(time.Hour)})
		}()
		wg.Wait()
		if linkErr != nil || takeErr != nil {
			t.Fatalf("%s: link %v, takeover %v", event, linkErr, takeErr)
		}
		if linked == taken.Claimed {
			t.Errorf("%s: linked %t, taken over %t — want exactly one", event, linked, taken.Claimed)
		}
		if linked && taken.Holder != late {
			t.Errorf("%s: the link won but the takeover was refused naming %q, want %s", event, taken.Holder, late)
		}
	}
}
