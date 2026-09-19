package loop

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// thinkingOnlyProvider reproduces the live failure: a thinking model given a
// small budget spends it reasoning and emits NO EventText.
//
// summarizeWith accumulates only EventText, so this returns ("", nil) — the
// err==nil-and-empty branch, which used to return did=false with no event, no
// error, and no marker of any kind. The observed session logged 1293 thinking
// events and zero recap markers.
type thinkingOnlyProvider struct{ calls int }

func (p *thinkingOnlyProvider) ID() string                                   { return "thinking-only" }
func (p *thinkingOnlyProvider) Probe(context.Context) error                  { return nil }
func (p *thinkingOnlyProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *thinkingOnlyProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsThinking: true}
}
func (p *thinkingOnlyProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.calls++
	ch := make(chan providers.Event, 3)
	ch <- providers.Event{Type: providers.EventThinking, Text: "let me consider how best to summarise"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

func declinesFrom(evs []providers.Event) []*providers.ContextDistillDeclinedInfo {
	var out []*providers.ContextDistillDeclinedInfo
	for _, e := range evs {
		if e.Type == providers.EventContextDistillDeclined && e.ContextDistill != nil {
			out = append(out, e.ContextDistill)
		}
	}
	return out
}

// The headline case. A recap that produces nothing must SAY so — this is the
// exact branch that let a live chat climb to the top of its window in silence.
func TestMaybeRecap_EmptySummary_EmitsDeclined(t *testing.T) {
	var evs []providers.Event
	prov := &thinkingOnlyProvider{}
	opts := RunOptions{
		Provider: prov, Model: "x",
		Context: &config.Context{KeepLastN: cptr(2), Reasoning: cptr("recap")},
	}
	_, did := maybeRecap(context.Background(), opts, distillableConvo(), 32768,
		func(e providers.Event) { evs = append(evs, e) }, "auto")
	if did {
		t.Fatal("a recap that produced no text must not report success")
	}
	d := declinesFrom(evs)
	if len(d) != 1 {
		t.Fatalf("want exactly one decline event, got %d: %+v", len(d), evs)
	}
	if d[0].Reason != providers.DistillDeclineEmptySummary {
		t.Errorf("reason = %q, want %q — a thinking model returning no text is the "+
			"empty-summary case, not a failure (there is no error to read)",
			d[0].Reason, providers.DistillDeclineEmptySummary)
	}
	if d[0].Mode != "recap" {
		t.Errorf("mode = %q, want recap — which block of config to edit depends on it", d[0].Mode)
	}
	// The message must name the fix. "declined" sends the reader to the source.
	if !strings.Contains(d[0].Message, "recap_max_chars") {
		t.Errorf("message does not name the knob to change: %q", d[0].Message)
	}
	// No EventError: there was no error. Reporting one would send the operator
	// looking for a failure that did not happen.
	for _, e := range evs {
		if e.Type == providers.EventError {
			t.Errorf("an empty summary is not an error: %q", e.Error)
		}
	}
}

// The competing branch, which must be DISTINGUISHABLE from the one above —
// that indistinguishability is the defect this work exists to remove.
func TestMaybeRecap_SplitDeclined_EmitsDeclined(t *testing.T) {
	var evs []providers.Event
	// 7 messages with keep_last_n 6: keep-N spans everything after the pinned
	// first turn, so there is nothing left to summarise.
	msgs := []providers.Message{
		userMsg("the task"), asstMsg("a1"), userMsg("q2"),
		asstMsg("a2"), userMsg("q3"), asstMsg("a3"), userMsg("q4"),
	}
	opts := RunOptions{
		Provider: &steerProvider{}, Model: "x",
		Context: &config.Context{KeepLastN: cptr(6), Reasoning: cptr("recap")},
	}
	_, did := maybeRecap(context.Background(), opts, msgs, 32768,
		func(e providers.Event) { evs = append(evs, e) }, "auto")
	if did {
		t.Fatal("split declined, so no distillation happened")
	}
	d := declinesFrom(evs)
	if len(d) != 1 {
		t.Fatalf("want one decline, got %d: %+v", len(d), evs)
	}
	if d[0].Reason != providers.DistillDeclineSplitDeclined {
		t.Fatalf("reason = %q, want %q", d[0].Reason, providers.DistillDeclineSplitDeclined)
	}
	// These two numbers ARE the diagnosis — without them the operator cannot
	// tell which way to move keep_last_n.
	if d[0].Messages != 7 || d[0].KeepLastN != 6 {
		t.Errorf("messages/keep_last_n = %d/%d, want 7/6 — the payload must carry "+
			"the two numbers that explain the refusal", d[0].Messages, d[0].KeepLastN)
	}
	if !strings.Contains(d[0].Message, "keep_last_n") {
		t.Errorf("message does not name the knob: %q", d[0].Message)
	}
}

// reasoning:keep is not a fault, but "nothing happened" must never be silent.
func TestMaybeRecap_ReasoningKeep_EmitsDeclined(t *testing.T) {
	var evs []providers.Event
	opts := RunOptions{
		Provider: &steerProvider{}, Model: "x",
		Context: &config.Context{KeepLastN: cptr(2), Reasoning: cptr("keep")},
	}
	if _, did := maybeRecap(context.Background(), opts, distillableConvo(), 32768,
		func(e providers.Event) { evs = append(evs, e) }, "auto"); did {
		t.Fatal("keep mode distils nothing")
	}
	d := declinesFrom(evs)
	if len(d) != 1 || d[0].Reason != providers.DistillDeclineReasoningKeep {
		t.Fatalf("want one reasoning_keep decline, got %+v", d)
	}
}

// growingProvider returns a summary LONGER than the span it replaces, so the
// distillation would make the context bigger. The live session did exactly this
// (14230 -> 14334) and applied it, because both numbers were measured and
// neither was compared.
type growingProvider struct{ calls int }

func (p *growingProvider) ID() string                                   { return "growing" }
func (p *growingProvider) Probe(context.Context) error                  { return nil }
func (p *growingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *growingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *growingProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.calls++
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText,
		Text: strings.Repeat("a summary that is somehow longer than what it replaces. ", 40)}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// A distillation that does not shrink must be REFUSED, not applied — and the
// span it would have dropped must not be harvested, because it is being kept.
func TestMaybeRecap_RefusesWhenResultIsNotSmaller(t *testing.T) {
	var evs []providers.Event
	harvested := 0
	opts := RunOptions{
		Provider: &growingProvider{}, Model: "x",
		Context: &config.Context{KeepLastN: cptr(2), Reasoning: cptr("recap"),
			HarvestToMemory: cptr(true)},
		BankCompactedSpan: func(context.Context, []providers.Message) (string, error) {
			harvested++
			return "mp_x", nil
		},
	}
	in := distillableConvo()
	out, did := maybeRecap(context.Background(), opts, in, 32768,
		func(e providers.Event) { evs = append(evs, e) }, "auto")
	if did {
		t.Fatal("a result that is not smaller must be refused, not applied")
	}
	if len(out) != len(in) {
		t.Errorf("the history was modified by a refused distillation: %d -> %d", len(in), len(out))
	}
	d := declinesFrom(evs)
	if len(d) != 1 || d[0].Reason != providers.DistillDeclineNotSmaller {
		t.Fatalf("want one not_smaller decline, got %+v", d)
	}
	// Both counts must ride the event: "14230 -> 14334" is the whole
	// explanation, and a bare refusal is not actionable.
	if d[0].BeforeTokens == 0 || d[0].AfterTokens == 0 || d[0].AfterTokens < d[0].BeforeTokens {
		t.Errorf("before/after = %d/%d, want both set with after >= before",
			d[0].BeforeTokens, d[0].AfterTokens)
	}
	// ⚠️ The harvest must NOT have run. It used to sit above the measurement,
	// so a declined recap banked a span it then kept — and with the gate
	// re-firing each iteration it banked the same span repeatedly.
	if harvested != 0 {
		t.Errorf("a refused distillation banked its span %d time(s) — the span is "+
			"still in the history, so banking it duplicates live content", harvested)
	}
}

// A decline that is a property of the CONFIGURATION cannot change within a run,
// so it must be reported once — not once per iteration, which would bury it in
// its own noise and burn a summarize call every iteration.
func TestRun_DistillDeclineIsNotRepeatedEveryIteration(t *testing.T) {
	var mu sync.Mutex
	var evs []providers.Event
	// recapCountingProvider reports the high footprint on EVERY ordinary turn,
	// so the threshold stays crossed for the whole run. A fake that reports it
	// only on turn 0 lets the gate fire exactly once no matter what the dedup
	// does — which makes this test pass without testing anything.
	prov := &recapCountingProvider{firstIn: 164000, maxCtx: 200000}
	q := make(chan steer.Message, 8)
	parked := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := config.ContextModeRecap
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, RunOptions{
			Provider: prov, Model: "x",
			Tools:      []tools.Tool{noopTool{}},
			Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:   steerSegs(),
			SteerQueue: q, Interactive: true,
			// keep_last_n 99 pins everything: the split declines permanently,
			// so every iteration over the threshold reaches the same verdict.
			Context: &config.Context{Mode: &m, KeepLastN: cptr(99), AutoRecapAtPct: cptr(50)},
			OnEvent: func(ev providers.Event) {
				mu.Lock()
				evs = append(evs, ev)
				mu.Unlock()
				if ev.Type == providers.EventAwaitingInput {
					parked <- struct{}{}
				}
			},
		})
	}()
	waitPark := func(what string) {
		t.Helper()
		select {
		case <-parked:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}
	// Drive several real turns. Each one re-enters the gate above the
	// threshold and reaches the same permanently-declining split.
	waitPark("initial park")
	for i := 0; i < 3; i++ {
		q <- steer.Message{Text: "continue"}
		waitPark("re-park")
	}
	cancel()
	<-done

	mu.Lock()
	d := declinesFrom(evs)
	mu.Unlock()
	prov.mu.Lock()
	calls := prov.turn
	prov.mu.Unlock()
	if len(d) != 1 {
		t.Fatalf("a configuration-bound decline was reported %d times over %d turns; "+
			"it must be reported once per (mode, reason) per run or it buries itself: %+v",
			len(d), calls, d)
	}
	if d[0].Reason != providers.DistillDeclineSplitDeclined {
		t.Errorf("reason = %q, want %q", d[0].Reason, providers.DistillDeclineSplitDeclined)
	}
}

// recapCountingProvider answers normal turns with text and the RECAP call with
// thinking only — reproducing the live empty-summary decline — while counting
// how many recap attempts were made.
//
// The recap call is identified by its system prompt, which is the only thing
// that distinguishes it from an ordinary turn at this seam.
type recapCountingProvider struct {
	mu         sync.Mutex
	recapCalls int
	firstIn    int
	maxCtx     int
	turn       int
}

func (p *recapCountingProvider) ID() string                                   { return "recap-counting" }
func (p *recapCountingProvider) Probe(context.Context) error                  { return nil }
func (p *recapCountingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *recapCountingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *recapCountingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	isRecap := false
	for _, b := range req.System {
		if strings.Contains(b.Text, "running RECAP of an agent's progress") {
			isRecap = true
		}
	}
	p.mu.Lock()
	if isRecap {
		p.recapCalls++
	}
	p.turn++
	p.mu.Unlock()
	// Every ORDINARY turn reports the same high footprint, so the threshold
	// stays crossed for the whole run. A fake that reports it only on turn 0
	// lets the gate fire once no matter what the debounce does — which makes a
	// debounce test pass without testing anything.
	in := 0
	if !isRecap {
		in = p.firstIn
	}

	ch := make(chan providers.Event, 2)
	if isRecap {
		// Thinking only: summarizeWith collects no text → empty summary.
		ch <- providers.Event{Type: providers.EventThinking, Text: "considering"}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: in}}
	close(ch)
	return ch, nil
}

// The OTHER half of the gate fix, and the one the dedup does NOT cover.
//
// Deduping the EVENT stops the transcript filling up; it does nothing about the
// work. A decline used to leave lastCompactIter untouched, so the gate re-fired
// on the very next iteration and paid for a summarize call every iteration for
// the rest of the run — against a condition that could not change. That is a
// real provider bill, and it is invisible in the event stream precisely because
// the event is deduped.
func TestRun_ADecliningDistillDoesNotBurnACallEveryIteration(t *testing.T) {
	prov := &recapCountingProvider{firstIn: 164000, maxCtx: 200000}
	q := make(chan steer.Message, 8)
	parked := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := config.ContextModeRecap
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, RunOptions{
			Provider: prov, Model: "x",
			Tools:      []tools.Tool{noopTool{}},
			Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:   bulkyRecapSegs(),
			SteerQueue: q, Interactive: true,
			// keep_last_n 0 always splits, so the gate reaches the summarizer
			// and the decline happens AFTER a call has been paid for.
			Context: &config.Context{Mode: &m, KeepLastN: cptr(0), AutoRecapAtPct: cptr(50)},
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventAwaitingInput {
					parked <- struct{}{}
				}
			},
		})
	}()
	waitPark := func(what string) {
		t.Helper()
		select {
		case <-parked:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}
	const turns = 4
	waitPark("initial park")
	for i := 0; i < turns; i++ {
		q <- steer.Message{Text: "continue"}
		waitPark("re-park")
	}
	cancel()
	<-done

	prov.mu.Lock()
	got := prov.recapCalls
	prov.mu.Unlock()
	// The debounce is one iteration wide, so over N turns above the threshold
	// the gate may attempt at most every other one. Without the fix this is
	// one attempt per turn.
	if max := (turns / 2) + 1; got > max {
		t.Errorf("the declining recap was attempted %d times over %d turns (max %d): "+
			"the debounce is not advancing on a DECLINE, so every iteration pays for "+
			"a summarize call against a condition that cannot change", got, turns, max)
	}
	if got == 0 {
		t.Fatal("the recap was never attempted — the fixture no longer exercises the gate")
	}
}

// selfReadingTool calls Context op=self the way an agent would, and records
// what the run's iteration context carried at that moment.
type selfReadingTool struct {
	mu   sync.Mutex
	seen []tools.LastDistillValue
}

func (s *selfReadingTool) Name() string        { return "peek" }
func (s *selfReadingTool) Description() string { return "records the run's distill state" }
func (s *selfReadingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (s *selfReadingTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	s.mu.Lock()
	s.seen = append(s.seen, tools.LastDistill(ctx))
	s.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// toolThenTextProvider alternates: a tool_use turn (so the tool runs and can
// read the iteration context) then a text turn that parks.
//
// It reports the high footprint on EVERY turn, so the recap gate stays open for
// the whole run rather than firing once.
type toolThenTextProvider struct {
	mu      sync.Mutex
	turn    int
	firstIn int
	maxCtx  int
}

func (p *toolThenTextProvider) ID() string                                   { return "tool-then-text" }
func (p *toolThenTextProvider) Probe(context.Context) error                  { return nil }
func (p *toolThenTextProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *toolThenTextProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *toolThenTextProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	turn := p.turn
	p.turn++
	p.mu.Unlock()

	ch := make(chan providers.Event, 2)
	if turn%2 == 0 {
		ch <- providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "t", Name: "peek", Input: json.RawMessage(`{}`)}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use",
			Usage: &providers.Usage{InputTokens: p.firstIn}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
			Usage: &providers.Usage{InputTokens: p.firstIn}}
	}
	close(ch)
	return ch, nil
}

// THE CROSSING: a decline recorded in the loop must reach a TOOL's context.
//
// The unit tests either side of this prove the loop emits the event and that
// op=self renders a value placed on ctx. Neither covers the carry between them,
// and that carry is exactly where this kind of work goes wrong — the value is
// produced, rendered, and never joined up.
//
// It also pins the property the dedup must not break: the event fires once, but
// the CONDITION is reported on every subsequent turn.
func TestRun_ADeclineReachesTheToolContext(t *testing.T) {
	peek := &selfReadingTool{}
	prov := &toolThenTextProvider{firstIn: 164000, maxCtx: 200000}
	q := make(chan steer.Message, 8)
	parked := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := config.ContextModeRecap
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, RunOptions{
			Provider: prov, Model: "x",
			Tools:      []tools.Tool{peek},
			Dispatcher: tools.NewDispatcher([]tools.Tool{peek}),
			Segments:   bulkyRecapSegs(),
			SteerQueue: q, Interactive: true,
			// keep_last_n 99 pins everything → a permanent split decline.
			Context: &config.Context{Mode: &m, KeepLastN: cptr(99), AutoRecapAtPct: cptr(50)},
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventAwaitingInput {
					parked <- struct{}{}
				}
			},
		})
	}()
	waitPark := func(what string) {
		t.Helper()
		select {
		case <-parked:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}
	waitPark("initial park")
	for i := 0; i < 3; i++ {
		q <- steer.Message{Text: "continue"}
		waitPark("re-park")
	}
	cancel()
	<-done

	peek.mu.Lock()
	seen := append([]tools.LastDistillValue(nil), peek.seen...)
	peek.mu.Unlock()

	var withDecline int
	for _, v := range seen {
		if v.Reason != "" {
			withDecline++
			if v.Reason != providers.DistillDeclineSplitDeclined {
				t.Errorf("tool saw reason %q, want %q", v.Reason, providers.DistillDeclineSplitDeclined)
			}
			if !strings.Contains(v.Message, "keep_last_n") {
				t.Errorf("tool saw a decline with no actionable message: %q", v.Message)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("the tool was never called — the fixture no longer exercises the carry")
	}
	if withDecline == 0 {
		t.Fatalf("the tool was called %d time(s) and never saw the decline: the loop "+
			"records it and op=self renders it, but nothing joins the two", len(seen))
	}
}

// The invariant, enforced rather than reviewed.
//
// "maybeRecap and maybeAutoCompact may not return did=false without emitting"
// is only useful if adding a bare return breaks something. Left to review it
// lasts exactly until the first busy week — and the cost of losing it is the
// silent climb this whole change exists to remove.
//
// Read from the SOURCE rather than by calling the functions, because the thing
// being asserted is a property of how they are written: there is no input that
// proves no future exit will skip the helper.
func TestDistillers_HaveNoSilentDeclinePath(t *testing.T) {
	src, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatalf("read loop.go: %v", err)
	}
	text := string(src)
	bare := regexp.MustCompile(`return\s+messages,\s*false`)

	for _, fn := range []string{"func maybeRecap(", "func maybeAutoCompact("} {
		i := strings.Index(text, fn)
		if i < 0 {
			t.Fatalf("%s not found — did it move or get renamed? This guard is now "+
				"watching nothing, which is worse than not having it.", fn)
		}
		end := strings.Index(text[i:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not find the end of %s", fn)
		}
		body := text[i : i+end]
		if got := bare.FindAllString(body, -1); len(got) > 0 {
			t.Errorf("%s has %d bare `return messages, false` — every decline must go "+
				"through declineDistill, or it is invisible to the operator. That "+
				"silence is the defect this file exists to prevent.", fn, len(got))
		}
		// Non-vacuity: the function must actually contain declines, or the
		// check above passes because there is nothing to find.
		if !strings.Contains(body, "declineDistill(") {
			t.Errorf("%s contains no declineDistill call — the guard above would "+
				"pass on an empty function", fn)
		}
	}
}
