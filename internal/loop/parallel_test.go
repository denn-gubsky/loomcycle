package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// slowTool sleeps for delay ms and reports its start/finish times so
// tests can assert real concurrency from wall-clock data. ms is read
// from input as `{"ms": <int>}`.
type slowTool struct {
	mu       sync.Mutex
	starts   map[string]time.Time
	finishes map[string]time.Time
	maxLive  int // peak concurrent in-flight calls observed
	live     int
}

func newSlowTool() *slowTool {
	return &slowTool{
		starts:   make(map[string]time.Time),
		finishes: make(map[string]time.Time),
	}
}

func (t *slowTool) Name() string                 { return "Slow" }
func (t *slowTool) Description() string          { return "" }
func (t *slowTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *slowTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		ID string `json:"id"`
		MS int    `json:"ms"`
	}
	_ = json.Unmarshal(input, &args)

	t.mu.Lock()
	t.live++
	if t.live > t.maxLive {
		t.maxLive = t.live
	}
	t.starts[args.ID] = time.Now()
	t.mu.Unlock()

	delay := time.Duration(args.MS) * time.Millisecond
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}

	t.mu.Lock()
	t.finishes[args.ID] = time.Now()
	t.live--
	t.mu.Unlock()

	return tools.Result{Text: "done:" + args.ID}, nil
}

// scriptedProvider drives one assistant turn with N tool_calls, then a
// terminal end_turn turn. Caller controls how many tool_calls are
// emitted and what their inputs are.
type scriptedProvider struct {
	toolCalls []providers.ToolUse
	calls     int
	// requests captures every providers.Request the loop hands to
	// Call(), in order. Used by TestParallelDispatch_PreservesMessageOrdering
	// to inspect the messages array on turn 2 (the one that carries the
	// tool_result content blocks) and assert tool_call ordering survives
	// the parallel dispatch.
	requests []providers.Request
	mu       sync.Mutex
}

func (p *scriptedProvider) ID() string                    { return "scripted" }
func (p *scriptedProvider) Probe(_ context.Context) error { return nil }
func (p *scriptedProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"scripted-model"}, nil
}
func (p *scriptedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *scriptedProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	turn := p.calls
	p.calls++
	p.requests = append(p.requests, req)
	ch := make(chan providers.Event, len(p.toolCalls)+2)
	if turn == 0 {
		ch <- providers.Event{Type: providers.EventText, Text: "spawning"}
		for _, tu := range p.toolCalls {
			tu := tu
			ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &tu}
		}
		ch <- providers.Event{
			Type:       providers.EventDone,
			StopReason: "tool_use",
			Usage:      &providers.Usage{},
		}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "all done"}
		ch <- providers.Event{
			Type:       providers.EventDone,
			StopReason: "end_turn",
			Usage:      &providers.Usage{},
		}
	}
	close(ch)
	return ch, nil
}

func makePending(n int, delayMs int) []providers.ToolUse {
	out := make([]providers.ToolUse, n)
	for i := 0; i < n; i++ {
		out[i] = providers.ToolUse{
			ID:    "call_" + strconv.Itoa(i),
			Name:  "Slow",
			Input: json.RawMessage(fmt.Sprintf(`{"id":"call_%d","ms":%d}`, i, delayMs)),
		}
	}
	return out
}

// TestParallelDispatch_RunsConcurrently_NotSerial pins the headline
// behaviour: 3 tool_calls with 100 ms each must finish in roughly
// 100 ms (not 300 ms). A serial dispatch would force ~300 ms.
func TestParallelDispatch_RunsConcurrently_NotSerial(t *testing.T) {
	pending := makePending(3, 100)
	tool := newSlowTool()
	prov := &scriptedProvider{toolCalls: pending}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	t0 := time.Now()
	res, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 8,
	})
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", res.StopReason)
	}
	// Wall-clock budget: 100 ms each tool, fully parallel = ~100 ms.
	// Allow generous slack (300 ms) to absorb scheduler noise on CI;
	// a serial dispatch would be ≥ 300 ms even after slack.
	if elapsed > 300*time.Millisecond {
		t.Errorf("dispatch took %v, want < 300ms (serial would be ≥ 300ms; parallelism appears broken)", elapsed)
	}
	// Peak in-flight must reach 3 (all three running together at
	// some moment); a serial dispatch would peak at 1.
	if tool.maxLive < 3 {
		t.Errorf("peak concurrent tools = %d, want ≥ 3 (parallel dispatch did not engage)", tool.maxLive)
	}
}

// TestParallelDispatch_PreservesMessageOrdering pins the contract
// that the message handed back to the model lists tool_results in
// tool_call order, even when tools finish out of order. The
// assertion is on the ACTUAL request the loop hands to the provider
// on turn 2 (the one carrying the tool_result content blocks),
// captured by scriptedProvider.requests.
func TestParallelDispatch_PreservesMessageOrdering(t *testing.T) {
	// Tool 0 medium, tool 1 slowest, tool 2 fastest → completion
	// order [2, 0, 1] differs from tool_call order [0, 1, 2]. The
	// indexed-slot write in executePendingTools must restore the
	// original ordering before the message is appended.
	pending := []providers.ToolUse{
		{ID: "call_0", Name: "Slow", Input: json.RawMessage(`{"id":"call_0","ms":50}`)},
		{ID: "call_1", Name: "Slow", Input: json.RawMessage(`{"id":"call_1","ms":80}`)},
		{ID: "call_2", Name: "Slow", Input: json.RawMessage(`{"id":"call_2","ms":10}`)},
	}
	tool := newSlowTool()
	prov := &scriptedProvider{toolCalls: pending}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	_, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 8,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Sanity guard: the test only exercises the reorder path when
	// finish times actually separate as planned. If scheduler noise
	// produces a different finish order on this run, skip rather
	// than report a misleading pass.
	tool.mu.Lock()
	if !(tool.finishes["call_2"].Before(tool.finishes["call_0"]) &&
		tool.finishes["call_0"].Before(tool.finishes["call_1"])) {
		tool.mu.Unlock()
		t.Skipf("scheduler did not separate finish times this run "+
			"(call_2=%v call_0=%v call_1=%v); cannot assert reordering",
			tool.finishes["call_2"], tool.finishes["call_0"], tool.finishes["call_1"])
	}
	tool.mu.Unlock()

	// Pull turn 2's request — that's the one carrying the user
	// message with all three tool_result blocks.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) < 2 {
		t.Fatalf("turn 2 not captured (got %d Call invocations)", len(prov.requests))
	}
	turn2 := prov.requests[1]

	// Find the user message containing tool_results and pull their
	// IDs in wire order. We expect three blocks correlated to
	// call_0/call_1/call_2 in that exact order.
	var ids []string
	for _, msg := range turn2.Messages {
		for _, c := range msg.Content {
			if c.Type == "tool_result" {
				ids = append(ids, c.ToolUseID)
			}
		}
	}
	want := []string{"call_0", "call_1", "call_2"}
	if len(ids) != len(want) {
		t.Fatalf("tool_result block count = %d, want %d (got %v)", len(ids), len(want), ids)
	}
	for i, got := range ids {
		if got != want[i] {
			t.Errorf("tool_result[%d] = %q, want %q (full order %v; reorder bug — "+
				"results slice was written in completion order, not tool_call order)",
				i, got, want[i], ids)
		}
	}
}

// TestParallelDispatch_RespectsParallelismCap pins that the bound
// is honoured: with parallelism=2 and 4 pending tools, peak
// in-flight is exactly 2.
func TestParallelDispatch_RespectsParallelismCap(t *testing.T) {
	pending := makePending(4, 60)
	tool := newSlowTool()
	prov := &scriptedProvider{toolCalls: pending}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	_, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tool.maxLive > 2 {
		t.Errorf("peak concurrent tools = %d, want ≤ 2 (cap leaked)", tool.maxLive)
	}
	if tool.maxLive < 2 {
		t.Errorf("peak concurrent tools = %d, want = 2 (cap underused; parallelism not engaging at all)", tool.maxLive)
	}
}

// TestParallelDispatch_SerialFallbackWhenCapIsOne pins that
// parallelism=1 produces strict serial behaviour — useful for debug
// and as a back-stop for users who want deterministic ordering.
func TestParallelDispatch_SerialFallbackWhenCapIsOne(t *testing.T) {
	pending := makePending(3, 30)
	tool := newSlowTool()
	prov := &scriptedProvider{toolCalls: pending}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	_, err := Run(context.Background(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tool.maxLive != 1 {
		t.Errorf("peak concurrent tools = %d, want = 1 (serial fallback failed)", tool.maxLive)
	}
}

// TestParallelDispatch_ContextCancelPropagates pins that a parent
// cancellation tears down all in-flight tool goroutines, not just
// the next one to start. Without ctx-aware semaphore acquisition,
// goroutines waiting on a saturated cap would leak past cancel.
func TestParallelDispatch_ContextCancelPropagates(t *testing.T) {
	pending := makePending(8, 500)
	tool := newSlowTool()
	prov := &scriptedProvider{toolCalls: pending}
	disp := tools.NewDispatcher([]tools.Tool{tool})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	t0 := time.Now()
	_, _ = Run(ctx, RunOptions{
		Provider:        prov,
		Model:           "x",
		Tools:           []tools.Tool{tool},
		Dispatcher:      disp,
		Segments:        []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		ToolParallelism: 2,
	})
	elapsed := time.Since(t0)

	// 8 tools × 500 ms each, parallelism 2 → ~2 s if cancel didn't
	// propagate to in-flight tools. If cancel propagates, we should
	// finish well under 1 s (the in-flight tools see ctx.Done() and
	// return early via their select).
	if elapsed > 1*time.Second {
		t.Errorf("Run took %v after 50ms cancel; goroutines did not observe ctx.Done()", elapsed)
	}
}
