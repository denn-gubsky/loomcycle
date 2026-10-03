package loop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// steeredStatefulRun wires a stateful run to a real steer registry entry, as
// the server does. While the provider answers the step numbered pushOn
// (1-based; 0 = never) it pushes `late` into the run's queue — an operator
// message sent while that step is being produced.
type steeredStatefulRun struct {
	reg  *steer.Registry
	prov *steeringScriptProvider
	echo *echoTool
	opts RunOptions

	mu     sync.Mutex
	steers []string
	errs   []string
	events []providers.EventType
}

// steeringScriptProvider is actionScriptProvider with a push into the run's
// steer queue during one of its calls.
type steeringScriptProvider struct {
	*actionScriptProvider
	reg     *steer.Registry
	pushOn  int
	late    steer.Message
	pushErr error
}

func (p *steeringScriptProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	ch, err := p.actionScriptProvider.Call(ctx, req)
	if p.actionScriptProvider.calls() == p.pushOn {
		_, perr := p.reg.Push(context.Background(), "run-stateful", p.late)
		p.mu.Lock()
		p.pushErr = perr
		p.mu.Unlock()
	}
	return ch, err
}

// prompts is every step's fed user message, in order.
func (p *steeringScriptProvider) prompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.observed...)
}

func newSteeredStatefulRun(t *testing.T, pushOn int, late string, scripts ...string) *steeredStatefulRun {
	t.Helper()
	reg := steer.NewRegistry(4)
	q, dereg := reg.Register(steer.Entry{RunID: "run-stateful"})
	t.Cleanup(dereg)
	r := &steeredStatefulRun{reg: reg, echo: &echoTool{reply: "observed"}}
	r.prov = &steeringScriptProvider{actionScriptProvider: &actionScriptProvider{scripts: scripts},
		reg: reg, pushOn: pushOn, late: steer.Message{Text: late}}
	r.opts = RunOptions{
		Provider: r.prov, Model: "x",
		Tools:             []tools.Tool{r.echo},
		Dispatcher:        tools.NewDispatcher([]tools.Tool{r.echo}),
		Segments:          statefulTaskSegs(),
		Context:           statefulCtx(nil),
		SteerQueue:        q,
		CloseSteerIfEmpty: func() bool { return reg.CloseIfEmpty("run-stateful") },
		OnSteer: func(m steer.Message) {
			r.mu.Lock()
			r.steers = append(r.steers, m.Text)
			r.mu.Unlock()
		},
		OnEvent: func(ev providers.Event) {
			r.mu.Lock()
			r.events = append(r.events, ev.Type)
			if ev.Type == providers.EventError {
				r.errs = append(r.errs, ev.Error)
			}
			r.mu.Unlock()
		},
	}
	return r
}

func (r *steeredStatefulRun) run(t *testing.T) RunResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := Run(ctx, r.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.prov.pushErr != nil {
		t.Fatalf("the push during step %d was refused: %v", r.prov.pushOn, r.prov.pushErr)
	}
	return res
}

func (r *steeredStatefulRun) saw(typ providers.EventType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == typ {
			return true
		}
	}
	return false
}

// pushAfterFinish is what a message sent once the run has finished gets.
func (r *steeredStatefulRun) pushAfterFinish() error {
	_, err := r.reg.Push(context.Background(), "run-stateful", steer.Message{Text: "too late"})
	return err
}

// A message sent while an autonomous stateful run produces its `done` step was
// acknowledged, so it is answered: the run takes another step with the message
// as its task and the state it had built, and that answer is the run's.
func TestRun_Stateful_AMessageQueuedDuringTheFinalStepIsAnsweredBeforeTheRunEnds(t *testing.T) {
	r := newSteeredStatefulRun(t, 1, "Answer in one sentence.",
		`{"patch":{"topic":"DDRAM"},"done":true,"final":"answer one"}`,
		`{"patch":{"short":true},"done":true,"final":"answer two"}`)
	res := r.run(t)
	if n := r.prov.calls(); n != 2 {
		t.Fatalf("provider called %d time(s), want 2 — the message was never answered", n)
	}
	if res.StopReason != "end_turn" || res.FinalText != "answer two" {
		t.Errorf("result = %q %q, want end_turn on the second answer", res.StopReason, res.FinalText)
	}
	second := r.prov.prompts()[1]
	if !strings.HasSuffix(second, "Latest observation:\nTask: Answer in one sentence.") {
		t.Errorf("the second step was not handed the message as its observation: %q", second)
	}
	if !strings.Contains(second, `"topic":"DDRAM"`) {
		t.Errorf("the second step lost the state the first one built: %q", second)
	}
	if len(r.steers) != 1 || r.steers[0] != "Answer in one sentence." {
		t.Errorf("OnSteer saw %v, want the message recorded once as an operator turn", r.steers)
	}
	if len(r.errs) != 0 {
		t.Errorf("errors = %v, want none", r.errs)
	}
	if err := r.pushAfterFinish(); !errors.Is(err, steer.ErrRunNotFound) {
		t.Errorf("a push after the run finished = %v, want ErrRunNotFound", err)
	}
}

// With nothing waiting, the run ends exactly as it did — one call, its answer —
// and its queue is closed to anything sent afterwards.
func TestRun_Stateful_AnEmptyQueueAtDoneEndsTheRunOnItsAnswer(t *testing.T) {
	r := newSteeredStatefulRun(t, 0, "", `{"patch":{"n":1},"done":true,"final":"answer one"}`)
	res := r.run(t)
	if n := r.prov.calls(); n != 1 || res.StopReason != "end_turn" || res.FinalText != "answer one" {
		t.Errorf("result = %q %q after %d call(s), want end_turn on answer one after 1", res.StopReason, res.FinalText, n)
	}
	if len(r.steers) != 0 || len(r.errs) != 0 {
		t.Errorf("steers %v errors %v, want none", r.steers, r.errs)
	}
	if err := r.pushAfterFinish(); !errors.Is(err, steer.ErrRunNotFound) {
		t.Errorf("a push after the run finished = %v, want ErrRunNotFound", err)
	}
}

// With no step left to answer it, a waiting message cannot be: the run
// completes on the answer it has, and the append loop's error names the
// message. It is not recorded as an operator turn.
func TestRun_Stateful_AMessageAtTheStepCapEndsCompletedAndNamesTheMessage(t *testing.T) {
	r := newSteeredStatefulRun(t, 1, "Answer in one sentence.",
		`{"patch":{"n":1},"done":true,"final":"answer one"}`)
	r.opts.MaxIterations = 1
	res := r.run(t)
	if n := r.prov.calls(); n != 1 || res.StopReason != "end_turn" || res.FinalText != "answer one" {
		t.Errorf("result = %q %q after %d call(s), want end_turn on answer one after 1", res.StopReason, res.FinalText, n)
	}
	want := "an operator message arrived after the final answer and the run has no iteration left to answer it: Answer in one sentence."
	if len(r.errs) != 1 || r.errs[0] != want {
		t.Errorf("errors = %q, want exactly %q", r.errs, want)
	}
	if len(r.steers) != 0 {
		t.Errorf("OnSteer saw %v — an unanswered message was recorded as a turn", r.steers)
	}
	if err := r.pushAfterFinish(); !errors.Is(err, steer.ErrRunNotFound) {
		t.Errorf("a push after the run finished = %v, want ErrRunNotFound", err)
	}
}

// A compaction control or a review verdict waiting at `done` is not an
// operator turn: the run ends on its answer, and no compaction is announced
// for a history this loop does not have.
func TestRun_Stateful_AControlMessageAtDoneIsNotAnOperatorTurn(t *testing.T) {
	for _, kind := range []string{steer.KindCompact, steer.KindApprove} {
		r := newSteeredStatefulRun(t, 1, "a summary", `{"patch":{"n":1},"done":true,"final":"answer one"}`)
		r.prov.late.Kind = kind
		res := r.run(t)
		if n := r.prov.calls(); n != 1 || res.FinalText != "answer one" {
			t.Errorf("%s: result %q after %d call(s), want answer one after 1", kind, res.FinalText, n)
		}
		if len(r.steers) != 0 || len(r.errs) != 0 || r.saw(providers.EventContextCompaction) {
			t.Errorf("%s: steers %v errors %v compaction %v, want none", kind, r.steers, r.errs,
				r.saw(providers.EventContextCompaction))
		}
	}
}

// An interactive stateful run is unchanged: a message sent while it works is
// not read between steps, it waits for the park at `done`, which hands it over
// as the operator's turn — and the park never closes the queue.
func TestRun_Stateful_AnInteractiveRunStillParksAndTakesTheMessageThere(t *testing.T) {
	r := newSteeredStatefulRun(t, 1, "and refresh cycles?",
		`{"patch":{},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"n":1},"done":true,"final":"answer one"}`,
		`{"patch":{"n":2},"done":true,"final":"answer two"}`)
	r.opts.Interactive = true
	closes := 0
	r.opts.CloseSteerIfEmpty = func() bool { closes++; return true }
	parks := make(chan struct{}, 8)
	inner := r.opts.OnEvent
	r.opts.OnEvent = func(ev providers.Event) {
		inner(ev)
		if ev.Type == providers.EventAwaitingInput {
			parks <- struct{}{}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, r.opts)
	}()
	for i := 1; i <= 2; i++ {
		select {
		case <-parks:
		case <-time.After(3 * time.Second):
			t.Fatalf("the run never reached park %d — it ended instead of waiting for its operator", i)
		}
	}
	cancel()
	<-done

	prompts := r.prov.prompts()
	if len(prompts) != 3 {
		t.Fatalf("provider called %d time(s), want 3", len(prompts))
	}
	if strings.Contains(prompts[1], "and refresh cycles?") {
		t.Errorf("an interactive run read its message between steps: %q", prompts[1])
	}
	if !strings.HasSuffix(prompts[2], "Latest observation:\noperator: and refresh cycles?") {
		t.Errorf("the park did not hand the message over as the operator's turn: %q", prompts[2])
	}
	if closes != 0 {
		t.Errorf("an interactive run closed its queue %d time(s) while parked", closes)
	}
}
