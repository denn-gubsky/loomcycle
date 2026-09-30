package channelhooks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// failRenewStore fails every lease renewal while fail is set, as a database
// failover or an exhausted pool would; every other call goes through.
type failRenewStore struct {
	store.Store
	fail atomic.Bool
	errs atomic.Int32
}

func (s *failRenewStore) ChannelHookRenew(ctx context.Context, key store.ChannelMessageKey, lease string, leaseUntil time.Time) (bool, error) {
	if s.fail.Load() {
		s.errs.Add(1)
		return false, errors.New("simulated database outage")
	}
	return s.Store.ChannelHookRenew(ctx, key, lease, leaseUntil)
}

// A job whose renewals keep failing stops once the lease the store last
// granted runs out — the store cannot tell it the lease is gone, but from
// that instant another claim may hold the message. The message is neither
// dropped nor delivered: it waits to be claimed again.
func TestWorker_RenewalsFailingPastTheLeaseStopTheJob(t *testing.T) {
	// The ask's own timeout is far beyond the test, so only a lost lease can
	// end the job.
	f, runs, _ := askFixture(t, time.Minute)
	fs := &failRenewStore{Store: f.st}
	f.w.cfg.Store = fs
	f.w.cfg.Lease = 300 * time.Millisecond
	hold, _ := answer(t, `{"decision":"hold","reason":"look"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
	done := make(chan struct{})
	go func() { f.w.claim(context.Background()); f.w.wg.Wait(); close(done) }()
	ask := pendingAsk(t, f.st, runs)

	fs.fail.Store(true)
	start := time.Now()
	select {
	case <-done:
		t.Logf("the job stopped %v after renewals began failing", time.Since(start))
	case <-time.After(5 * time.Second):
		t.Fatalf("the job still waits on its ask %d renewal failure(s) after its lease ran out", fs.errs.Load())
	}
	if row, _ := f.st.InterruptGet(context.Background(), ask.InterruptID); row.Status == store.InterruptStatusPending {
		t.Errorf("the stopped job's ask is still pending")
	}
	if n := f.w.Stats().Decisions["drop"]; n != 0 {
		t.Errorf("the stopped job dropped the message (%d drop decisions)", n)
	}
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 0 {
		t.Errorf("the stopped job delivered %d message(s)", got)
	}
	later := time.Now().Add(time.Second)
	if items, _ := f.st.ChannelHookClaim(context.Background(), "probe", later, later.Add(time.Minute), 10); len(items) != 1 {
		t.Errorf("the message no longer awaits a decision: %d claimable", len(items))
	}
}

// Renewals that succeed keep a job waiting on a person for as long as the
// person takes, many leases over.
func TestWorker_RenewedLeaseKeepsAWaitingJob(t *testing.T) {
	f, runs, bus := askFixture(t, time.Minute)
	f.w.cfg.Lease = 150 * time.Millisecond
	hold, _ := answer(t, `{"decision":"hold"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{}`, nil)
	wait := runAsync(f)
	ask := pendingAsk(t, f.st, runs)
	time.Sleep(4 * f.w.cfg.Lease)
	if row, _ := f.st.InterruptGet(context.Background(), ask.InterruptID); row.Status != store.InterruptStatusPending {
		t.Fatalf("the ask is %s after four renewed leases, want still pending", row.Status)
	}
	resolveAsk(t, f.st, bus, ask, "release")
	wait()
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 1 {
		t.Fatalf("delivered %d, want the released message", got)
	}
}

// waitFor polls cond until it holds or d passes, and reports whether it held.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// A hold waits on a person; renewals then fail for longer than the lease,
// and the SAME worker claims the message again. Before, both jobs passed
// every compare-and-set under the replica's shared owner: the new job
// cancelled the old one's ask, the old job read that as "nobody answered"
// and dropped the message, and the person's release on the new ask found
// nothing. Now the old job is stopped when its lease runs out and could not
// settle under the new claim anyway: the person's release delivers the
// message once, and nothing drops it.
func TestWorker_ASameReplicaReclaimAfterFailedRenewalsDeliversThePersonsRelease(t *testing.T) {
	f, runs, bus := askFixture(t, 5*time.Second)
	fs := &failRenewStore{Store: f.st}
	f.w.cfg.Store = fs
	f.w.cfg.Lease = 300 * time.Millisecond
	hold, calls := answer(t, `{"decision":"hold","reason":"look"}`)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", hold.URL))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
	wait := runAsync(f)
	first := pendingAsk(t, f.st, runs)

	fs.fail.Store(true) // the outage: every renewal errors
	time.Sleep(800 * time.Millisecond)
	// The same replica's next poll, the lease lapsed: a worker under the same
	// owner (its own WaitGroup, so the two jobs can overlap as they did).
	again := New(f.w.cfg)
	again.claim(context.Background())
	fs.fail.Store(false) // recovery

	var fresh store.InterruptRow
	if !waitFor(3*time.Second, func() bool {
		rows, _ := f.st.InterruptListByRun(context.Background(), first.RunID, store.InterruptStatusPending)
		for _, r := range rows {
			if r.InterruptID != first.InterruptID {
				fresh = r
				return true
			}
		}
		return false
	}) {
		t.Fatal("the new claim did not ask the person again")
	}
	if err := f.st.InterruptResolve(context.Background(), fresh.InterruptID, "release", store.InterruptResolvedByAPI, nil); err != nil {
		t.Fatalf("the person's release on the new ask was refused: %v", err)
	}
	bus.Notify("intr:" + fresh.InterruptID)
	wait()
	again.wg.Wait()
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 1 {
		t.Fatalf("delivered %d after the person released the message, want 1 (decisions %v then %v)",
			got, f.w.Stats().Decisions, again.Stats().Decisions)
	}
	if n := f.w.Stats().Decisions["drop"] + again.Stats().Decisions["drop"]; n != 0 {
		t.Errorf("a drop was decided %d time(s): the superseded job acted on the cancelled ask", n)
	}
	// Once per claim: the new claim runs the chain again (a hook call may
	// repeat), and the superseded job runs nothing more.
	if n := calls.Load(); n != 2 {
		t.Errorf("the hook was called %d times, want 2 (once per claim)", n)
	}
}

// A job whose claim was superseded — the lease lapsed and the same owner
// claimed the message again — is refused by every call that takes the lease:
// its renew reports the lease lost, its progress is not saved, and its drop
// and release change nothing. The current claim still settles the message.
func TestWorker_ASupersededClaimSettlesNothing(t *testing.T) {
	f := newFixture(t)
	f.setDef("", "inbox", Def{Hooks: chain(webhookEntry("gate", "https://unused.example"))})
	f.publish("inbox", "", store.MemoryScopeGlobal, `{"n":1}`, nil)
	ctx := context.Background()
	now := time.Now()
	old, _ := f.st.ChannelHookClaim(ctx, f.w.cfg.Owner, now, now.Add(time.Millisecond), 1)
	if len(old) != 1 {
		t.Fatal("first claim")
	}
	later := now.Add(time.Second)
	cur, _ := f.st.ChannelHookClaim(ctx, f.w.cfg.Owner, later, later.Add(time.Hour), 1)
	if len(cur) != 1 {
		t.Fatal("the same owner did not claim the message again")
	}

	var lostCalls atomic.Int32
	lost := func() { lostCalls.Add(1) }
	j := &job{w: f.w, msg: old[0].Message, key: keyOf(old[0].Message), lease: old[0].Lease, progress: old[0].Progress, lost: lost}
	f.w.cfg.Lease = 30 * time.Millisecond
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { j.renew(rctx, lost, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("the superseded job's renew did not report its lease lost")
	}
	cancel()
	if lostCalls.Load() != 1 {
		t.Fatalf("lost called %d times by the renew, want 1", lostCalls.Load())
	}
	if err := j.saveProgress(ctx, 1, nil, 0, time.Time{}, ""); err == nil || lostCalls.Load() != 2 {
		t.Errorf("the superseded job saved progress: err=%v, lost calls %d", err, lostCalls.Load())
	}
	j.drop(ctx, nil, "superseded", "gate")
	j.release(ctx, nil, j.body(), false)
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 0 {
		t.Fatalf("the superseded job delivered %d message(s)", got)
	}
	if ok, err := f.st.ChannelReleaseHookHeld(ctx, keyOf(cur[0].Message), cur[0].Lease, nil, time.Time{}); err != nil || !ok {
		t.Fatalf("the current claim could not release the message (the superseded job dropped it?): ok=%v err=%v", ok, err)
	}
	if got := len(f.peek("inbox", "", store.MemoryScopeGlobal)); got != 1 {
		t.Fatalf("delivered %d, want the message the current claim released", got)
	}
}
