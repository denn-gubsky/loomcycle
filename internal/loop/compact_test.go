package loop

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func cptr[T any](v T) *T { return &v }

func userMsg(text string) providers.Message {
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: text}}}
}
func asstMsg(text string) providers.Message {
	return providers.Message{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: text}}}
}
func toolResultMsg() providers.Message {
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", Text: "r"}}}
}

// CompactionSplit keeps at least keepLastN, snapped to a clean (fresh user-turn)
// boundary so a tool_use/tool_result pair is never split, and pins the first
// user turn when keep_first.
func TestCompactionSplit(t *testing.T) {
	// u0 a1 u2 a3 u4 a5 — fresh user turns at 0,2,4.
	msgs := []providers.Message{userMsg("0"), asstMsg("1"), userMsg("2"), asstMsg("3"), userMsg("4"), asstMsg("5")}
	// keepLastN=2, keepFirst=true: firstIdx=1; target=4 (fresh user turn) → cut=4.
	firstIdx, cut, ok := CompactionSplit(msgs, 2, true)
	if !ok || firstIdx != 1 || cut != 4 {
		t.Fatalf("got firstIdx=%d cut=%d ok=%v, want 1/4/true", firstIdx, cut, ok)
	}
	// keepLastN=0 → keep none (cut=len), summarize all after firstIdx.
	if _, cut, ok := CompactionSplit(msgs, 0, false); !ok || cut != 6 {
		t.Errorf("keepLastN=0: got cut=%d ok=%v, want 6/true", cut, ok)
	}
	// Don't split a tool cycle: a tool_result user turn at the target index isn't
	// a clean boundary, so cut snaps back to the previous fresh user turn.
	tcyc := []providers.Message{userMsg("0"), asstMsg("tool_use"), toolResultMsg(), asstMsg("3")}
	// target=4-1=3 (index 3 is assistant, not a fresh user) → snap back to 0... but
	// firstIdx=1 (keepFirst), so no clean boundary in (1,3] → cut=len (keep none).
	if fi, cut, ok := CompactionSplit(tcyc, 1, true); !ok || cut != len(tcyc) || fi != 1 {
		t.Errorf("tool-cycle: got fi=%d cut=%d ok=%v, want 1/%d/true (no clean mid-boundary)", fi, cut, ok, len(tcyc))
	}
	// Too short → no-op.
	if _, _, ok := CompactionSplit([]providers.Message{userMsg("0")}, 4, true); ok {
		t.Error("single-message convo should be a no-op (ok=false)")
	}
}

func TestShouldAutoCompact(t *testing.T) {
	on := &config.Compaction{Enabled: cptr(true), AutoCompactAtPct: cptr(80)}
	// used/window = 850/1000 = 85% >= 80 → fire (iter past the debounce).
	if !shouldAutoCompact(on, 850, 1000, 5, -2) {
		t.Error("85% should fire")
	}
	// below threshold
	if shouldAutoCompact(on, 700, 1000, 5, -2) {
		t.Error("70% should not fire")
	}
	// disabled
	if shouldAutoCompact(&config.Compaction{Enabled: cptr(false)}, 900, 1000, 5, -2) {
		t.Error("disabled should not fire")
	}
	// unknown window
	if shouldAutoCompact(on, 900, 0, 5, -2) {
		t.Error("unknown window (0) should not fire")
	}
	// debounce: just compacted last iteration
	if shouldAutoCompact(on, 900, 1000, 3, 2) {
		t.Error("one-iteration debounce should suppress")
	}
	// nil
	if shouldAutoCompact(nil, 900, 1000, 5, -2) {
		t.Error("nil compaction should not fire")
	}
}

// maybeAutoCompact summarizes the middle inline and keeps the pinned task + the
// last-N tail (the auto/self path computes the summary itself).
func TestMaybeAutoCompact_SummarizesAndKeepsTail(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	opts := RunOptions{
		Provider:   &steerProvider{}, // Call returns text "ok" → summary="ok"
		Model:      "x",
		Compaction: &config.Compaction{KeepLastN: cptr(2), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
	}
	var compacted bool
	out, did := maybeAutoCompact(context.Background(), opts, msgs, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("expected compaction to happen")
	}
	compacted = did
	_ = compacted
	// [user(pinned task + summary), assistant(ack), <last 2 verbatim>]
	if len(out) != 4 {
		t.Fatalf("got %d messages, want 4 (summary pair + 2 kept): %+v", len(out), out)
	}
	if out[0].Role != "user" || !strings.Contains(out[0].Content[0].Text, "the task") {
		t.Errorf("first turn should pin the task verbatim: %q", out[0].Content[0].Text)
	}
	if out[2].Content[0].Text != "q3" || out[3].Content[0].Text != "a3" {
		t.Errorf("last-2 tail not kept verbatim: %+v", out[2:])
	}
}

// CompactionMessages is the single source of truth for the compacted history
// shape (the live loop AND replayTranscript both build it). It must start on a
// user turn and end on an assistant turn so the operator's next input alternates
// cleanly (Anthropic et al. require start-with-user + role alternation).
func TestCompactionMessages_Shape(t *testing.T) {
	tail := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "recent question"}}},
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "recent answer"}}},
	}
	msgs := CompactionMessages("THE TASK", "THE SUMMARY", tail)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (summary pair + 2 kept tail)", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("first role = %q, want user (providers require start-with-user)", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content[0].Text, "THE SUMMARY") || !strings.Contains(msgs[0].Content[0].Text, "THE TASK") {
		t.Errorf("pinned task + summary not both embedded in the user turn: %q", msgs[0].Content[0].Text)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", msgs[1].Role)
	}
	if msgs[2].Role != "user" || msgs[2].Content[0].Text != "recent question" {
		t.Errorf("kept tail not appended verbatim: %+v", msgs[2])
	}
}

// A steer.KindCompact control delivered to a PARKED interactive run replaces the
// in-memory conversation with the summary pair and RE-parks (no provider call),
// and the next real operator turn's request carries ONLY the compacted history.
// Fail-before: pre-feature, a compact-kind message was appended as a user turn
// (history kept growing) and triggered a provider call.
func TestRun_Interactive_CompactReplacesHistory(t *testing.T) {
	q := make(chan steer.Message, 4)
	parked := make(chan struct{}, 8)
	compacted := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &steerProvider{} // inject nil → always end_turn; records every request
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			OnEvent: func(ev providers.Event) {
				switch ev.Type {
				case providers.EventAwaitingInput:
					parked <- struct{}{}
				case providers.EventContextCompaction:
					compacted <- struct{}{}
				}
			},
		})
		close(done)
	}()

	waitOn := func(ch <-chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}
	calls := func() int { prov.mu.Lock(); defer prov.mu.Unlock(); return len(prov.requests) }

	waitOn(parked, "initial end_turn park")
	if got := calls(); got != 1 {
		t.Fatalf("calls=%d before compact, want 1", got)
	}

	// Compact while parked → emits context_compaction + re-parks, NO provider call.
	q <- steer.Message{Kind: steer.KindCompact, Text: "SUMMARY-X"}
	waitOn(compacted, "compaction applied")
	if got := calls(); got != 1 {
		t.Errorf("compact triggered a provider call (calls=%d, want 1 — must just re-park)", got)
	}

	// A real operator turn now → exactly one provider call on the compacted history.
	q <- steer.Message{Text: "continue"}
	waitOn(parked, "re-park after the real turn")
	if got := calls(); got != 2 {
		t.Fatalf("calls=%d after the real turn, want 2", got)
	}
	prov.mu.Lock()
	last := append([]providers.Message(nil), prov.requests[len(prov.requests)-1]...)
	prov.mu.Unlock()

	txt := func(m providers.Message) string {
		var b strings.Builder
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
		return b.String()
	}
	if len(last) != 3 {
		t.Fatalf("compacted request has %d messages, want 3 (summary pair + the new turn): %+v", len(last), last)
	}
	if last[0].Role != "user" || !strings.Contains(txt(last[0]), "SUMMARY-X") {
		t.Errorf("msg[0] = %q (%s); want the summary user turn", txt(last[0]), last[0].Role)
	}
	if last[1].Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", last[1].Role)
	}
	if last[2].Role != "user" || txt(last[2]) != "continue" {
		t.Errorf("msg[2] = %q (%s); want user 'continue'", txt(last[2]), last[2].Role)
	}
	for _, m := range last {
		if txt(m) == "go" {
			t.Errorf("the original pre-compaction turn survived: %+v", m)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not terminate after cancel")
	}
}

// ctxUsageProvider records the context footprint visible on ctx at each Call —
// exactly what a Context op=self invoked that turn would report (the loop hands
// the provider the same iterCtx the tools get). The FIRST call reports a large
// InputTokens usage so lastCtxTokens becomes large; every call ends the turn.
type ctxUsageProvider struct {
	mu       sync.Mutex
	seenUsed []int
	firstIn  int
	maxCtx   int
	turn     int
}

func (p *ctxUsageProvider) ID() string                                   { return "ctxusage-test" }
func (p *ctxUsageProvider) Probe(context.Context) error                  { return nil }
func (p *ctxUsageProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *ctxUsageProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *ctxUsageProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.seenUsed = append(p.seenUsed, tools.ContextUsage(ctx).Used)
	turn := p.turn
	p.turn++
	p.mu.Unlock()

	in := 0
	if turn == 0 {
		in = p.firstIn
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: in}}
	close(ch)
	return ch, nil
}

// A parked interactive run that compacts (steer.KindCompact) must refresh the
// context footprint so the NEXT operator turn's Context op=self reports the
// compacted (small) size — not the stale pre-compaction footprint.
//
// Fail-before: the loop never refreshed lastCtxTokens on compaction, so op=self
// kept reporting the old ~full context (e.g. 164k / 82%) for a whole turn even
// though the real wire request had already shrunk — the reported bug.
func TestRun_Interactive_ContextUsageRefreshedAfterCompaction(t *testing.T) {
	q := make(chan steer.Message, 4)
	parked := make(chan struct{}, 8)
	compacted := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &ctxUsageProvider{firstIn: 164000, maxCtx: 200000}
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			OnEvent: func(ev providers.Event) {
				switch ev.Type {
				case providers.EventAwaitingInput:
					parked <- struct{}{}
				case providers.EventContextCompaction:
					compacted <- struct{}{}
				}
			},
		})
		close(done)
	}()
	waitOn := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}

	// Turn 0: a real turn that reports a large footprint (164k), then parks.
	waitOn(parked, "initial end_turn park")
	// Compact while parked → shrinks the in-memory history; re-parks (no call).
	q <- steer.Message{Kind: steer.KindCompact, Text: "SUMMARY-X"}
	waitOn(compacted, "compaction applied")
	// A real operator turn now → exactly one provider call on the compacted history.
	q <- steer.Message{Text: "continue"}
	waitOn(parked, "re-park after the real turn")

	prov.mu.Lock()
	seen := append([]int(nil), prov.seenUsed...)
	prov.mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want >=2 provider calls, got %d (%v)", len(seen), seen)
	}
	post := seen[len(seen)-1]
	if post >= prov.firstIn {
		t.Fatalf("post-compaction Context op=self footprint = %d; want the small compacted size, not the stale pre-compaction %d", post, prov.firstIn)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not terminate after cancel")
	}
}
