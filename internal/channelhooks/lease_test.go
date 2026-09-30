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
