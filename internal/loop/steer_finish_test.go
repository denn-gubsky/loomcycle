package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// lateSteerProvider answers every call with "answer N" and records what it
// was sent. While it writes the answer of the call numbered in pushOn, it
// pushes `late` into the run's steer queue — an operator message sent while
// the run's final answer is streaming.
type lateSteerProvider struct {
	mu       sync.Mutex
	requests [][]providers.Message
	reg      *steer.Registry
	runID    string
	pushOn   int // 1-based call number; 0 = never push
	late     string
	pushErr  error
}

func (p *lateSteerProvider) ID() string                                   { return "late-steer-test" }
func (p *lateSteerProvider) Probe(context.Context) error                  { return nil }
func (p *lateSteerProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *lateSteerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *lateSteerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]providers.Message(nil), req.Messages...))
	n := len(p.requests)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: fmt.Sprintf("answer %d", n)}
	if n == p.pushOn {
		_, err := p.reg.Push(context.Background(), p.runID, steer.Message{Text: p.late})
		p.mu.Lock()
		p.pushErr = err
		p.mu.Unlock()
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func (p *lateSteerProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// lateSteerRun wires a non-interactive run to a real steer registry entry, as
// the server does, and returns the options plus what OnSteer and OnEvent saw.
type lateSteerRun struct {
	reg    *steer.Registry
	prov   *lateSteerProvider
	opts   RunOptions
	mu     sync.Mutex
	steers []string
	errs   []string
}

func newLateSteerRun(t *testing.T, pushOn int, late string) *lateSteerRun {
	t.Helper()
	reg := steer.NewRegistry(4)
	q, dereg := reg.Register(steer.Entry{RunID: "run-late"})
	t.Cleanup(dereg)
	r := &lateSteerRun{reg: reg, prov: &lateSteerProvider{reg: reg, runID: "run-late", pushOn: pushOn, late: late}}
	r.opts = RunOptions{
		Provider:          r.prov,
		Model:             "x",
		Segments:          steerSegs(),
		SteerQueue:        q,
		CloseSteerIfEmpty: func() bool { return reg.CloseIfEmpty("run-late") },
		OnSteer: func(m steer.Message) {
			r.mu.Lock()
			r.steers = append(r.steers, m.Text)
			r.mu.Unlock()
		},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventError {
				r.mu.Lock()
				r.errs = append(r.errs, ev.Error)
				r.mu.Unlock()
			}
		},
	}
	return r
}

// pushAfterFinish is what a message sent once the run has finished gets.
func (r *lateSteerRun) pushAfterFinish() error {
	_, err := r.reg.Push(context.Background(), "run-late", steer.Message{Text: "too late"})
	return err
}

// A message sent while the run's final answer is streaming was acknowledged,
// so it is answered: the run takes one more turn with the message in view,
// recorded as an operator turn, and that answer is the run's.
func TestRun_ASteerSentDuringTheFinalAnswerIsAnsweredBeforeTheRunEnds(t *testing.T) {
	r := newLateSteerRun(t, 1, "Answer in one sentence.")
	res, err := Run(context.Background(), r.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.prov.pushErr != nil {
		t.Fatalf("the push during the answer was refused: %v", r.prov.pushErr)
	}
	if n := r.prov.calls(); n != 2 {
		t.Fatalf("provider called %d time(s), want 2 — the message was never answered", n)
	}
	if res.StopReason != "end_turn" || res.FinalText != "answer 2" {
		t.Errorf("result = %q %q, want end_turn on the second answer", res.StopReason, res.FinalText)
	}
	second := r.prov.requests[1]
	at := userTextIdx(second, "Answer in one sentence.")
	if at != len(second)-1 || at < 1 || second[at-1].Role != "assistant" || lastAssistantText(second[:at]) != "answer 1" {
		t.Errorf("second request = %+v, want the message as the last user turn, after the first answer", second)
	}
	if len(r.steers) != 1 || r.steers[0] != "Answer in one sentence." {
		t.Errorf("OnSteer saw %v, want the message recorded once as an operator turn", r.steers)
	}
	if err := r.pushAfterFinish(); !errors.Is(err, steer.ErrRunNotFound) {
		t.Errorf("a push after the run finished = %v, want ErrRunNotFound", err)
	}
}

// With nothing waiting, the run ends exactly as it did: one call, the first
// answer, and a queue closed to anything sent afterwards.
func TestRun_AnEmptyQueueAtTheFinishEndsTheRunOnItsAnswer(t *testing.T) {
	r := newLateSteerRun(t, 0, "")
	res, err := Run(context.Background(), r.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := r.prov.calls(); n != 1 || res.StopReason != "end_turn" || res.FinalText != "answer 1" {
		t.Errorf("result = %q %q after %d call(s), want end_turn on answer 1 after 1", res.StopReason, res.FinalText, n)
	}
	if len(r.steers) != 0 || len(r.errs) != 0 {
		t.Errorf("steers %v errors %v, want none", r.steers, r.errs)
	}
	if err := r.pushAfterFinish(); !errors.Is(err, steer.ErrRunNotFound) {
		t.Errorf("a push after the run finished = %v, want ErrRunNotFound", err)
	}
}

// With no iteration left to answer it, a waiting message cannot be: the run
// completes on the answer it has, and an error names the message so it is not
// acknowledged and silently lost. It is not appended as an unanswered turn.
func TestRun_ASteerAtTheIterationCapEndsCompletedAndNamesTheMessage(t *testing.T) {
	r := newLateSteerRun(t, 1, "Answer in one sentence.")
	r.opts.MaxIterations = 1
	res, err := Run(context.Background(), r.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := r.prov.calls(); n != 1 || res.StopReason != "end_turn" || res.FinalText != "answer 1" {
		t.Errorf("result = %q %q after %d call(s), want end_turn on answer 1 after 1", res.StopReason, res.FinalText, n)
	}
	if len(r.errs) != 1 || !strings.Contains(r.errs[0], "no iteration left") || !strings.Contains(r.errs[0], "Answer in one sentence.") {
		t.Errorf("errors = %v, want one naming the unanswered message", r.errs)
	}
	if len(r.steers) != 0 {
		t.Errorf("OnSteer saw %v — an unanswered message was recorded as a turn", r.steers)
	}
	if err := r.pushAfterFinish(); !errors.Is(err, steer.ErrRunNotFound) {
		t.Errorf("a push after the run finished = %v, want ErrRunNotFound", err)
	}
}

// A long unanswerable message is named, clipped.
func TestClipRunes_ShortensLongTextOnARuneBoundary(t *testing.T) {
	if got := clipRunes("héllo", 10); got != "héllo" {
		t.Errorf("short text = %q, want it unchanged", got)
	}
	if got := clipRunes("héllo", 2); got != "hé…" {
		t.Errorf("clipped = %q, want hé…", got)
	}
}

// A message waiting behind the approval of a held, non-interactive run is
// answered too: approval ends the hold, not the run's reading of its queue.
func TestRun_ASteerQueuedBehindAnApprovalIsAnswered(t *testing.T) {
	holds := 0
	r := startReviewRun(t, context.Background(), func(o *RunOptions) {
		// The run's own queue, pushed to from inside the hold's announcement
		// so the approval and the message are both waiting when it reads.
		q := make(chan steer.Message, 8)
		o.SteerQueue = q
		inner := o.OnEvent
		o.OnEvent = func(ev providers.Event) {
			if ev.Type == providers.EventAwaitingReview {
				holds++
				q <- verdict(steer.KindApprove, "")
				if holds == 1 {
					q <- steer.Message{Text: "add a summary line"}
				}
			}
			inner(ev)
		}
	})
	res := r.result(t)
	if r.prov.calls() != 2 || res.FinalText != "answer 2" {
		t.Fatalf("result %q after %d call(s), want the second answer after 2", res.FinalText, r.prov.calls())
	}
	if got := r.prov.lastUserText(); got != "add a summary line" {
		t.Errorf("second call's last user turn = %q, want the waiting message", got)
	}
	if holds != 2 {
		t.Errorf("held %d time(s), want 2 — the answer to the message is reviewed too", holds)
	}
}

// A resumed held run that comes back approved, and is not interactive, ends
// on the answer it was holding — unless a message is waiting, which it answers
// first, as a live run approved at the end of its turn does.
func TestRun_ResumeHeld_ApprovedRunAnswersAWaitingSteer(t *testing.T) {
	q := make(chan steer.Message, 1)
	q <- steer.Message{Text: "cover the rollback"}
	r := startResumedHold(t, context.Background(), func(o *RunOptions) {
		o.SteerQueue = q
		o.ReviewNow = func(context.Context) bool { return false } // comes back approved
	})
	res := r.result(t)
	if r.prov.calls() != 1 || res.StopReason != "end_turn" || res.FinalText != "answer 1" {
		t.Fatalf("result = %q %q after %d call(s), want end_turn on the answer to the message", res.StopReason, res.FinalText, r.prov.calls())
	}
	if got := r.prov.lastUserText(); got != "cover the rollback" {
		t.Errorf("the call's last user turn = %q, want the waiting message", got)
	}
}
