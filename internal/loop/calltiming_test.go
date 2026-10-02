package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/mock"
)

// fakeClock advances by the next step on every read after the first, so a
// timing test can say exactly how long each phase of a call took.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	steps []time.Duration
	reads int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads > 0 && c.reads-1 < len(c.steps) {
		c.now = c.now.Add(c.steps[c.reads-1])
	}
	c.reads++
	return c.now
}

// The timing measured at the provider call site must arrive on the usage event
// the ledger records — that event is the only thing that leaves the loop per
// call, so a timing stamped anywhere else would never be persisted.
func TestRun_UsageEventCarriesTheCallTiming(t *testing.T) {
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_MS", "0")
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS", "0")
	// Reads: call start, first event (+300ms), done (+1200ms).
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0), steps: []time.Duration{300 * time.Millisecond, 1200 * time.Millisecond}}

	var usage *providers.Usage
	_, err := Run(context.Background(), RunOptions{
		Provider: mock.New(),
		Model:    "mock-generic",
		Segments: []PromptSegment{
			{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hello"}}},
		},
		Now: clk.Now,
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventUsage {
				usage = ev.Usage
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if usage == nil || usage.Timing == nil {
		t.Fatalf("usage event carries no timing: %+v", usage)
	}
	if got := *usage.Timing; got.DurationMs != 1500 || got.TTFTMs != 300 {
		t.Fatalf("timing = %+v, want duration 1500ms and ttft 300ms", got)
	}
}

// A summary emits no usage event, so its timing reaches the throughput estimate
// only through the ctx observer — named with the provider and model that served
// it, or it could not be attributed.
func TestSummarize_ReportsTheCallToTheObserver(t *testing.T) {
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_MS", "0")
	t.Setenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS", "0")
	var seen *providers.Usage
	ctx := providers.WithCallObserver(context.Background(), func(u *providers.Usage) { seen = u })
	msgs := []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "a long conversation"}}}}
	if _, err := Summarize(ctx, mock.New(), "mock-generic", msgs, 30); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if seen == nil || seen.Timing == nil {
		t.Fatalf("observer saw %+v, want a timed usage", seen)
	}
	if seen.Provider != "mock" || seen.Model != "mock-generic" {
		t.Fatalf("observed call attributed to %q/%q, want mock/mock-generic", seen.Provider, seen.Model)
	}
}
