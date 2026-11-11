package concurrency

import (
	"context"
	"testing"
	"time"
)

// A resumed run takes its slot through Wait or WaitForUser. Bound to a RunSlot
// it can lend that slot while a call waits for a provider lane, as an admitted
// run can, and the release still ends it.
func TestSemaphore_WaitBindsTheRunSlotSoTheRunCanLendIt(t *testing.T) {
	for _, name := range []string{"Wait", "WaitForUser"} {
		t.Run(name, func(t *testing.T) {
			s := New(1, 4, time.Second)
			rs := &RunSlot{}
			var release func()
			var err error
			if name == "Wait" {
				release, _, err = s.Wait(context.Background(), rs)
			} else {
				release, _, _, err = s.WaitForUser(context.Background(), "", "alice", rs)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !rs.Lend() {
				t.Fatal("the slot Wait took cannot be lent: it is not bound to the run")
			}
			if st := s.Stats(); st.Active != 0 || st.Lent != 1 {
				t.Errorf("after the lend: %+v, want nothing active and one lent", st)
			}
			release()
			if st := s.Stats(); st.Active != 0 || st.Lent != 0 {
				t.Errorf("after the release: %+v, want nothing active or lent", st)
			}
		})
	}
}

// waited says whether the caller found no free slot and queued for one.
func TestSemaphore_WaitReportsWhetherItQueued(t *testing.T) {
	s := New(1, 4, time.Second)
	first, waited, err := s.Wait(context.Background(), nil)
	if err != nil || waited {
		t.Fatalf("Wait on a free slot: waited=%v err=%v, want false, nil", waited, err)
	}
	done := make(chan bool, 1)
	go func() {
		release, _, waited, err := s.WaitForUser(context.Background(), "", "alice", nil)
		if err == nil {
			release()
		}
		done <- waited
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 1 })
	first()
	if !<-done {
		t.Error("WaitForUser behind a held slot reported it did not wait")
	}
}
