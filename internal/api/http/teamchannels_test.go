package http

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// starterIO is the channel executor a walk with c1 as its source gets.
func starterIO(srv *Server) *teamChannelIO {
	return &teamChannelIO{srv: srv, acl: &teamgraph.TeamChannels{Subscribe: []string{"c1"}, Publish: []string{"c2"}}}
}

func publishC1(t *testing.T, srv *Server, n int) {
	t.Helper()
	if _, err := srv.systemPublisher.PublishNow(context.Background(), "c1", "", store.MemoryScopeGlobal, "",
		json.RawMessage(fmt.Sprintf(`{"n":%d}`, n)), "_system", 0, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// A Starter reads after its committed cursor. It used to peek from the oldest
// message, so every acked message came back on the next wave.
func TestTeamChannelIO_ReadStartsAtTheCommittedCursor(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	io, ctx := starterIO(srv), context.Background()
	publishC1(t, srv, 1)
	publishC1(t, srv, 2)

	first, cursor, err := io.Read(ctx, "c1", 1, 10, 100)
	if err != nil || len(first) != 2 {
		t.Fatalf("first read: %d messages, err %v; want 2", len(first), err)
	}
	if err := io.Ack(ctx, "c1", cursor); err != nil {
		t.Fatalf("ack: %v", err)
	}
	publishC1(t, srv, 3)

	next, _, err := io.Read(ctx, "c1", 1, 10, 100)
	if err != nil || len(next) != 1 || string(next[0].Payload) != `{"n":3}` {
		t.Fatalf("after the ack: got %v (err %v), want only the third message", next, err)
	}
}

// A Starter waits for its batch: a message published a moment after the read
// began is returned, instead of the walk failing with "no message".
func TestTeamChannelIO_ReadWaitsForWant(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	go func() {
		time.Sleep(100 * time.Millisecond)
		publishC1(t, srv, 1)
	}()
	start := time.Now()
	msgs, _, err := starterIO(srv).Read(context.Background(), "c1", 1, 10, 1500)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("got %d messages (err %v), want the one published during the wait", len(msgs), err)
	}
	if time.Since(start) >= 1500*time.Millisecond {
		t.Errorf("the read waited out its whole budget; a publish should wake it")
	}
}

// A short read after the wait is not an error: the Starter decides what it means.
func TestTeamChannelIO_ReadReturnsShortAfterTheWait(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	publishC1(t, srv, 1)
	start := time.Now()
	msgs, _, err := starterIO(srv).Read(context.Background(), "c1", 3, 10, 200)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("got %d messages (err %v), want the 1 available", len(msgs), err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Errorf("returned after %v with fewer than want; it should wait for the batch", time.Since(start))
	}
}

// wait_ms 0 means the operator's long-poll cap, and no wait may exceed it.
func TestTeamChannelIO_ReadWaitIsBoundedByTheLongPollCap(t *testing.T) {
	srv, cleanup := channelFanFixture(t) // cap 2000 ms
	defer cleanup()
	io := starterIO(srv)
	for _, tc := range []struct{ in, want int }{{0, 2000}, {500, 500}, {99999, 2000}} {
		if got := io.readWait(tc.in); got != time.Duration(tc.want)*time.Millisecond {
			t.Errorf("readWait(%d) = %v, want %d ms", tc.in, got, tc.want)
		}
	}
}
