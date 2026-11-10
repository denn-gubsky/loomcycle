package coord

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// TestPostgresBackplane_APanickingListenReconnectsAndKeepsDelivering: a panic
// in one LISTEN session is treated like a dropped connection, so the
// subscription reconnects and its channel keeps delivering instead of closing
// and leaving every listener on it stale.
func TestPostgresBackplane_APanickingListenReconnectsAndKeepsDelivering(t *testing.T) {
	var calls atomic.Int32
	b := &PostgresBackplane{listenOnce: func(ctx context.Context, topic string, out chan<- Event) error {
		if calls.Add(1) == 1 {
			panic("listen bug")
		}
		out <- Event{Topic: topic, Payload: []byte("after")}
		<-ctx.Done()
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Event, 1)
	safego.Go(ctx, "test.backplane", func() { b.runSubscribeLoop(ctx, "loomcycle.test", out) })
	select {
	case ev := <-out:
		if string(ev.Payload) != "after" {
			t.Fatalf("payload %q", ev.Payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the subscription did not reconnect after a panicking LISTEN session")
	}
}
