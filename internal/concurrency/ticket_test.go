package concurrency

import (
	"context"
	"errors"
	"testing"
)

// A second run naming a ticket that a waiting run already holds — a duplicate
// agent_id, which the caller refuses only after the run has queued — must not
// take the first run's place in the ticket index. It did: its track replaced
// the first waiter and its untrack then deleted the entry, so the run still
// waiting lost its queue position and reason for every read after.
func TestAdmit_DuplicateTicketKeepsTheWaitingRunsPosition(t *testing.T) {
	inUse := errors.New("agent_id in use")
	for _, tc := range []struct {
		name string
		s    func() *Semaphore
		want string
	}{
		{"global cap", func() *Semaphore { return New(1, 8, 0) }, QueueReasonGlobalCap},
		{"user cap", func() *Semaphore { return New(8, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(4) }, QueueReasonUserCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s()
			held, err := s.Admit(context.Background(), AdmitRequest{UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first, infos := admitAsync(t, ctx, s, AdmitRequest{UserID: "u", Ticket: "dup"})
			recvInfo(t, infos)

			var during QueueInfo
			_, err = s.Admit(context.Background(), AdmitRequest{UserID: "u", Ticket: "dup", OnQueued: func(QueueInfo) error {
				during, _ = s.Position("dup")
				return inUse
			}})
			if !errors.Is(err, inUse) {
				t.Fatalf("duplicate: err = %v, want the OnQueued refusal", err)
			}
			if during.Position != 1 {
				t.Errorf("while the duplicate was queued, Position(dup) = %+v — want the first run's place, 1", during)
			}
			q, ok := s.Position("dup")
			if !ok || q.Position != 1 || q.Reason != tc.want {
				t.Errorf("after the duplicate withdrew, Position(dup) = %+v %v — want the first run still at 1 (%s)", q, ok, tc.want)
			}

			cancel()
			if r := recvResult(t, first); !errors.Is(r.err, context.Canceled) {
				t.Fatalf("first run: %v, want context.Canceled", r.err)
			}
			if _, ok := s.Position("dup"); ok {
				t.Error("the first run left the queue and still has a position")
			}
			held()
		})
	}
}
