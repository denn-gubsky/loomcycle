package loop

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// waitingProvider is a model-driven provider (no run budget, no lifetime limit
// of its own) whose call never answers until its ctx ends. hadClock records
// whether the run it served was given a clock.
type waitingProvider struct {
	hadClock atomic.Bool
	wall     atomic.Int64 // the clock's lifetime limit, ns
}

func (p *waitingProvider) ID() string                    { return "fake" }
func (p *waitingProvider) Probe(_ context.Context) error { return nil }
func (p *waitingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"fake-model"}, nil
}
func (p *waitingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *waitingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	if clk := providers.RunClockFromContext(ctx); clk != nil {
		p.hadClock.Store(true)
		p.wall.Store(int64(clk.WallLimit()))
	}
	ch := make(chan providers.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func wallOpts(p providers.Provider, seconds int) RunOptions {
	return RunOptions{Provider: p, Model: "fake", MaxWallSeconds: seconds,
		Segments: []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}}
}

// A model-driven run with max_wall_seconds is ended by it: the loop calls
// OnWallLimit once the run has lived that long, and the cancel it makes ends
// the run.
func TestRun_MaxWallSecondsCallsOnWallLimitForAModelDrivenRun(t *testing.T) {
	p := &waitingProvider{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var fired atomic.Int32
	opts := wallOpts(p, 1)
	opts.OnWallLimit = func() {
		fired.Add(1)
		cancel(errors.New("cancelled by the server"))
	}
	start := time.Now()
	_, err := Run(ctx, opts)
	lived := time.Since(start)

	if fired.Load() != 1 {
		t.Fatalf("OnWallLimit fired %d times, want 1", fired.Load())
	}
	if lived < 900*time.Millisecond || lived > 4*time.Second {
		t.Errorf("the run lived %s, want about 1s", lived)
	}
	// The server's cancel ended it, not the loop's own: no wall-limit failure.
	if errors.Is(err, errWallLimit) {
		t.Errorf("err = %v; a run its OnWallLimit cancelled must not also fail as a wall limit", err)
	}
	if !p.hadClock.Load() || time.Duration(p.wall.Load()) != time.Second {
		t.Errorf("the run's clock: present=%v limit=%s, want a 1s limit on it", p.hadClock.Load(), time.Duration(p.wall.Load()))
	}
}

// With nothing to cancel it, the loop ends the run itself and says why.
func TestRun_MaxWallSecondsWithNoCancellerFailsTheRun(t *testing.T) {
	_, err := Run(context.Background(), wallOpts(&waitingProvider{}, 1))
	if !errors.Is(err, errWallLimit) || !strings.Contains(err.Error(), "max_wall_seconds") {
		t.Fatalf("err = %v, want a wall-limit failure naming max_wall_seconds", err)
	}
}

// The limit holds even when the cancel it asked for never lands.
func TestRun_MaxWallSecondsStillEndsARunItsCancelDidNotReach(t *testing.T) {
	opts := wallOpts(&waitingProvider{}, 1)
	opts.wallCancelGrace = 200 * time.Millisecond
	opts.OnWallLimit = func() {} // a registry that no longer knows the run
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), opts)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errWallLimit) {
			t.Errorf("err = %v, want the loop's own wall-limit failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run outlived its limit: nothing ended it when the cancel did not land")
	}
}

// A resumed run keeps the lifetime it had already used.
func TestRun_MaxWallSecondsCountsTheLifetimeARunResumesWith(t *testing.T) {
	opts := wallOpts(&waitingProvider{}, 60)
	opts.RunClockCarry = providers.RunClockState{Wall: 2 * time.Minute}
	start := time.Now()
	_, err := Run(context.Background(), opts)
	if !errors.Is(err, errWallLimit) {
		t.Fatalf("err = %v, want the run ended at once: it had already lived past its limit", err)
	}
	if lived := time.Since(start); lived > 3*time.Second {
		t.Errorf("a run already past its limit lived another %s", lived)
	}
}

// A run with no limit of its own gets no clock, as before: it must neither
// inherit nor pause a parent's.
func TestRun_NoMaxWallSecondsGivesAModelDrivenRunNoClock(t *testing.T) {
	p := &waitingProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	parent := providers.WithRunClock(ctx, providers.NewRunClock(time.Now(), providers.RunClockState{}))
	_, _ = Run(parent, wallOpts(p, 0))
	if p.hadClock.Load() {
		t.Error("a run with no max_wall_seconds saw a clock — its parent's leaked to it")
	}
}
