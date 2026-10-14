package tools

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The emitter a run gives its tools is also the ctx's provider event sink, so
// a model call a tool makes for the run (a decision, an embedding) can report
// its lane wait on the run's stream. Without it the call lanes see no sink and
// the wait goes unreported.
func TestWithEventEmitter_IsTheProvidersEventSink(t *testing.T) {
	if providers.EventSink(context.Background()) != nil {
		t.Fatal("a ctx with no run has an event sink")
	}
	var got []providers.Event
	ctx := WithEventEmitter(context.Background(), func(ev providers.Event) { got = append(got, ev) })
	sink := providers.EventSink(ctx)
	if sink == nil {
		t.Fatal("a ctx carrying the run's emitter has no provider event sink")
	}
	sink(providers.Event{Type: providers.EventCallQueued})
	if len(got) != 1 || got[0].Type != providers.EventCallQueued {
		t.Fatalf("the sink delivered %+v to the run's emitter, want the call_queued event", got)
	}
}
