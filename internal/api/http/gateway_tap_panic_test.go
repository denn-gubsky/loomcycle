package http

import (
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A tap that panics mid-stream ends the call as a failed one: the reader gets
// an error (the gateway answers 502), never a truncated completion that reads
// as finished, and the provider's stream is still read to its end.
func TestGatewayTap_APanicFailsTheCallAndDrainsTheProvider(t *testing.T) {
	ch := make(chan providers.Event)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		ch <- providers.Event{Type: providers.EventText, Text: "partial"}
		// A span with no timer behind it panics on the done event.
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn"}
		ch <- providers.Event{Type: providers.EventText, Text: "after"}
		close(ch)
	}()
	m := &gatewayMeter{span: &loop.ModelCall{}}
	done := make(chan error, 1)
	go func() {
		_, err := collectProviderEventsIntoResponse(m.tap(ch), "id", "req", "p", "m")
		done <- err
	}()
	select {
	case err := <-done:
		var perr *gatewayProviderErr
		if !errors.As(err, &perr) {
			t.Fatalf("err = %v, want the call failed (a provider error the gateway answers 502)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway call never ended after its tap panicked")
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider's stream was left unread after the tap panicked")
	}
}
