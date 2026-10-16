package concurrency

import (
	"context"
	"testing"
)

// TestSemaphore_SetCapsRaisedAdmitsTheRunsAlreadyWaiting — a raised cap starts
// the runs already queued at once, ahead of a run that arrives after the
// reload. Before the fix SetCaps woke nobody: a new run took the new slot while
// the queued one waited for a release, and each release still woke only one.
func TestSemaphore_SetCapsRaisedAdmitsTheRunsAlreadyWaiting(t *testing.T) {
	s := New(1, 8, 0)
	first, err := s.Admit(context.Background(), AdmitRequest{UserID: "a"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	defer first()
	var waiting []<-chan admitResult
	for _, u := range []string{"b", "c"} {
		out, infos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: u})
		recvInfo(t, infos)
		waiting = append(waiting, out)
	}

	s.SetCaps(3, 8)

	for i, out := range waiting {
		r := recvResult(t, out)
		if r.err != nil {
			t.Fatalf("queued run %d: %v", i, r.err)
		}
		defer r.release()
	}
	if st := s.Stats(); st.Active != 3 || st.Queued != 0 {
		t.Fatalf("after raising the cap to 3: %+v, want 3 active, 0 queued", st)
	}
}

// TestSemaphore_SetCapsLoweredHoldsWhileRunsAreWaiting — a lowered cap holds
// even with runs queued: a release while more than the new cap still run starts
// nobody. Before the fix every release handed its slot straight to a waiter, so
// under a steady queue the old cap stayed in force.
func TestSemaphore_SetCapsLoweredHoldsWhileRunsAreWaiting(t *testing.T) {
	s := New(3, 8, 0)
	var running []func()
	for i := 0; i < 3; i++ {
		rel, err := s.Admit(context.Background(), AdmitRequest{})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		running = append(running, rel)
	}
	out, infos := admitAsync(t, context.Background(), s, AdmitRequest{})
	recvInfo(t, infos)

	s.SetCaps(1, 8)
	running[0]()
	running[1]()
	assertWaiting(t, out)
	if st := s.Stats(); st.Active != 1 || st.Queued != 1 {
		t.Fatalf("two of three ended under a cap of 1: %+v, want 1 active, 1 queued", st)
	}

	running[2]()
	r := recvResult(t, out)
	if r.err != nil {
		t.Fatalf("queued run: %v", r.err)
	}
	r.release()
}

// TestSemaphore_PerUserCapRaisedOrRemovedMovesItsWaitersOn — runs waiting on
// their user's cap move on when a reload raises or removes it, without waiting
// for one of the user's runs to end. Before the fix nothing woke them, and with
// the cap removed (0) not even a release did: `perUser >= 0` always held.
func TestSemaphore_PerUserCapRaisedOrRemovedMovesItsWaitersOn(t *testing.T) {
	for _, newCap := range []int{2, 0} {
		s := New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4)
		first, err := s.Admit(context.Background(), AdmitRequest{TenantID: "t", UserID: "u"})
		if err != nil {
			t.Fatalf("cap %d: first: %v", newCap, err)
		}
		out, infos := admitAsync(t, context.Background(), s, AdmitRequest{TenantID: "t", UserID: "u"})
		if q := recvInfo(t, infos); q.Reason != QueueReasonUserCap {
			t.Fatalf("cap %d: queued as %+v, want user_cap", newCap, q)
		}

		s.WithPerUserCap(newCap)

		r := recvResult(t, out)
		if r.err != nil {
			t.Fatalf("cap %d: waiting run: %v", newCap, r.err)
		}
		r.release()
		first()
		if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
			t.Fatalf("cap %d: after both ended: %+v, want all zero", newCap, st)
		}
	}
}
