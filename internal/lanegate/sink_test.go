package lanegate

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// reportsWait holds the only (p, kind) slot, starts call on a ctx carrying the
// run's event sink, and returns what the sink saw once the call has its slot.
func reportsWait(t *testing.T, sched *concurrency.CallScheduler, kind string, ctx func(context.Context) context.Context, call func(context.Context) error) []providers.Event {
	t.Helper()
	release, err := sched.Acquire(context.Background(), "p", kind, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	var log eventLog
	done := make(chan error, 1)
	go func() { done <- call(ctx(providers.WithEventSink(context.Background(), log.on))) }()
	waitKind(t, sched, kind, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	time.Sleep(10 * time.Millisecond) // a wait the admission can measure
	release()
	if err := recv(t, "the queued call", done); err != nil {
		t.Fatalf("call: %v", err)
	}
	return log.list()
}

func asRun(ctx context.Context) context.Context { return ctx }

// wantWaitReported checks a call_queued then a call_admitted for (p, kind).
func wantWaitReported(t *testing.T, evs []providers.Event, kind string) {
	t.Helper()
	if len(evs) != 2 || evs[0].Type != providers.EventCallQueued || evs[1].Type != providers.EventCallAdmitted {
		t.Fatalf("the run's sink saw %+v, want call_queued then call_admitted", evs)
	}
	if a := evs[0].Admission; a == nil || *a != (providers.AdmissionInfo{Reason: "lane", Position: 1, Provider: "p", Kind: kind}) {
		t.Errorf("call_queued payload = %+v, want lane, position 1, provider p, kind %s", a, kind)
	}
	if a := evs[1].Admission; a == nil || a.Provider != "p" || a.Kind != kind || a.WaitMs < 10 {
		t.Errorf("call_admitted payload = %+v, want provider p, kind %s, wait_ms >= 10", a, kind)
	}
}

// A decision a tool asks for a run reports its wait for the decision lane on
// the run's stream, through the event sink the run puts on its tools' ctx. It
// used to wait silently: only the loop's own model calls reported.
func TestDecision_ReportsItsWaitOnTheRunsEventSink(t *testing.T) {
	sched := kindLane(config.ModelKindDecision, concurrency.LaneConfig{MaxConcurrent: 1})
	evs := reportsWait(t, sched, config.ModelKindDecision, asRun, func(ctx context.Context) error {
		_, err := WrapDecision("p", &fakeDecision{}, sched).Decide(ctx, yesNo)
		return err
	})
	wantWaitReported(t, evs, config.ModelKindDecision)
}

// The same for an embedding (a Memory or Document search inside a tool call).
func TestEmbedder_ReportsItsWaitOnTheRunsEventSink(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	evs := reportsWait(t, sched, config.ModelKindEmbedder, asRun, func(ctx context.Context) error {
		_, err := WrapEmbedder("p", &fakeEmbedder{}, sched).Embed(ctx, []string{"a"})
		return err
	})
	wantWaitReported(t, evs, config.ModelKindEmbedder)
}

// A chat call made for a run with no Request.OnEvent — a summary, a listwise
// rerank — reports on the run's sink instead.
func TestCall_WithoutAnEventHookReportsOnTheRunsEventSink(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	evs := reportsWait(t, sched, config.ModelKindChat, asRun, func(ctx context.Context) error {
		p := &fakeProvider{next: make(chan providers.Event, 1)}
		p.next <- done()
		close(p.next)
		ch, err := Wrap("p", p, sched).Call(ctx, providers.Request{Model: "m"})
		if err != nil {
			return err
		}
		for range ch {
		}
		return nil
	})
	wantWaitReported(t, evs, config.ModelKindChat)
}

// Background work started from a run's ctx (a consolidation pass, an embedding
// fill) is not the run's work and may outlive it: its waits are not reported
// on the run.
func TestEmbedder_BackgroundWorkReportsNothingOnTheRun(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	evs := reportsWait(t, sched, config.ModelKindEmbedder, WithBackground, func(ctx context.Context) error {
		_, err := WrapEmbedder("p", &fakeEmbedder{}, sched).Embed(ctx, []string{"a"})
		return err
	})
	if len(evs) != 0 {
		t.Fatalf("background work reported %+v on the run", evs)
	}
}
