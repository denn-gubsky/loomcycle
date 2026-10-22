package loop

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/errkind"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// lastErrorEvent returns the last error event in events, or nil.
func lastErrorEventOf(events []providers.Event) *providers.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == providers.EventError {
			return &events[i]
		}
	}
	return nil
}

// The error a run cancelled mid-turn ends on is classified cancelled, so a
// reader (the Web UI terminal, an adapter) can tell a stopped run from a
// failed one without parsing the text.
func TestRun_MidTurnCancelErrorIsClassifiedCancelled(t *testing.T) {
	p := &cutStreamProvider{streamed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-p.streamed; cancel() }()
	_, _, events := runRecording(t, ctx, RunOptions{Provider: p, Model: "m"})
	ev := lastErrorEventOf(events)
	if ev == nil || ev.ErrorInfo == nil || ev.ErrorInfo.Category != errkind.CategoryCancelled || ev.ErrorInfo.Retryable {
		t.Fatalf("error event = %+v, want error_info category %q, not retryable", ev, errkind.CategoryCancelled)
	}
}

// A run the wall-limit watcher cut is recorded failed, so its error is not
// called cancelled.
func TestRun_WallLimitCutErrorIsNotClassifiedCancelled(t *testing.T) {
	_, _, events := runRecording(t, context.Background(), wallOpts(&waitingProvider{}, 1))
	if ev := lastErrorEventOf(events); ev != nil && ev.ErrorInfo != nil && ev.ErrorInfo.Category == errkind.CategoryCancelled {
		t.Fatalf("error event = %+v: a wall-limit failure is classified cancelled", ev)
	}
}
