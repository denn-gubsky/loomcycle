package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// endRecorder is a provider that keeps something per run: it records the run
// identity each call carried and every RunDone it is told.
type endRecorder struct {
	fakeProvider
	mu    sync.Mutex
	metas []providers.RunMeta
	ended []string
}

func (e *endRecorder) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	meta, _ := providers.RunMetaFromContext(ctx)
	e.mu.Lock()
	e.metas = append(e.metas, meta)
	e.mu.Unlock()
	return e.fakeProvider.Call(ctx, req)
}

func (e *endRecorder) RunDone(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended = append(e.ended, runID)
}

// wrapped stands in for the registry's wrapper: it implements nothing beyond
// Provider and Unwrap.
type wrapped struct{ providers.Provider }

func (w wrapped) Unwrap() providers.Provider { return w.Provider }

// TestRun_TellsItsProviderTheRunEnded — a provider's call lane keeps a slot
// reserved for a run between its calls, under the id the calls carry
// (RunMeta.RunID). When the run ends the loop says so, once, with that same
// id — through the registry's wrapper, and whether the run succeeded or not —
// or the slot would sit reserved for a run that will never call again.
//
// The calls also carry the run's tenant and user: the lane takes turns by them.
func TestRun_TellsItsProviderTheRunEnded(t *testing.T) {
	ident := tools.RunIdentityValue{AgentID: "agent-7", TenantID: "acme", UserID: "alice"}
	for _, tc := range []struct {
		name      string
		responses [][]providers.Event
		wantErr   bool
	}{
		{"completed run", [][]providers.Event{endTurn("done")}, false},
		{"failed run", nil, true}, // no scripted response: the first call fails
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &endRecorder{fakeProvider: fakeProvider{responses: tc.responses}}
			ctx := tools.WithRunIdentity(context.Background(), ident)
			_, err := Run(ctx, RunOptions{
				Provider: wrapped{rec},
				Model:    "x",
				Segments: []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run err = %v, wantErr=%v", err, tc.wantErr)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.ended) != 1 || rec.ended[0] != "agent-7" {
				t.Fatalf("RunDone calls = %v, want exactly [agent-7]", rec.ended)
			}
			if len(rec.metas) == 0 {
				t.Fatal("the provider was never called")
			}
			for _, m := range rec.metas {
				if m.RunID != "agent-7" {
					t.Errorf("a call carried RunID %q, want agent-7 — RunDone would name a run the lane never saw", m.RunID)
				}
				if m.TenantID != "acme" || m.UserID != "alice" {
					t.Errorf("a call carried tenant=%q user=%q, want acme/alice", m.TenantID, m.UserID)
				}
			}
		})
	}
}

// laneWaitProvider is a provider whose Call waits — as a call queued in a
// provider's lane does — until its ctx ends, and then fails with the ctx's
// error. Nothing is ever streamed.
type laneWaitProvider struct {
	fakeProvider
	waiting chan struct{}
	once    sync.Once
}

func (p *laneWaitProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.once.Do(func() { close(p.waiting) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRun_TurnCancelledWhileItsCallWaitsDoesNotMarkTheModelStalled — a call
// can sit in a provider's lane for as long as the queue is, so an operator
// cancelling the turn of a queued run is an ordinary event. The call then fails
// with the turn's cancellation, which says nothing about the model: marking it
// stalled would take a healthy model out of routing for every other run.
func TestRun_TurnCancelledWhileItsCallWaitsDoesNotMarkTheModelStalled(t *testing.T) {
	prov := &laneWaitProvider{waiting: make(chan struct{})}
	armCh := make(chan context.CancelCauseFunc, 4)
	sink := newEventSink()
	var mu sync.Mutex
	var stalled, limited []string
	opts := RunOptions{
		Provider:    prov,
		Model:       "x",
		Segments:    steerSegs(),
		SteerQueue:  make(chan steer.Message, 1),
		Interactive: true,
		OnEvent:     sink.emit,
		ArmTurnCancel: func(tc context.CancelCauseFunc) func() {
			armCh <- tc
			return func() {}
		},
		MarkStalled: func(provider, model, reason string) {
			mu.Lock()
			defer mu.Unlock()
			stalled = append(stalled, provider+"/"+model+": "+reason)
		},
		MarkRateLimited: func(provider, model string, _ time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			limited = append(limited, provider+"/"+model)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { defer close(finished); _, _ = Run(ctx, opts) }()

	cancelTurn := <-armCh
	<-prov.waiting
	cancelTurn(ErrTurnCancelled)
	// The run parks for its operator; end it so the feedback is final.
	sink.waitFor(t, providers.EventAwaitingInput, 5*time.Second)
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end after a whole-run cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stalled) != 0 || len(limited) != 0 {
		t.Fatalf("a cancelled turn gave the resolver feedback about the model: stalled=%v rate-limited=%v", stalled, limited)
	}
}
