package concurrency

import (
	"context"
	"errors"
	"testing"
	"time"
)

// admitAsync runs Admit on its own goroutine and returns its outcome channel
// and a channel of the QueueInfos its OnQueued saw. It waits until the run has
// queued at least once, so callers know where it stands.
type admitResult struct {
	release func()
	err     error
}

func admitAsync(t *testing.T, ctx context.Context, s *Semaphore, r AdmitRequest) (<-chan admitResult, <-chan QueueInfo) {
	t.Helper()
	infos := make(chan QueueInfo, 4)
	r.OnQueued = func(q QueueInfo) error {
		infos <- q
		return nil
	}
	out := make(chan admitResult, 1)
	go func() {
		rel, err := s.Admit(ctx, r)
		out <- admitResult{rel, err}
	}()
	return out, infos
}

func recvInfo(t *testing.T, c <-chan QueueInfo) QueueInfo {
	t.Helper()
	select {
	case q := <-c:
		return q
	case <-time.After(2 * time.Second):
		t.Fatal("run never reported that it was queued")
		return QueueInfo{}
	}
}

func recvResult(t *testing.T, c <-chan admitResult) admitResult {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("Admit did not return")
		return admitResult{}
	}
}

func assertWaiting(t *testing.T, c <-chan admitResult) {
	t.Helper()
	select {
	case r := <-c:
		t.Fatalf("Admit returned (err %v) while it should still wait", r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestAdmit_OverUserCapWaitsForTheUsersRun — a run over its user's cap waits
// (reported as user_cap) instead of being refused, and starts when one of that
// user's runs ends.
func TestAdmit_OverUserCapWaitsForTheUsersRun(t *testing.T) {
	s := New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4)
	first, err := s.Admit(context.Background(), AdmitRequest{TenantID: "t", UserID: "u"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	out, infos := admitAsync(t, context.Background(), s, AdmitRequest{TenantID: "t", UserID: "u", Ticket: "r2"})
	if q := recvInfo(t, infos); q != (QueueInfo{Reason: QueueReasonUserCap, Position: 1}) {
		t.Fatalf("queued as %+v, want user_cap position 1", q)
	}
	if q, ok := s.Position("r2"); !ok || q.Reason != QueueReasonUserCap {
		t.Fatalf("Position(r2) = %+v %v, want user_cap", q, ok)
	}
	assertWaiting(t, out)

	// Another user is not held back by it.
	other, err := s.Admit(context.Background(), AdmitRequest{TenantID: "t", UserID: "v"})
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	other()

	first()
	r := recvResult(t, out)
	if r.err != nil {
		t.Fatalf("waiting run: %v", r.err)
	}
	if _, ok := s.Position("r2"); ok {
		t.Fatal("Position still reports a run that started")
	}
	r.release()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
		t.Fatalf("after release: %+v, want all zero", st)
	}
}

// TestAdmit_UserWaitingBoundRefuses — past WithQueuedPerUserCap waiting runs,
// and with no waiting configured, a run over the cap is refused as before.
func TestAdmit_UserWaitingBoundRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waiting int
	}{{"no waiting configured", 0}, {"bound reached", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(tc.waiting)
			first, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			defer first()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for i := 0; i < tc.waiting; i++ {
				_, infos := admitAsync(t, ctx, s, AdmitRequest{UserID: "u"})
				recvInfo(t, infos)
			}
			_, err = s.Admit(context.Background(), AdmitRequest{UserID: "u"})
			if !IsPerUserQuotaExhausted(err) {
				t.Fatalf("err = %v, want per-user quota exhausted", err)
			}
		})
	}
}

// TestAdmit_RejectNeverWaits — admission "reject" refuses at once over the
// user's cap and when no slot is free, and leaves the counts as they were.
func TestAdmit_RejectNeverWaits(t *testing.T) {
	s := New(1, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4)
	held, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(context.Background(), AdmitRequest{UserID: "u", Reject: true}); !IsPerUserQuotaExhausted(err) {
		t.Fatalf("over the user cap: err = %v, want per-user quota exhausted", err)
	}
	if _, err := s.Admit(context.Background(), AdmitRequest{UserID: "v", Reject: true}); !IsBackpressure(err) {
		t.Fatalf("no free slot: err = %v, want backpressure", err)
	}
	st := s.Stats()
	if st.Active != 1 || st.Queued != 0 || st.PerUser["u"] != 1 || st.PerUser["v"] != 0 {
		t.Fatalf("after refusals: %+v, want only u's one run", st)
	}
	held()
}

// TestAdmit_NeverTimesOut — the semaphore's queue timeout bounds Acquire, not
// Admit: a queued run waits past it.
func TestAdmit_NeverTimesOut(t *testing.T) {
	s := New(1, 8, 10*time.Millisecond)
	held, err := s.Admit(context.Background(), AdmitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out, infos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: "u"})
	if q := recvInfo(t, infos); q.Reason != QueueReasonGlobalCap || q.Position != 1 {
		t.Fatalf("queued as %+v, want global_cap position 1", q)
	}
	select {
	case r := <-out:
		t.Fatalf("Admit returned after the queue timeout: %v", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	held()
	r := recvResult(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.release()
}

// TestAdmit_UserCapThenGlobalCapReportsBoth — a run that waits on its user's
// cap and then finds every slot taken reports both waits.
func TestAdmit_UserCapThenGlobalCapReportsBoth(t *testing.T) {
	s := New(2, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4)
	mine, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.Admit(context.Background(), AdmitRequest{UserID: "v"})
	if err != nil {
		t.Fatal(err)
	}
	out, infos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: "u"})
	if q := recvInfo(t, infos); q.Reason != QueueReasonUserCap {
		t.Fatalf("first wait %+v, want user_cap", q)
	}
	// v queues for the slot u's first run will free, before u's run moves on.
	vOut, vInfos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: "w"})
	recvInfo(t, vInfos)
	mine()
	// The freed slot goes to w, who was queued for one; u's run is now under
	// its cap but finds no slot.
	if q := recvInfo(t, infos); q.Reason != QueueReasonGlobalCap {
		t.Fatalf("second wait %+v, want global_cap", q)
	}
	w := recvResult(t, vOut)
	w.release()
	r := recvResult(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.release()
	theirs()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
		t.Fatalf("after all releases: %+v, want all zero", st)
	}
}

// TestAdmit_PositionFollowsTheRoundRobin — a queued run's position is where
// the round-robin will serve it, not its arrival order.
func TestAdmit_PositionFollowsTheRoundRobin(t *testing.T) {
	s := New(1, 16, 0)
	held, err := s.Admit(context.Background(), AdmitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, r := range []AdmitRequest{
		{UserID: "a", Ticket: "a1"}, {UserID: "a", Ticket: "a2"}, {UserID: "a", Ticket: "a3"}, {UserID: "b", Ticket: "b1"},
	} {
		_, infos := admitAsync(t, ctx, s, r)
		recvInfo(t, infos)
	}
	for ticket, want := range map[string]int{"a1": 1, "b1": 2, "a2": 3, "a3": 4} {
		q, ok := s.Position(ticket)
		if !ok || q.Position != want {
			t.Errorf("Position(%s) = %+v %v, want %d", ticket, q, ok, want)
		}
	}
	cancel()
	held()
}

// TestAdmit_OnQueuedErrorWithdrawsTheRun — an OnQueued failure (the run row
// could not be written) ends the wait with that error and leaves no count.
func TestAdmit_OnQueuedErrorWithdrawsTheRun(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		s    func() *Semaphore
	}{
		{"global cap", func() *Semaphore { return New(1, 8, 0) }},
		{"user cap", func() *Semaphore { return New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(2) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s()
			held, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Admit(context.Background(), AdmitRequest{UserID: "u", Ticket: "x", OnQueued: func(QueueInfo) error { return boom }})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the OnQueued error", err)
			}
			if _, ok := s.Position("x"); ok {
				t.Fatal("withdrawn run still has a position")
			}
			held()
			if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
				t.Fatalf("after release: %+v, want all zero", st)
			}
		})
	}
}

// TestAdmit_CancelWhileWaitingOnUserCapBalances — a run cancelled while it
// waits on its user's cap leaves no count, and the user's next waiting run
// still moves on.
func TestAdmit_CancelWhileWaitingOnUserCapBalances(t *testing.T) {
	s := New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4)
	held, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gone, infos := admitAsync(t, ctx, s, AdmitRequest{UserID: "u"})
	recvInfo(t, infos)
	next, nextInfos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: "u"})
	recvInfo(t, nextInfos)
	cancel()
	if r := recvResult(t, gone); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled run: %v, want context.Canceled", r.err)
	}
	held()
	r := recvResult(t, next)
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.release()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
		t.Fatalf("after release: %+v, want all zero", st)
	}
}

// TestAdmit_CancelRacingAWakeBalances — many cancels racing the wake of a
// run waiting on its user's cap never strand a per-user count.
func TestAdmit_CancelRacingAWakeBalances(t *testing.T) {
	s := New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(1)
	for i := 0; i < 200; i++ {
		held, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		out, infos := admitAsync(t, ctx, s, AdmitRequest{UserID: "u"})
		recvInfo(t, infos)
		go held()
		cancel()
		if r := recvResult(t, out); r.err == nil {
			r.release()
		}
		deadline := time.Now().Add(2 * time.Second)
		for st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0; st = s.Stats() {
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: %+v, want all zero", i, st)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// TestAdmit_ClusterModePollsTheSharedCount — in cluster mode a run over the
// cap polls the shared counter and starts once another replica's run ends.
func TestAdmit_ClusterModePollsTheSharedCount(t *testing.T) {
	gate := newStubQuotaGate()
	gate.atCapFor["u"] = true
	s := New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(2).WithUserQuotaStore(gate)
	s.userPoll = 5 * time.Millisecond
	out, infos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: "u", Ticket: "c1"})
	if q := recvInfo(t, infos); q.Reason != QueueReasonUserCap {
		t.Fatalf("queued as %+v, want user_cap", q)
	}
	if _, ok := s.Position("c1"); !ok {
		t.Fatal("Position does not report the polling run")
	}
	assertWaiting(t, out)
	gate.mu.Lock()
	gate.atCapFor["u"] = false
	gate.mu.Unlock()
	r := recvResult(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if gate.count("u") != 1 {
		t.Fatalf("shared count = %d, want 1", gate.count("u"))
	}
	r.release()
	if st := s.Stats(); st.Queued != 0 {
		t.Fatalf("queued = %d after start, want 0", st.Queued)
	}
}
