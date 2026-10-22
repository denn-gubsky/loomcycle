package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// cutStreamProvider streams "partial" and then holds the turn open until the
// call's ctx ends, closing the stream the way a driver does when its request
// is cancelled: with no done event, and — when errFrame is set — with the
// error frame a driver's failed read surfaces. finishFirst instead sends done
// and then ends the run itself before closing, so the turn finished first.
type cutStreamProvider struct {
	errFrame    bool
	finishFirst func()
	streamed    chan struct{} // closed once "partial" is on the channel
}

func (p *cutStreamProvider) ID() string                                   { return "stub" }
func (p *cutStreamProvider) Probe(context.Context) error                  { return nil }
func (p *cutStreamProvider) ListModels(context.Context) ([]string, error) { return []string{"m"}, nil }
func (p *cutStreamProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *cutStreamProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 4)
	go func() {
		defer close(ch)
		ch <- providers.Event{Type: providers.EventText, Text: "partial"}
		if p.streamed != nil {
			close(p.streamed)
		}
		if p.finishFirst != nil {
			ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
			p.finishFirst()
			<-ctx.Done()
			return
		}
		<-ctx.Done()
		if p.errFrame {
			ch <- providers.Event{Type: providers.EventError, Error: "stub: read body: " + ctx.Err().Error()}
		}
	}()
	return ch, nil
}

// assertEndedCancelled checks a run its own ctx ended mid-turn: the ctx's
// error comes back, the run is reported cancelled with the text it had, and
// no done event claims it finished.
func assertEndedCancelled(t *testing.T, res RunResult, err error, events []providers.Event, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("Run err = %v (stop_reason %q, text %q), want %v: a run whose ctx ended mid-turn did not finish", err, res.StopReason, res.FinalText, want)
	}
	if res.StopReason != "cancelled" {
		t.Errorf("stop_reason = %q, want cancelled", res.StopReason)
	}
	if res.FinalText != "partial" {
		t.Errorf("final text = %q, want the partial text the turn had streamed", res.FinalText)
	}
	if n := countEvents(events, providers.EventDone); n != 0 {
		t.Errorf("EventDone count = %d, want 0: a cancelled run must not report done", n)
	}
}

// The run's caller cancels while the model is mid-turn: the provider's stream
// closes with no done event. The loop used to read that as a finished turn
// and return no error, so the run was recorded completed with its partial
// text and an empty stop reason.
func TestRun_RunCtxCancelledMidTurnEndsCancelled(t *testing.T) {
	p := &cutStreamProvider{streamed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-p.streamed; cancel() }()
	res, err, events := runRecording(t, ctx, RunOptions{Provider: p, Model: "m"})
	assertEndedCancelled(t, res, err, events, context.Canceled)
}

// The caller's deadline passes mid-turn: the run ends with the deadline's
// error, not as a completion.
func TestRun_RunCtxDeadlineMidTurnEndsCancelled(t *testing.T) {
	// The provider holds the turn open until the deadline, so the deadline
	// always lands mid-turn, after the text.
	p := &cutStreamProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err, events := runRecording(t, ctx, RunOptions{Provider: p, Model: "m"})
	assertEndedCancelled(t, res, err, events, context.DeadlineExceeded)
}

// A driver whose read failed on the cancelled request surfaces an error frame
// before closing. That is the cancel too, not a provider fault: the run ends
// cancelled rather than failed with "provider error: ... context canceled".
func TestRun_RunCtxCancelledMidTurnWithErrorFrameEndsCancelled(t *testing.T) {
	p := &cutStreamProvider{errFrame: true, streamed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-p.streamed; cancel() }()
	res, err, events := runRecording(t, ctx, RunOptions{Provider: p, Model: "m"})
	assertEndedCancelled(t, res, err, events, context.Canceled)
}

// A turn the model finished (done arrived) before the run's ctx ended is a
// finished run: it completes on its answer.
func TestRun_TurnDoneBeforeRunCtxCancelCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &cutStreamProvider{finishFirst: cancel}
	res, err, _ := runRecording(t, ctx, RunOptions{Provider: p, Model: "m"})
	if err != nil {
		t.Fatalf("Run err = %v, want nil: the turn finished before the cancel", err)
	}
	if res.StopReason != "end_turn" || res.FinalText != "partial" {
		t.Errorf("result = (%q, %q), want (end_turn, partial)", res.StopReason, res.FinalText)
	}
}
