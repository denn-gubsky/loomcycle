package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// (endTurnProvider lives in steer_test.go — one text + end_turn per Call, with
// a calls() counter — reused here as the single-clean-turn provider.)

// fakePauseGate parks the loop on the FIRST iteration boundary, then releases
// it when release() is called — exercising the RunOptions.PauseGate seam.
type fakePauseGate struct {
	mu       sync.Mutex
	parked   bool
	releaseC chan struct{}
	requests int
}

func newFakePauseGate() *fakePauseGate { return &fakePauseGate{releaseC: make(chan struct{})} }

func (g *fakePauseGate) PauseRequested() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	return g.requests == 1 // pause only on the first boundary
}

func (g *fakePauseGate) Park(ctx context.Context) error {
	g.mu.Lock()
	g.parked = true
	g.mu.Unlock()
	select {
	case <-g.releaseC:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *fakePauseGate) isParked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parked
}

func (g *fakePauseGate) release() { close(g.releaseC) }

func userSeg(text string) []PromptSegment {
	return []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: text}}}}
}

// TestRun_ParksAtBoundaryWhenPaused asserts the loop calls PauseGate.Park at
// the iteration boundary when a pause is requested, blocks there (before the
// provider call), and resumes to end_turn once the gate releases.
func TestRun_ParksAtBoundaryWhenPaused(t *testing.T) {
	gate := newFakePauseGate()
	prov := &endTurnProvider{}
	disp := tools.NewDispatcher(nil)

	done := make(chan struct{})
	var res RunResult
	var runErr error
	go func() {
		res, runErr = Run(context.Background(), RunOptions{
			Provider:        prov,
			Model:           "x",
			Dispatcher:      disp,
			Segments:        userSeg("go"),
			ToolParallelism: 8,
			PauseGate:       gate,
		})
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for !gate.isParked() {
		select {
		case <-done:
			t.Fatal("run finished without parking — PauseGate.Park was never called")
		case <-deadline:
			t.Fatal("timed out waiting for the loop to park")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	// The park sits BEFORE the provider call.
	if prov.calls() != 0 {
		t.Errorf("provider called %d times while parked; want 0", prov.calls())
	}

	gate.release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish after the gate released")
	}
	if runErr != nil {
		t.Fatalf("Run after resume: %v", runErr)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", res.StopReason)
	}
	if prov.calls() != 1 {
		t.Errorf("provider called %d times after resume; want 1", prov.calls())
	}
}

// TestRun_PauseGateCancelledWhileParked asserts cancelling the run ctx while
// parked unblocks Park and the loop exits cleanly (no provider call).
func TestRun_PauseGateCancelledWhileParked(t *testing.T) {
	gate := newFakePauseGate()
	prov := &endTurnProvider{}
	disp := tools.NewDispatcher(nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = Run(ctx, RunOptions{
			Provider:        prov,
			Model:           "x",
			Dispatcher:      disp,
			Segments:        userSeg("go"),
			ToolParallelism: 8,
			PauseGate:       gate,
		})
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for !gate.isParked() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for park")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after ctx cancel while parked")
	}
	if runErr == nil {
		t.Error("want a non-nil error (ctx cancelled while parked)")
	}
	if prov.calls() != 0 {
		t.Errorf("provider called %d times; want 0 (cancelled while parked)", prov.calls())
	}
}
