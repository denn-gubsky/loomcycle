package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// waitThenAnswerProvider stands in for a call queued in a provider's lane: its
// first Call blocks until its ctx ends and fails with the ctx's error, having
// streamed nothing. Every later call answers end_turn at once.
type waitThenAnswerProvider struct {
	mu      sync.Mutex
	calls   int
	waiting chan struct{}
}

func (p *waitThenAnswerProvider) ID() string                                   { return "scripted" }
func (p *waitThenAnswerProvider) Probe(context.Context) error                  { return nil }
func (p *waitThenAnswerProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *waitThenAnswerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *waitThenAnswerProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	n := p.calls
	p.calls++
	p.mu.Unlock()
	if n == 0 {
		close(p.waiting)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "resumed"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func (p *waitThenAnswerProvider) callCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

// TestRun_TurnCancelWhileItsCallWaitsParksTheRun — an operator cancelling the
// turn of an interactive run whose model call is still waiting (in a provider's
// lane, before anything streamed) stops that turn, exactly as a cancel during
// generation does: the run parks for the operator's next message and goes on
// from it. The call fails with the turn's cancellation; read as a provider
// error, it ended the whole run instead.
func TestRun_TurnCancelWhileItsCallWaitsParksTheRun(t *testing.T) {
	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	prov := &waitThenAnswerProvider{waiting: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, turnCancelOpts(prov, []tools.Tool{noopTool{}}, q, armCh, sink))
		done <- err
	}()

	tc0 := <-armCh
	<-prov.waiting
	tc0(ErrTurnCancelled)

	sink.waitFor(t, providers.EventTurnCancelled, 2*time.Second)
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)
	select {
	case err := <-done:
		t.Fatalf("the run ended (err=%v) on a turn-cancel while its call waited; it must park", err)
	default:
	}

	q <- steer.Message{Text: "continue"}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)
	if prov.callCount() != 2 {
		t.Fatalf("provider called %d times, want 2 (the cancelled call and the continued turn)", prov.callCount())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not end after a whole-run cancel")
	}
}

// slotProbeProvider answers end_turn and records whether the run's global slot
// was lent when the call was made.
type slotProbeProvider struct {
	mu   sync.Mutex
	lent []bool
}

func (p *slotProbeProvider) ID() string                                   { return "probe" }
func (p *slotProbeProvider) Probe(context.Context) error                  { return nil }
func (p *slotProbeProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *slotProbeProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *slotProbeProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.lent = append(p.lent, concurrency.RunSlotFrom(ctx).Lent())
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// TestRun_TurnCancelWhileItsCallWaitsTakesTheRunSlotBackBeforeItComputes — a
// turn call waiting for its lane lends the run's global slot. Cancelling that
// turn ends the wait under the cancelled turn ctx, so the lane cannot take the
// slot back when none is free: the run parks with its slot still lent. When the
// operator's next message arrives the run must hold a global slot again before
// it computes, whatever provider that turn is routed to — here, one retuned
// onto a provider no lane gates, which takes nothing back on its own.
func TestRun_TurnCancelWhileItsCallWaitsTakesTheRunSlotBackBeforeItComputes(t *testing.T) {
	sem := concurrency.New(1, 8, 0)
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"p": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	gated := lanegate.Wrap("p", &slotProbeProvider{}, sched)

	// Another call holds the lane's only slot for the whole test.
	_, releaseLane, err := lanegate.Hold(context.Background(), gated, providers.Request{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLane()

	// The run holds the runtime's only global slot.
	rs := &concurrency.RunSlot{}
	endRun, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "alice", Slot: rs})
	if err != nil {
		t.Fatal(err)
	}
	defer endRun()

	q := make(chan steer.Message, 4)
	armCh := make(chan context.CancelCauseFunc, 8)
	sink := newEventSink()
	retuned := &slotProbeProvider{}
	opts := turnCancelOpts(gated, []tools.Tool{noopTool{}}, q, armCh, sink)
	opts.ReResolveOnOperatorTurn = func(context.Context) (providers.Provider, string, string, bool, error) {
		return retuned, "y", "", true, nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = Run(concurrency.WithRunSlot(runCtx, rs), opts) }()

	tc0 := <-armCh
	deadline := time.Now().Add(3 * time.Second)
	for !rs.Lent() {
		if time.Now().After(deadline) {
			t.Fatal("the turn call never lent the run's slot while it waited for its lane")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Another run starts in the lent slot, so none is free when the turn ends.
	endOther, err := sem.Admit(context.Background(), concurrency.AdmitRequest{UserID: "bob", Reject: true})
	if err != nil {
		t.Fatalf("no run could start in the lent slot: %v", err)
	}
	tc0(ErrTurnCancelled)
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)

	// A slot is free again; the operator goes on.
	endOther()
	q <- steer.Message{Text: "continue"}
	sink.waitFor(t, providers.EventAwaitingInput, 2*time.Second)

	retuned.mu.Lock()
	lent := append([]bool(nil), retuned.lent...)
	retuned.mu.Unlock()
	if len(lent) != 1 {
		t.Fatalf("the retuned provider was called %d times, want 1", len(lent))
	}
	if lent[0] {
		t.Fatal("the continued turn computed with the run's global slot still lent")
	}
	if st := sem.Stats(); st.Active != 1 {
		t.Errorf("runtime active = %d, want the run's one", st.Active)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not end after a whole-run cancel")
	}
}
