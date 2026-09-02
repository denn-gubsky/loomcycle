package loop

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// recapMode builds a resolved context policy in recap mode with the given tuning.
func recapMode(keepLastN int, reasoning string) *config.Context {
	m := config.ContextModeRecap
	c := &config.Context{Mode: &m}
	if keepLastN >= 0 {
		c.KeepLastN = &keepLastN
	}
	if reasoning != "" {
		c.Reasoning = &reasoning
	}
	return c
}

func TestContextRecapMode(t *testing.T) {
	if contextRecapMode(nil) {
		t.Error("nil context is append, not recap")
	}
	if contextRecapMode(&config.Context{}) {
		t.Error("no mode is append, not recap")
	}
	appendM := config.ContextModeAppend
	if contextRecapMode(&config.Context{Mode: &appendM}) {
		t.Error("mode=append is not recap")
	}
	if !contextRecapMode(recapMode(-1, "")) {
		t.Error("mode=recap should be recap")
	}
}

// shouldAutoRecap mirrors shouldAutoCompact: fires at the threshold, is
// self-enabling (mode=recap needs no separate flag), honors the window guard +
// the one-iteration debounce, and never fires outside recap mode.
func TestShouldAutoRecap(t *testing.T) {
	on := recapMode(6, "recap")
	on.AutoRecapAtPct = cptr(80)
	if !shouldAutoRecap(on, 850, 1000, 5, -2) {
		t.Error("85% should fire")
	}
	if shouldAutoRecap(on, 700, 1000, 5, -2) {
		t.Error("70% should not fire")
	}
	if shouldAutoRecap(on, 900, 0, 5, -2) {
		t.Error("unknown window (0) should not fire")
	}
	if shouldAutoRecap(on, 900, 1000, 3, 2) {
		t.Error("one-iteration debounce should suppress")
	}
	// append mode never auto-recaps, even over threshold.
	appendM := config.ContextModeAppend
	if shouldAutoRecap(&config.Context{Mode: &appendM, AutoRecapAtPct: cptr(80)}, 900, 1000, 5, -2) {
		t.Error("append mode must not auto-recap")
	}
	if shouldAutoRecap(nil, 900, 1000, 5, -2) {
		t.Error("nil context must not auto-recap")
	}
	// Default threshold (80%) applies when AutoRecapAtPct is unset.
	if !shouldAutoRecap(recapMode(6, "recap"), 810, 1000, 5, -2) {
		t.Error("81% should fire at the default 80% threshold")
	}
}

// RecapReasoning must use the RUNNING-recap prompt (bounded note), NOT the
// compaction prompt's proportional target, bound its token cap to the char
// budget, flatten the whole span, and offer no tools.
func TestRecapReasoning_UsesBoundedRunningRecapPrompt(t *testing.T) {
	p := &recapProbeProvider{reply: "counter=5; two SET ops applied."}
	_, err := RecapReasoning(context.Background(), p, "m",
		[]providers.Message{userMsg("SET counter 3"), asstMsg("ok"), userMsg("ADD 2")}, 512)
	if err != nil {
		t.Fatalf("RecapReasoning: %v", err)
	}
	sys := p.req.System[0].Text
	if !strings.Contains(sys, "running RECAP") {
		t.Errorf("recap prompt should ask for a running recap, got %q", sys)
	}
	if !strings.Contains(sys, strconv.Itoa(512)) {
		t.Errorf("recap prompt should carry the %d-char budget, got %q", 512, sys)
	}
	if strings.Contains(sys, "% of the original length") {
		t.Errorf("recap must not reuse the compaction proportional target: %q", sys)
	}
	// Token cap is bounded and leaves headroom over the char budget.
	if p.req.MaxTokens == 0 || p.req.MaxTokens*4 <= 512 {
		t.Errorf("MaxTokens=%d, want a bound with headroom over 512 chars", p.req.MaxTokens)
	}
	// Whole span flattened into one user message; no tools.
	if len(p.req.Messages) != 1 || p.req.Messages[0].Role != "user" {
		t.Fatalf("want one flattened user message, got %+v", p.req.Messages)
	}
	body := p.req.Messages[0].Content[0].Text
	for _, w := range []string{"SET counter 3", "ADD 2"} {
		if !strings.Contains(body, w) {
			t.Errorf("flattened span missing %q: %q", w, body)
		}
	}
	if len(p.req.Tools) != 0 {
		t.Errorf("recap call must offer no tools, got %d", len(p.req.Tools))
	}
}

// RecapMessages: the task is a user turn ON ITS OWN (no header to nest), the
// recap is a SEPARATE assistant turn, and the kept tail is appended verbatim. An
// empty recap (drop policy) omits the recap turn entirely.
func TestRecapMessages_Shape(t *testing.T) {
	tail := []providers.Message{userMsg("recent q"), asstMsg("recent a")}
	msgs := RecapMessages("THE TASK", "THE RECAP", tail)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (task + recap + 2 tail): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content[0].Text != "THE TASK" {
		t.Errorf("msg[0] must be the RAW task user turn (no header), got %q", msgs[0].Content[0].Text)
	}
	if msgs[1].Role != "assistant" || !strings.Contains(msgs[1].Content[0].Text, "THE RECAP") {
		t.Errorf("msg[1] must be the recap assistant turn, got %+v", msgs[1])
	}
	if msgs[2].Content[0].Text != "recent q" || msgs[3].Content[0].Text != "recent a" {
		t.Errorf("kept tail not appended verbatim: %+v", msgs[2:])
	}
	// drop policy: empty recap → no recap turn.
	drop := RecapMessages("THE TASK", "", tail)
	if len(drop) != 3 || drop[0].Content[0].Text != "THE TASK" || drop[1].Content[0].Text != "recent q" {
		t.Errorf("empty recap must omit the recap turn: %+v", drop)
	}
}

// maybeRecap folds the middle into a recap assistant turn, keeps the last-N tail
// verbatim, and pins the task as its own user turn.
func TestMaybeRecap_RecapsAndKeepsTail(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	opts := RunOptions{Provider: &steerProvider{}, Model: "x", Context: recapMode(2, "recap")}
	out, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("expected a recap distillation")
	}
	// [user(task), assistant(recap "ok"), q3, a3]
	if len(out) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(out), out)
	}
	if out[0].Role != "user" || out[0].Content[0].Text != "the task" {
		t.Errorf("task must be pinned raw as its own turn: %q", out[0].Content[0].Text)
	}
	if out[1].Role != "assistant" || !strings.Contains(out[1].Content[0].Text, "Progress recap") {
		t.Errorf("recap must be a separate assistant turn: %+v", out[1])
	}
	if out[2].Content[0].Text != "q3" || out[3].Content[0].Text != "a3" {
		t.Errorf("last-2 tail not kept verbatim: %+v", out[2:])
	}
}

// THE load-bearing property: recap-mode keeps the fed prompt FLAT across repeated
// distillations. After two recaps the pinned task turn is byte-identical (no
// nesting), and the SECOND recap call is fed the FIRST recap turn (fold-forward),
// so the recap accumulates without a separate running-state to thread or restore.
func TestMaybeRecap_FlatAndFoldsForward(t *testing.T) {
	p := &recapProbeProvider{reply: "RECAP-CONTENT"}
	opts := RunOptions{Provider: p, Model: "x", Context: recapMode(2, "recap")}

	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	out1, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("first recap did not fire")
	}
	task1 := out1[0].Content[0].Text

	// New turns arrive, then a second recap.
	out1 = append(out1, userMsg("q4"), asstMsg("a4"), userMsg("q5"), asstMsg("a5"))
	out2, did := maybeRecap(context.Background(), opts, out1, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("second recap did not fire")
	}
	task2 := out2[0].Content[0].Text

	// Flat: the pinned task turn is byte-identical, not re-wrapped/nested.
	if task1 != "the task" || task2 != "the task" {
		t.Errorf("pinned task not flat across recaps: %q then %q (want %q both)", task1, task2, "the task")
	}
	// Fold-forward: the second recap call was fed the first recap's turn.
	body := p.req.Messages[0].Content[0].Text
	if !strings.Contains(body, "Progress recap") {
		t.Errorf("second recap was not fed the prior recap turn (no fold-forward): %q", body)
	}
}

// reasoning=drop makes NO provider call and drops the evicted span with no recap
// turn — the cheapest policy.
func TestMaybeRecap_DropMode(t *testing.T) {
	p := &recapProbeProvider{reply: "SHOULD-NOT-BE-CALLED"}
	opts := RunOptions{Provider: p, Model: "x", Context: recapMode(2, "drop")}
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	out, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("drop mode should still distil (drop the evicted span)")
	}
	if p.calls != 0 {
		t.Errorf("drop mode must make NO recap call, got %d", p.calls)
	}
	// [user(task), q3, a3] — no recap turn.
	if len(out) != 3 || out[0].Content[0].Text != "the task" || out[1].Content[0].Text != "q3" {
		t.Errorf("drop mode shape wrong: %+v", out)
	}
}

// reasoning=keep is a no-op: recap mode with keep distils nothing.
func TestMaybeRecap_KeepMode(t *testing.T) {
	p := &recapProbeProvider{reply: "x"}
	opts := RunOptions{Provider: p, Model: "x", Context: recapMode(2, "keep")}
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	out, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if did {
		t.Error("keep mode must not distil")
	}
	if p.calls != 0 {
		t.Errorf("keep mode must make no call, got %d", p.calls)
	}
	if len(out) != len(msgs) {
		t.Errorf("keep mode must leave messages unchanged: %d != %d", len(out), len(msgs))
	}
}

// A failed recap call changes nothing (the run keeps its history) and names the
// failure via emit — the same fail-open invariant as compaction.
func TestMaybeRecap_SurvivesRecapFailure(t *testing.T) {
	opts := RunOptions{Provider: &errProvider{}, Model: "x", Context: recapMode(2, "recap")}
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	var gotErr bool
	out, did := maybeRecap(context.Background(), opts, msgs, 0, func(ev providers.Event) {
		if ev.Type == providers.EventError {
			gotErr = true
		}
	}, "auto")
	if did {
		t.Error("a failed recap must not distil")
	}
	if !gotErr {
		t.Error("a failed recap must emit an error event")
	}
	if len(out) != len(msgs) {
		t.Errorf("a failed recap must leave the history unchanged: %d != %d", len(out), len(msgs))
	}
}

// errProvider always returns an error frame from Call.
type errProvider struct{}

func (p *errProvider) ID() string                                   { return "err-test" }
func (p *errProvider) Probe(context.Context) error                  { return nil }
func (p *errProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *errProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *errProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 1)
	ch <- providers.Event{Type: providers.EventError, Error: "boom"}
	close(ch)
	return ch, nil
}

// Full-loop integration: in recap mode the trigger gate routes to maybeRecap (not
// compaction) when the previous turn's footprint crosses the threshold, emits
// EventContextRecap, and the next request runs on the shrunk history. This is the
// one seam the unit tests above don't cover — that Run() actually wires recapMode
// to the gate. keep_last_n=0 forces a distil on the short interactive history.
func TestRun_RecapMode_AutoRecapsAndShrinks(t *testing.T) {
	q := make(chan steer.Message, 4)
	parked := make(chan struct{}, 8)
	recapped := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &ctxUsageProvider{firstIn: 164000, maxCtx: 200000} // turn 0 → 82% footprint
	m := config.ContextModeRecap
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
			Context:     &config.Context{Mode: &m, KeepLastN: cptr(0), AutoRecapAtPct: cptr(50)},
			OnEvent: func(ev providers.Event) {
				switch ev.Type {
				case providers.EventAwaitingInput:
					parked <- struct{}{}
				case providers.EventContextRecap:
					recapped <- struct{}{}
				case providers.EventContextCompaction:
					t.Errorf("recap mode must NOT emit a compaction marker")
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
	waitOn(parked, "initial end_turn park")
	q <- steer.Message{Text: "continue"} // a real turn → the recap gate fires at its top
	waitOn(recapped, "auto-recap fired")
	waitOn(parked, "re-park after the recapped turn")

	prov.mu.Lock()
	seen := append([]int(nil), prov.seenUsed...)
	prov.mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want >=2 provider calls, got %d (%v)", len(seen), seen)
	}
	if post := seen[len(seen)-1]; post >= prov.firstIn {
		t.Errorf("post-recap Context op=self footprint = %d; want the small recapped size, not the pre-recap %d", post, prov.firstIn)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not terminate after cancel")
	}
}
