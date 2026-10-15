package concurrency

import (
	"context"
	"testing"
	"time"
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

// TestAdmit_ARunMovedOnFromItsUserCapKeepsItsPlaceInTheQueue — a run that
// waited on its user's cap keeps its place in the queue while it moves on to
// wait for a slot. Before the fix it gave the place up first, so a run that
// arrived in between could fill the queue and the run that had already queued
// (and been reported queued to its caller) was refused "queue full". The two
// stages are driven one at a time here, so the window between them is held open
// instead of raced for.
func TestAdmit_ARunMovedOnFromItsUserCapKeepsItsPlaceInTheQueue(t *testing.T) {
	s := New(1, 1, 0).WithPerUserCap(1).WithQueuedPerUserCap(1)
	key := userKey{tenant: "t", user: "u"}
	first, err := s.Admit(context.Background(), AdmitRequest{TenantID: "t", UserID: "u"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	queued := make(chan struct{}, 1)
	type userResult struct {
		waited bool
		err    error
	}
	moved := make(chan userResult, 1)
	go func() {
		waited, err := s.admitUserLocal(context.Background(), key, 1, AdmitRequest{OnQueued: func(QueueInfo) error {
			queued <- struct{}{}
			return nil
		}})
		moved <- userResult{waited, err}
	}()
	<-queued

	first() // the waiting run is counted against its user and moves on ...
	if r := <-moved; r.err != nil || !r.waited {
		t.Fatalf("moving on from the user cap: waited=%v err=%v, want waited", r.waited, r.err)
	}
	// ... and before it asks for a slot, a new run takes the free one and
	// another finds the queue full: the place is still the waiting run's.
	other, err := s.Admit(context.Background(), AdmitRequest{UserID: "w"})
	if err != nil {
		t.Fatalf("new run taking the free slot: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Admit(ctx, AdmitRequest{UserID: "x"}); !IsBackpressure(err) {
		t.Fatalf("a run arriving with the queue's one place taken: got %v, want queue full", err)
	}

	got := make(chan admitResult, 1)
	go func() {
		rel, err := s.admitGlobal(context.Background(), key, key, nil, AdmitRequest{}, true)
		got <- admitResult{rel, err}
	}()
	assertWaiting(t, got)
	other()
	r := recvResult(t, got)
	if r.err != nil {
		t.Fatalf("the run that waited on its user's cap: %v", r.err)
	}
	r.release()
	if st := s.Stats(); st.Active != 0 || st.Queued != 0 || len(st.PerUser) != 0 {
		t.Fatalf("after all ended: %+v, want all zero", st)
	}
}
