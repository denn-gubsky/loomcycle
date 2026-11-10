package lanegate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// A forwarder that panics mid-stream ends the call as a failed one: its reader
// gets an error event, never a closed stream that reads as finished, and the
// driver's stream is read to its end before the lane slot goes back.
func TestForward_APanicFailsTheCallAndDrainsTheDriver(t *testing.T) {
	in := make(chan providers.Event)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		in <- providers.Event{Type: providers.EventText, Text: "partial"}
		in <- providers.Event{Type: providers.EventDone} // release panics here
		in <- providers.Event{Type: providers.EventText, Text: "after"}
		close(in)
	}()
	var releases atomic.Int32
	release := func() {
		if releases.Add(1) == 1 {
			panic("release bug")
		}
	}
	out := make(chan providers.Event)
	ctx := context.Background()
	safego.Go(ctx, "test.forward", func() { forward(ctx, in, out, release, 0) })

	var sawErr bool
	timeout := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case ev, ok := <-out:
			open = ok
			if ok && ev.Type == providers.EventError {
				sawErr = true
			}
		case <-timeout:
			t.Fatal("the forwarder's output never closed after it panicked")
		}
	}
	if !sawErr {
		t.Error("the call's stream closed with no error event: a truncated stream reads as a finished one")
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the driver's stream was left unread after the forwarder panicked")
	}
	if releases.Load() < 2 {
		t.Errorf("release called %d times, want the slot released after the panic too", releases.Load())
	}
}
