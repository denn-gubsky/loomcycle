package loop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// reviewProvider answers every call with "answer N" and records what it was
// sent, so a test can tell which answer a run completed on and whether
// feedback reached the model.
type reviewProvider struct {
	mu       sync.Mutex
	requests [][]providers.Message
}

func (p *reviewProvider) ID() string                                   { return "answer-test" }
func (p *reviewProvider) Probe(context.Context) error                  { return nil }
func (p *reviewProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *reviewProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *reviewProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]providers.Message(nil), req.Messages...))
	n := len(p.requests)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: fmt.Sprintf("answer %d", n)}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func (p *reviewProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *reviewProvider) lastUserText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	msgs := p.requests[len(p.requests)-1]
	last := msgs[len(msgs)-1]
	if last.Role != "user" || len(last.Content) == 0 {
		return ""
	}
	return last.Content[0].Text
}

// reviewRun starts a run in the background and reports each event type it
// emits, so a test can wait for a hold.
type reviewRun struct {
	q      chan steer.Message
	events chan providers.Event
	done   chan runOutcome
	prov   *reviewProvider
	steers atomic.Int32
}

func startReviewRun(t *testing.T, ctx context.Context, mutate func(*RunOptions)) *reviewRun {
	t.Helper()
	r := &reviewRun{
		q:      make(chan steer.Message, 8),
		events: make(chan providers.Event, 64),
		done:   make(chan runOutcome, 1),
		prov:   &reviewProvider{},
	}
	opts := RunOptions{
		Provider:   r.prov,
		Model:      "x",
		Segments:   steerSegs(),
		SteerQueue: r.q,
		Review:     true,
		OnSteer:    func(steer.Message) { r.steers.Add(1) },
		OnEvent: func(ev providers.Event) {
			select {
			case r.events <- ev:
			default:
			}
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	go func() {
		res, err := Run(ctx, opts)
		r.done <- runOutcome{res, err}
	}()
	return r
}

// waitFor returns the next event of type want, failing after 2s.
func (r *reviewRun) waitFor(t *testing.T, want providers.EventType) providers.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-r.events:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event", want)
		}
	}
}

type runOutcome struct {
	res RunResult
	err error
}

// finish waits for the run to end and returns how.
func (r *reviewRun) finish(t *testing.T) runOutcome {
	t.Helper()
	select {
	case o := <-r.done:
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish")
	}
	return runOutcome{}
}

// result waits for a run that must end without an error.
func (r *reviewRun) result(t *testing.T) RunResult {
	t.Helper()
	o := r.finish(t)
	if o.err != nil {
		t.Errorf("Run: %v", o.err)
	}
	return o.res
}

// verdict is a verdict sent now, as the server would enqueue it.
func verdict(kind, text string) steer.Message {
	return steer.Message{Kind: kind, Text: text, EnqueuedAt: time.Now()}
}

// A held run does not complete until approved, and completes on the answer it
// was held on.
func TestRun_Review_ApproveCompletesWithTheHeldAnswer(t *testing.T) {
	r := startReviewRun(t, context.Background(), nil)
	ev := r.waitFor(t, providers.EventAwaitingReview)
	if ev.AwaitingReview == nil || ev.AwaitingReview.Round != 1 {
		t.Errorf("hold = %+v, want round 1", ev.AwaitingReview)
	}
	select {
	case <-r.done:
		t.Fatal("a held run completed without a verdict")
	case <-time.After(50 * time.Millisecond):
	}
	r.q <- verdict(steer.KindApprove, "")
	res := r.result(t)
	if res.StopReason != "end_turn" || res.FinalText != "answer 1" || r.prov.calls() != 1 {
		t.Errorf("result = %q %q after %d calls, want end_turn on answer 1", res.StopReason, res.FinalText, r.prov.calls())
	}
}

// Reject with feedback: the feedback is the model's next user turn, persisted
// like a steer, and the revision is held again as round 2.
func TestRun_Review_RejectWithFeedbackRevisesAndHoldsAgain(t *testing.T) {
	r := startReviewRun(t, context.Background(), nil)
	r.waitFor(t, providers.EventAwaitingReview)
	r.q <- verdict(steer.KindReject, "redo section 3")
	ev := r.waitFor(t, providers.EventAwaitingReview)
	if ev.AwaitingReview.Round != 2 {
		t.Errorf("second hold round = %d, want 2", ev.AwaitingReview.Round)
	}
	if got := r.prov.lastUserText(); got != "redo section 3" {
		t.Errorf("revision was sent %q as its last user turn, want the feedback", got)
	}
	if r.steers.Load() != 1 {
		t.Errorf("OnSteer fired %d times, want 1 — feedback not persisted as a user turn", r.steers.Load())
	}
	r.q <- verdict(steer.KindApprove, "")
	if res := r.result(t); res.FinalText != "answer 2" {
		t.Errorf("final text = %q, want the revised answer", res.FinalText)
	}
}

// Reject with no feedback ends the run rejected, on no further model call.
func TestRun_Review_RejectWithoutFeedbackEndsRejected(t *testing.T) {
	r := startReviewRun(t, context.Background(), nil)
	r.waitFor(t, providers.EventAwaitingReview)
	r.q <- verdict(steer.KindReject, "  ")
	res := r.result(t)
	if res.StopReason != StopReasonRejected || r.prov.calls() != 1 {
		t.Errorf("result = %q after %d calls, want %q after 1", res.StopReason, r.prov.calls(), StopReasonRejected)
	}
	done := r.waitFor(t, providers.EventDone)
	if done.StopReason != StopReasonRejected {
		t.Errorf("done event stop reason = %q", done.StopReason)
	}
}

// A verdict queued before the hold began is a duplicate of one already acted
// on; acting on it would approve a revision nobody has read.
func TestRun_Review_VerdictFromBeforeTheHoldIsDiscarded(t *testing.T) {
	r := startReviewRun(t, context.Background(), nil)
	r.waitFor(t, providers.EventAwaitingReview)
	stale := verdict(steer.KindApprove, "")
	stale.EnqueuedAt = time.Now().Add(-time.Minute)
	r.q <- stale
	select {
	case <-r.done:
		t.Fatal("a stale approval released the hold")
	case <-time.After(50 * time.Millisecond):
	}
	r.q <- verdict(steer.KindApprove, "")
	if res := r.result(t); res.StopReason != "end_turn" {
		t.Errorf("stop reason = %q", res.StopReason)
	}
}

// An operator message sent while held is feedback, like a reject with text.
func TestRun_Review_OperatorMessageWhileHeldIsFeedback(t *testing.T) {
	r := startReviewRun(t, context.Background(), nil)
	r.waitFor(t, providers.EventAwaitingReview)
	r.q <- steer.Message{Text: "also cover SSRF", EnqueuedAt: time.Now()}
	r.waitFor(t, providers.EventAwaitingReview)
	if got := r.prov.lastUserText(); got != "also cover SSRF" {
		t.Errorf("last user turn = %q", got)
	}
	r.q <- verdict(steer.KindApprove, "")
	r.result(t)
}

// Disarming review on a held run releases it as approved.
func TestRun_Review_DisarmWhileHeldApproves(t *testing.T) {
	orig := parkHeartbeatInterval
	parkHeartbeatInterval = 5 * time.Millisecond
	defer func() { parkHeartbeatInterval = orig }()

	var armed atomic.Bool
	armed.Store(true)
	r := startReviewRun(t, context.Background(), func(o *RunOptions) {
		o.ReviewNow = func(context.Context) bool { return armed.Load() }
	})
	r.waitFor(t, providers.EventAwaitingReview)
	armed.Store(false)
	if res := r.result(t); res.StopReason != "end_turn" || res.FinalText != "answer 1" {
		t.Errorf("result = %q %q, want approved on answer 1", res.StopReason, res.FinalText)
	}
}

// Cancelling a held run ends it as cancelled, never as a clean finish: the
// answer it was held on was not approved.
func TestRun_Review_CancelWhileHeldIsNotACompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := startReviewRun(t, ctx, nil)
	r.waitFor(t, providers.EventAwaitingReview)
	cancel()
	o := r.finish(t)
	if !errors.Is(o.err, context.Canceled) || o.res.StopReason == "end_turn" {
		t.Errorf("cancelled hold returned (%q, %v), want a cancellation error", o.res.StopReason, o.err)
	}
}

// An approved answer on an interactive run is accepted, and the run then waits
// for its next message as any interactive run does.
func TestRun_Review_ApprovedInteractiveRunParksForInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startReviewRun(t, ctx, func(o *RunOptions) { o.Interactive = true })
	r.waitFor(t, providers.EventAwaitingReview)
	r.q <- verdict(steer.KindApprove, "")
	r.waitFor(t, providers.EventAwaitingInput)
	cancel()
	r.finish(t)
}

// A compaction while held is applied, and the hold is announced again so the
// run's latest event still says it is held.
func TestRun_Review_CompactionWhileHeldReannouncesTheHold(t *testing.T) {
	r := startReviewRun(t, context.Background(), nil)
	r.waitFor(t, providers.EventAwaitingReview)
	r.q <- steer.Message{Kind: steer.KindCompact, Text: "summary", KeepN: 1, EnqueuedAt: time.Now()}
	r.waitFor(t, providers.EventContextCompaction)
	if ev := r.waitFor(t, providers.EventAwaitingReview); ev.AwaitingReview.Round != 1 {
		t.Errorf("re-announced round = %d, want still 1", ev.AwaitingReview.Round)
	}
	r.q <- verdict(steer.KindApprove, "")
	r.result(t)
}

// Nothing but a held run acts on a verdict: an interactive run waiting for
// work drops one, and one drained mid-run never becomes a user turn.
func TestRun_Review_VerdictIsNeverAnOperatorTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startReviewRun(t, ctx, func(o *RunOptions) {
		o.Review = false
		o.Interactive = true
	})
	// Drained at the top of the first iteration.
	r.q <- verdict(steer.KindReject, "drained verdict")
	r.waitFor(t, providers.EventAwaitingInput)
	if got := r.prov.lastUserText(); got == "drained verdict" {
		t.Error("a drained verdict became a user turn")
	}
	// Delivered while parked for input.
	r.q <- verdict(steer.KindReject, "parked verdict")
	time.Sleep(50 * time.Millisecond)
	if r.prov.calls() != 1 {
		t.Errorf("calls = %d, want 1: a verdict resumed a run waiting for work", r.prov.calls())
	}
	if r.steers.Load() != 0 {
		t.Errorf("OnSteer fired %d times for verdicts", r.steers.Load())
	}
	cancel()
	r.finish(t)
}

// A stateful run has no finished answer to hold, so it says review does not
// apply to it and ends without holding.
func TestRun_Review_StatefulRunReportsItIsNotApplied(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{`{"patch":{"n":1},"done":true,"final":"ok"}`}}
	echo := &echoTool{reply: "observed"}
	var reported, held bool
	_, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		SteerQueue: make(chan steer.Message, 1),
		Review:     true,
		OnEvent: func(ev providers.Event) {
			switch ev.Type {
			case providers.EventCapabilityInert:
				reported = reported || ev.CapabilityInert.Gate == "review"
			case providers.EventAwaitingReview:
				held = true
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reported || held {
		t.Errorf("reported = %v, held = %v; want reported and not held", reported, held)
	}
}
