package loop

import (
	"context"
	"encoding/json"
	"fmt"
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
	// ⚠️ ONE PER KEY, not one in total. With the second tier in place a single
	// gate opening can decline TWICE — the mode's distiller and then compaction
	// — for different reasons, and those are genuinely different news. What
	// must never happen is the SAME (mode, reason, severity) repeating across
	// turns, which is the burying this dedup exists to prevent.
	seen := map[string]int{}
	for _, x := range d {
		seen[x.Mode+"|"+x.Reason+"|"+x.Severity]++
	}
	for key, n := range seen {
		if n > 1 {
			t.Errorf("%q was reported %d times over %d turns; a configuration-bound "+
				"condition must be reported once per run or it buries itself", key, n, calls)
		}
	}
	if len(d) == 0 {
		t.Fatal("no declines at all — the fixture no longer exercises the gate")
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
			// WHICH reason is not the point — the second tier means it may be
			// the mode's distiller or compaction, whichever declined last. What
			// this test is about is the CARRY: a value recorded in the loop
			// reaching a tool's context at all.
			if v.Message == "" {
				t.Errorf("tool saw reason %q with no message — the fix does not survive "+
					"the carry, which is most of what makes it useful", v.Reason)
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

// singleTurnProvider answers once at end_turn — the shape of a continuation
// that replies without calling a tool. It records the message count of each
// request so a test can see whether the history was distilled BEFORE the send.
type singleTurnProvider struct {
	mu       sync.Mutex
	sentMsgs []int
	maxCtx   int
	reply    string
}

func (p *singleTurnProvider) ID() string                                   { return "single-turn" }
func (p *singleTurnProvider) Probe(context.Context) error                  { return nil }
func (p *singleTurnProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *singleTurnProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *singleTurnProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.sentMsgs = append(p.sentMsgs, len(req.Messages))
	p.mu.Unlock()
	text := p.reply
	if text == "" {
		text = "ok"
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: text}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// B-iii, the iteration-0 hole: a run that answers in ONE iteration must still
// be able to distil, because the gate runs before the request is sent.
//
// The footprint used to be zero until a call RETURNED, so a continuation
// replaying a large history had no opportunity at all — it sent the whole thing
// and finished. The observed session's last run sent 30100 tokens of a 32768
// window this way and reclaimed nothing.
func TestRun_SingleIterationOverThresholdDistils(t *testing.T) {
	// A prior history well over the threshold: ~40 messages of real bulk, of
	// which keep_last_n 2 keeps only the tail.
	var prior []providers.Message
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			prior = append(prior, userMsg(bulky("q")))
		} else {
			prior = append(prior, asstMsg(bulky("a")))
		}
	}
	// The estimate must actually clear the threshold, or this test proves
	// nothing about the gate.
	est := estimateMessageTokens(prior)
	window := est * 2 // the history alone is ~50% of the window
	if est == 0 {
		t.Fatal("fixture has no weight")
	}

	prov := &singleTurnProvider{maxCtx: window, reply: "short"}
	m := config.ContextModeRecap
	var recapped int
	var mu sync.Mutex
	_, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:         []tools.Tool{noopTool{}},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:      steerSegs(),
		PriorMessages: prior,
		Context: &config.Context{Mode: &m, KeepLastN: cptr(2),
			AutoRecapAtPct: cptr(50), Reasoning: cptr("drop")},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextRecap {
				mu.Lock()
				recapped++
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	got := recapped
	mu.Unlock()
	if got == 0 {
		t.Fatal("a single-iteration run over the threshold did not distil — the gate " +
			"runs at the TOP of the iteration, so an unseeded footprint makes this " +
			"impossible however full the replayed prompt is")
	}

	prov.mu.Lock()
	sent := append([]int(nil), prov.sentMsgs...)
	prov.mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("the provider was never called")
	}
	// The distillation must happen BEFORE the send, not after it: the point is
	// that the oversized prompt never leaves.
	if sent[0] >= len(prior) {
		t.Errorf("the first request carried %d messages of a %d-message history — "+
			"the distillation did not take effect before the send", sent[0], len(prior))
	}
}

// The seed must not fire the gate on a SMALL history. A run that is nowhere
// near its window distilling at start would throw away context for nothing,
// and chars/4 overcounts, so this is the direction to check.
func TestRun_SeedDoesNotDistilAShortHistory(t *testing.T) {
	prov := &singleTurnProvider{maxCtx: 200000}
	m := config.ContextModeRecap
	var recapped int
	var mu sync.Mutex
	_, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:         []tools.Tool{noopTool{}},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:      steerSegs(),
		PriorMessages: distillableConvo(),
		Context:       &config.Context{Mode: &m, KeepLastN: cptr(2), AutoRecapAtPct: cptr(50)},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextRecap {
				mu.Lock()
				recapped++
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if recapped != 0 {
		t.Errorf("distilled %d time(s) on a history far below the threshold — the "+
			"seed is firing the gate when it should not", recapped)
	}
}

// effectiveWindow is shared by the seed and the per-turn update precisely so
// they cannot disagree about how full the window is.
func TestEffectiveWindow_BudgetOnlyLowers(t *testing.T) {
	cap200k := &singleTurnProvider{maxCtx: 200000}
	for _, tc := range []struct {
		name             string
		reported, budget int
		want             int
	}{
		{"static capability when nothing reported", 0, 0, 200000},
		{"a per-call report wins over the capability", 131072, 0, 131072},
		{"a budget lowers it", 0, 32768, 32768},
		{"a budget cannot enlarge a fixed window", 0, 999999, 200000},
		{"a budget lowers a per-call report too", 131072, 8192, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveWindow(tc.reported, RunOptions{Provider: cap200k, MaxContextTokens: tc.budget})
			if got != tc.want {
				t.Errorf("effectiveWindow(%d, budget=%d) = %d, want %d",
					tc.reported, tc.budget, got, tc.want)
			}
		})
	}
}

// fatTool has a description and schema of realistic size, so a catalogue of
// them weighs what a real one weighs.
type fatTool struct{ n int }

func (f fatTool) Name() string        { return fmt.Sprintf("tool%d", f.n) }
func (f fatTool) Description() string { return strings.Repeat("a tool the agent may call. ", 20) }
func (f fatTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"` +
		strings.Repeat("x", 300) + `"}}}`)
}
func (f fatTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "ok"}, nil
}

// billingProvider reports what a provider ACTUALLY bills — system + tools +
// messages — rather than the conversation alone.
type billingProvider struct {
	mu     sync.Mutex
	billed []int
	maxCtx int
}

func (p *billingProvider) ID() string                                   { return "billing" }
func (p *billingProvider) Probe(context.Context) error                  { return nil }
func (p *billingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *billingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *billingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	in := estimatePreambleTokens(req.System, req.Tools) + estimateMessageTokens(req.Messages)
	p.mu.Lock()
	p.billed = append(p.billed, in)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
		Usage: &providers.Usage{InputTokens: in}}
	close(ch)
	return ch, nil
}

// ⚠️ THE INVERSE OF TestRun_SingleIterationOverThresholdDistils, and the shape
// that shipped broken.
//
// That test uses a 40-message history, so the message-only estimate crosses the
// threshold and the gate opens — the half of B-iii that worked. This one is a
// SHORT conversation whose tokens are almost entirely system prompt and tool
// catalogue, which is the chat/local shape: a small window with a large
// preamble.
//
// Found in production, not by the suite: a 2048-token window measured 2 tokens
// of conversation against a real request of 3340, so the run sent 163% of its
// window with the gate shut and no decline.
//
// A gate driven by an estimate needs a fixture where the estimate DISAGREES
// with the real figure, or it only ever proves the agreeing case.
func TestRun_FootprintCountsTheSystemPromptAndTools(t *testing.T) {
	var toolset []tools.Tool
	for i := 0; i < 12; i++ {
		toolset = append(toolset, fatTool{n: i})
	}
	segs := []PromptSegment{
		{Role: "system", Content: []PromptContentBlock{{Type: "trusted-text",
			Text: strings.Repeat("You are a helpful assistant with a long operator preamble. ", 40)}}},
		{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi there"}}},
	}
	_, fresh := splitSegments(segs)
	msgOnly := estimateMessageTokens(fresh)

	prov := &billingProvider{maxCtx: 2048}
	m := config.ContextModeRecap
	var frames int
	var mu sync.Mutex
	_, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      toolset,
		Dispatcher: tools.NewDispatcher(toolset),
		Segments:   segs,
		// reasoning:keep short-circuits FIRST in maybeRecap, before the split
		// and before any summariser call — so an opened gate emits a decline
		// unconditionally. That makes this test read the GATE, not the
		// distiller: zero frames can only mean the gate stayed shut.
		Context:          &config.Context{Mode: &m, Reasoning: cptr("keep"), AutoRecapAtPct: cptr(50)},
		MaxContextTokens: 2048,
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextDistillDeclined {
				mu.Lock()
				frames++
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Non-vacuity: the fixture must actually be the disagreeing shape, or this
	// test proves nothing about the estimator.
	if msgOnly*100 >= 2048*50 {
		t.Fatalf("fixture is not the disagreeing shape: messages alone are %d tokens, "+
			"already over the 50%% threshold", msgOnly)
	}
	prov.mu.Lock()
	billed := append([]int(nil), prov.billed...)
	prov.mu.Unlock()
	if len(billed) == 0 || billed[0]*100 < 2048*50 {
		t.Fatalf("fixture does not cross the threshold on the REAL count: billed %v", billed)
	}

	mu.Lock()
	got := frames
	mu.Unlock()
	if got == 0 {
		t.Errorf("the gate never opened: the conversation alone is %d tokens (%.1f%%) "+
			"but the real request is %d (%.1f%%) — the footprint is ignoring the system "+
			"prompt and tool catalogue, which on a small window IS the request",
			msgOnly, float64(msgOnly)*100/2048, billed[0], float64(billed[0])*100/2048)
	}
}

func exhaustedFrom(evs []providers.Event) []*providers.ContextExhaustedInfo {
	var out []*providers.ContextExhaustedInfo
	for _, e := range evs {
		if e.Type == providers.EventContextExhausted && e.ContextExhausted != nil {
			out = append(out, e.ContextExhausted)
		}
	}
	return out
}

// A decline that leaves the window reclaimable is INFO; one that cannot is a
// WARNING. The distinction is what makes the channel worth reading: if every
// decline were a warning, none of them would be.
func TestDeclineSeverity_OnlySplitDeclinedWarns(t *testing.T) {
	for reason, want := range map[string]string{
		providers.DistillDeclineSplitDeclined:   providers.DistillSeverityWarning,
		providers.DistillDeclineEmptySummary:    providers.DistillSeverityInfo,
		providers.DistillDeclineNotSmaller:      providers.DistillSeverityInfo,
		providers.DistillDeclineReasoningKeep:   providers.DistillSeverityInfo,
		providers.DistillDeclineSummarizeFailed: providers.DistillSeverityInfo,
	} {
		if got := declineSeverity(reason); got != want {
			t.Errorf("declineSeverity(%q) = %q, want %q", reason, got, want)
		}
	}
}

// The severity must ride the emitted event, not just exist as a function —
// a consumer branches on the payload.
func TestMaybeRecap_SplitDeclineCarriesWarningSeverity(t *testing.T) {
	var evs []providers.Event
	msgs := []providers.Message{
		userMsg("the task"), asstMsg("a1"), userMsg("q2"),
		asstMsg("a2"), userMsg("q3"), asstMsg("a3"), userMsg("q4"),
	}
	opts := RunOptions{
		Provider: &steerProvider{}, Model: "x",
		Context: &config.Context{KeepLastN: cptr(6), Reasoning: cptr("recap")},
	}
	maybeRecap(context.Background(), opts, msgs, 32768,
		func(e providers.Event) { evs = append(evs, e) }, "auto")
	d := declinesFrom(evs)
	if len(d) != 1 {
		t.Fatalf("want one decline, got %+v", d)
	}
	if d[0].Severity != providers.DistillSeverityWarning {
		t.Errorf("severity = %q, want warning — keep_last_n pinning the conversation "+
			"means this path will decline identically forever", d[0].Severity)
	}
	// ⚠️ The message must name WHICH keep_last_n. Recap reads
	// context.keep_last_n; an operator sent to compaction.keep_last_n edits a
	// setting that was not the problem and watches the window keep filling.
	if !strings.Contains(d[0].Message, "context.keep_last_n") {
		t.Errorf("message does not qualify the key: %q", d[0].Message)
	}
	if strings.Contains(d[0].Message, "compaction.keep_last_n") {
		t.Errorf("recap decline names the COMPACTION key: %q", d[0].Message)
	}
}

// The compaction branch must name the other one.
func TestMaybeAutoCompact_SplitDeclineNamesTheCompactionKey(t *testing.T) {
	var evs []providers.Event
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2")}
	opts := RunOptions{
		Provider: &steerProvider{}, Model: "x",
		Compaction: &config.Compaction{KeepLastN: cptr(6), KeepFirst: cptr(true)},
	}
	maybeAutoCompact(context.Background(), opts, msgs, 32768,
		func(e providers.Event) { evs = append(evs, e) }, "auto")
	d := declinesFrom(evs)
	if len(d) != 1 {
		t.Fatalf("want one decline, got %+v", d)
	}
	if !strings.Contains(d[0].Message, "compaction.keep_last_n") {
		t.Errorf("message does not qualify the key: %q", d[0].Message)
	}
}

// ⚠️ THE GUARANTEE: the window is either reclaimed, or the run says clearly
// that it cannot be.
//
// The runtime cannot promise to reclaim — keep_last_n can pin an entire
// conversation and there is then nothing to distil. What it can promise is
// never to fail silently, which is what this event is.
//
// The fixture is the unreclaimable one deliberately: keep_last_n pins every
// message, so the split declines and no amount of retrying changes it.
func TestRun_ExhaustionIsReportedWhenNothingCanReclaim(t *testing.T) {
	prov := &recapCountingProvider{firstIn: 30000, maxCtx: 32768}
	q := make(chan steer.Message, 8)
	parked := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := config.ContextModeRecap
	var mu sync.Mutex
	var evs []providers.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, RunOptions{
			Provider: prov, Model: "x",
			Tools:      []tools.Tool{noopTool{}},
			Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:   steerSegs(),
			SteerQueue: q, Interactive: true,
			// keep_last_n 99 pins everything → permanently declining split.
			Context: &config.Context{Mode: &m, KeepLastN: cptr(99), AutoRecapAtPct: cptr(50)},
			// 30000/32768 = 91%, above the 80 default backstop.
			MaxContextTokens: 32768,
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

	mu.Lock()
	ex := exhaustedFrom(evs)
	mu.Unlock()
	if len(ex) == 0 {
		t.Fatal("the window was never reclaimed and the run said nothing — this is the " +
			"silent climb the whole line exists to remove")
	}
	// Banded, not per-iteration. The footprint is constant across these turns,
	// so the condition is equally true every time the gate opens — reporting it
	// each time would bury it in itself, which is the failure the decline dedup
	// already had to solve.
	if len(ex) > 1 {
		t.Errorf("reported exhaustion %d times at a constant footprint; it must be "+
			"banded, or the report buries itself", len(ex))
	}
	e := ex[0]
	if e.UsedPct < 80 {
		t.Errorf("used_pct = %d, want >= the backstop threshold", e.UsedPct)
	}
	if e.UsedTokens == 0 || e.WindowTokens == 0 {
		t.Errorf("exhaustion report carries no numbers: %+v", e)
	}
	// It must carry what was TRIED, not only that the window is full — "the
	// mechanism ran and refused" and "nothing ran" need opposite responses.
	if len(e.Verdicts) == 0 {
		t.Error("no tier verdicts — a reader cannot tell a refusal from an absence")
	} else if e.Verdicts[0].Reason != providers.DistillDeclineSplitDeclined {
		t.Errorf("verdict reason = %q, want split_declined", e.Verdicts[0].Reason)
	}
	// ⚠️ EVERY tier's fix must survive into the report, not just the last.
	// With the second tier in place this run declines TWICE for different
	// reasons — recap on context.keep_last_n, compaction on
	// compaction.keep_last_n — and an operator handed only one fixes half the
	// problem and watches the window keep filling.
	for _, want := range []string{"context.keep_last_n", "compaction.keep_last_n"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the report drops %s — it carries only some tiers' fixes: %q", want, e.Message)
		}
	}
	if len(e.Verdicts) < 2 {
		t.Errorf("only %d verdict(s): the second tier should have been tried and "+
			"refused too, and the report must show both attempts", len(e.Verdicts))
	}
}

// A run comfortably inside its window must NOT be told it is exhausted. This is
// the direction that turns the event into noise, and noise is how a channel
// that matters gets ignored.
func TestRun_NoExhaustionBelowTheBackstop(t *testing.T) {
	prov := &recapCountingProvider{firstIn: 1000, maxCtx: 200000} // 0.5%
	q := make(chan steer.Message, 8)
	parked := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := config.ContextModeRecap
	var mu sync.Mutex
	var evs []providers.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, RunOptions{
			Provider: prov, Model: "x",
			Tools:      []tools.Tool{noopTool{}},
			Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:   steerSegs(),
			SteerQueue: q, Interactive: true,
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
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("initial park: timed out")
	}
	q <- steer.Message{Text: "continue"}
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("re-park: timed out")
	}
	cancel()
	<-done

	mu.Lock()
	ex := exhaustedFrom(evs)
	mu.Unlock()
	if len(ex) != 0 {
		t.Errorf("reported exhaustion at 0.5%% of the window: %+v", ex[0])
	}
}

// ⚠️ THE SECOND TIER — the change this phase exists for.
//
// A recap that declines used to leave nothing else to try, so the run climbed
// to the provider's limit with compaction never consulted. The fixture makes
// recap decline for a reason compaction does NOT share: a thinking model
// returns no text within the recap budget (empty_summary), while compaction
// runs on a different prompt and a different budget and succeeds.
//
// That asymmetry is the whole argument for compaction being the right last
// resort. A second tier that failed for the same reasons would be theatre.
type recapFailsCompactionWorksProvider struct {
	mu        sync.Mutex
	recapCall int
	compCall  int
	inTokens  int
	maxCtx    int
}

func (p *recapFailsCompactionWorksProvider) ID() string                  { return "asym" }
func (p *recapFailsCompactionWorksProvider) Probe(context.Context) error { return nil }
func (p *recapFailsCompactionWorksProvider) ListModels(context.Context) ([]string, error) {
	return nil, nil
}
func (p *recapFailsCompactionWorksProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *recapFailsCompactionWorksProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	// Each distiller is identified by a phrase unique to ITS prompt. A loose
	// match like "compact" also hits an ordinary turn whose tool guide mentions
	// Context op=compact — which then reports 0 input tokens, the footprint
	// never rises, and the gate never opens at all.
	isRecap, isCompact := false, false
	for _, b := range req.System {
		if strings.Contains(b.Text, "running RECAP of an agent's progress") {
			isRecap = true
		}
		if strings.Contains(b.Text, "You are compacting a conversation to free up") {
			isCompact = true
		}
	}
	p.mu.Lock()
	if isRecap {
		p.recapCall++
	} else if isCompact {
		p.compCall++
	}
	p.mu.Unlock()

	ch := make(chan providers.Event, 2)
	switch {
	case isRecap:
		// Thinking only: summarizeWith collects no text → empty_summary.
		ch <- providers.Event{Type: providers.EventThinking, Text: "considering"}
	case isCompact:
		ch <- providers.Event{Type: providers.EventText, Text: "a compact summary"}
	default:
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	}
	in := 0
	if !isRecap && !isCompact {
		in = p.inTokens
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
		Usage: &providers.Usage{InputTokens: in}}
	close(ch)
	return ch, nil
}

func TestRun_RecapDeclineFallsThroughToCompaction(t *testing.T) {
	// A real history, so BOTH tiers get past the split and actually reach their
	// summarisers. A short conversation declines at the split for both, which
	// proves nothing about the fall-through — the first version of this test
	// did exactly that and passed its own premise check by accident.
	var prior []providers.Message
	for i := 0; i < 30; i++ {
		if i%2 == 0 {
			prior = append(prior, userMsg(bulky("q")))
		} else {
			prior = append(prior, asstMsg(bulky("a")))
		}
	}
	// The history must exceed the BACKSTOP threshold (80% by default), not just
	// the recap threshold. The backstop sits ABOVE the primary by design — a
	// fixture between the two exercises recap and correctly never reaches
	// compaction, which is the mistake the first version of this test made.
	window := estimateMessageTokens(prior) * 100 / 90 // ~90% of the window
	m := config.ContextModeRecap

	prov := &recapFailsCompactionWorksProvider{maxCtx: window}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:         []tools.Tool{noopTool{}},
		Dispatcher:    tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:      steerSegs(),
		PriorMessages: prior,
		Context:       &config.Context{Mode: &m, KeepLastN: cptr(2), AutoRecapAtPct: cptr(50)},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	prov.mu.Lock()
	recapN, compN := prov.recapCall, prov.compCall
	prov.mu.Unlock()
	if recapN == 0 {
		t.Fatal("recap was never attempted — the fixture no longer exercises the primary")
	}
	if compN == 0 {
		t.Error("recap declined and compaction was NEVER TRIED: the run has no second " +
			"tier, so a declining recap leaves the window to fill to the provider's limit")
	}
}

// ⚠️ C4 — the window beats a pinning keep_last_n.
//
// The declined-split early return never reached capKeptTailToWindow, so a run
// whose kept tail alone exceeded the window had no escape: keep_last_n could
// veto every distillation path and the run climbed to the provider's limit.
// That was deferred until the second tier made it load-bearing — a backstop
// keep_last_n can veto is not a backstop.
//
// The precedence is now explicit: keep_last_n is a PREFERENCE about how much to
// keep verbatim; the window is a HARD LIMIT. A preference does not override a
// limit.
func TestSplitOrCutToWindow_TheWindowBeatsAPinningKeepLastN(t *testing.T) {
	var msgs []providers.Message
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			msgs = append(msgs, userMsg(bulky("q")))
		} else {
			msgs = append(msgs, asstMsg(bulky("a")))
		}
	}
	total := estimateMessageTokens(msgs)

	// keep_last_n 99 pins every message: CompactionSplit declines outright.
	if _, _, ok := CompactionSplit(msgs, 99, true); ok {
		t.Fatal("fixture does not decline the plain split — it proves nothing")
	}

	// A budget the pinned tail CANNOT fit → the window wins and a cut is forced.
	_, cut, ok := splitOrCutToWindow(msgs, 99, true, total/4)
	if !ok {
		t.Fatal("the kept tail alone exceeds the window and the distillation was " +
			"abandoned anyway — keep_last_n is still vetoing the backstop")
	}
	if cut <= 1 {
		t.Errorf("cut = %d: nothing was moved into the summarised span", cut)
	}
	if got := estimateMessageTokens(msgs[cut:]); got > total/4 {
		t.Errorf("kept tail is %d tokens against a %d budget — it was not cut to fit",
			got, total/4)
	}
}

// The override fires ONLY when the tail genuinely does not fit. A short
// conversation that keep_last_n spans is still declined: there is nothing to
// reclaim, and cutting it would discard context for no gain.
func TestSplitOrCutToWindow_AFittingTailIsStillDeclined(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2")}
	if _, _, ok := splitOrCutToWindow(msgs, 99, true, 1_000_000); ok {
		t.Error("a tiny conversation that fits the window was cut anyway — the " +
			"window override must fire only when the tail does not fit")
	}
}

// ⚠️ THE `enabled` DECISION, which is the one judgement call in this phase.
//
// compaction.enabled is OFF by default ("opt-in so existing agents are
// byte-identical"). If the backstop honoured that default, it would be absent
// for most agents and the requirement it exists for — the window must not
// overflow — would not hold.
//
// So the distinction is between a FEATURE and a SAFETY NET:
//
//	nil / unset      → the backstop RUNS. Nobody opted out; "do nothing" is not
//	                   a defensible answer at the point of failure.
//	explicit false   → HONOURED. That is an operator stating they do not want
//	                   this mechanism, and silently overriding a stated choice
//	                   is worse than the overflow — the run reports exhaustion
//	                   instead, so the consequence is visible rather than
//	                   inferred.
//	explicit true    → runs, obviously.
func TestBackstopAvailable_HonoursAnExplicitOptOutButNotTheDefault(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name string
		c    *config.Compaction
		want bool
	}{
		{"no compaction block at all", nil, true},
		{"block present, enabled unset", &config.Compaction{}, true},
		{"explicit enabled: true", &config.Compaction{Enabled: &yes}, true},
		{"explicit enabled: false", &config.Compaction{Enabled: &no}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backstopAvailable(tc.c); got != tc.want {
				t.Errorf("backstopAvailable = %v, want %v — %s", got, tc.want,
					map[bool]string{
						true:  "the default must not disable the safety net",
						false: "an explicit opt-out must be honoured",
					}[tc.want])
			}
		})
	}
}
