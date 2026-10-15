package concurrency

import (
	"context"
	"errors"
	"testing"
)

// TestSemaphore_AWaiterThatGivesUpAfterTheHandOffPassesTheSlotOn — a waiter
// handed a slot as it gave up (its client left, its OnQueued failed) gives the
// slot to the next waiter. Before the fix cancelWaiter only took the slot back:
// it sat idle with runs still queued until some unrelated release or new run.
func TestSemaphore_AWaiterThatGivesUpAfterTheHandOffPassesTheSlotOn(t *testing.T) {
	s := New(1, 8, 0)
	running, err := s.Admit(context.Background(), AdmitRequest{UserID: "a"})
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	// B waits ahead of C: already-admitted work is served first.
	b := &semWaiter{ch: make(chan struct{}, 1)}
	s.mu.Lock()
	s.admitted = append(s.admitted, b)
	s.queued++
	s.mu.Unlock()
	outC, infos := admitAsync(t, context.Background(), s, AdmitRequest{UserID: "c"})
	recvInfo(t, infos)

	running()                         // the slot goes to B ...
	s.cancelWaiter(b, userKey{}, nil) // ... which has given up meanwhile

	r := recvResult(t, outC)
	if r.err != nil {
		t.Fatalf("C: %v", r.err)
	}
	r.release()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Fatalf("after C ended: %+v, want all zero", st)
	}
}

// TestAdmit_ARunMovedOnFromItsUserCapIsNeverRefusedAsQueueFull — a run that
// waited on its user's cap keeps its place in the queue when it moves on to
// wait for a slot. Before the fix it gave the place up first, so a run arriving
// in between could fill the queue and the run that had already queued (and been
// reported queued to its caller) was refused "queue full". The window is
// between two goroutines, so the test repeats it.
func TestAdmit_ARunMovedOnFromItsUserCapIsNeverRefusedAsQueueFull(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := New(2, 1, 0).WithPerUserCap(1).WithQueuedPerUserCap(1)
		other, err := s.Admit(context.Background(), AdmitRequest{TenantID: "t", UserID: "v"})
		if err != nil {
			t.Fatalf("iteration %d: other: %v", i, err)
		}
		first, err := s.Admit(context.Background(), AdmitRequest{TenantID: "t", UserID: "u"})
		if err != nil {
			t.Fatalf("iteration %d: first: %v", i, err)
		}
		out, infos := admitAsync(t, context.Background(), s, AdmitRequest{TenantID: "t", UserID: "u"})
		recvInfo(t, infos)

		first() // the waiting run moves on; new runs race it for the slot and the queue
		lateCtx, cancelLate := context.WithCancel(context.Background())
		var late []chan error
		for _, u := range []string{"w", "x"} {
			c := make(chan error, 1)
			go func(u string) {
				rel, err := s.Admit(lateCtx, AdmitRequest{TenantID: "t", UserID: u})
				if err == nil {
					rel() // a short run: the waiting run gets its slot after it
				}
				c <- err
			}(u)
			late = append(late, c)
		}
		r := recvResult(t, out)
		cancelLate()
		if r.err != nil {
			var bp *BackpressureError
			if errors.As(r.err, &bp) {
				t.Fatalf("iteration %d: the run that waited on its user's cap was refused: %v", i, r.err)
			}
			t.Fatalf("iteration %d: waiting run: %v", i, r.err)
		}
		r.release()
		for _, c := range late {
			<-c
		}
		other()
		if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
			t.Fatalf("iteration %d: after all ended: %+v, want all zero", i, st)
		}
	}
}
