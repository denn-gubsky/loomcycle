package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC CT P2: context.harvest_to_memory extends the L0-only compaction banking to
// the recap and stateful distillation modes, so the consolidator harvests their
// evicted spans' durable facts too. These tests pin that the recap and stateful
// paths bank the evicted span when opted in, do NOT when off, and never fail the
// run when banking errors.

func TestMaybeRecap_BanksToMemoryWhenOptedIn(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	var banked [][]providers.Message
	opts := RunOptions{
		Provider: &steerProvider{},
		Model:    "x",
		// drop mode: pure eviction, no recap call — the harsh case for harvest.
		Context: &config.Context{Mode: cptr(config.ContextModeRecap), KeepLastN: cptr(2), Reasoning: cptr("drop"), HarvestToMemory: cptr(true)},
		BankCompactedSpan: func(_ context.Context, dropped []providers.Message) (string, error) {
			banked = append(banked, dropped)
			return "mp_1", nil
		},
	}
	_, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("expected recap distillation to happen")
	}
	if len(banked) != 1 {
		t.Fatalf("banked %d spans, want exactly 1 (the evicted middle)", len(banked))
	}
	// The evicted span is everything between the pinned first turn and the kept
	// last-2 tail: a1, q2, a2.
	if len(banked[0]) != 3 {
		t.Fatalf("banked span = %d msgs, want 3 (a1, q2, a2)", len(banked[0]))
	}
}

func TestMaybeRecap_NoBankWhenHarvestOff(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	banked := 0
	opts := RunOptions{
		Provider: &steerProvider{},
		Model:    "x",
		// harvest_to_memory OFF. Even with a callback installed (e.g. because the
		// agent also set compaction.memory_flush, which is compaction-semantics), a
		// recap distillation must NOT bank — only harvest_to_memory enables recap
		// banking.
		Context: &config.Context{Mode: cptr(config.ContextModeRecap), KeepLastN: cptr(2), Reasoning: cptr("drop")},
		BankCompactedSpan: func(_ context.Context, _ []providers.Message) (string, error) {
			banked++
			return "x", nil
		},
	}
	if _, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto"); !did {
		t.Fatal("expected recap distillation")
	}
	if banked != 0 {
		t.Fatalf("recap banked %d times with harvest_to_memory off — memory_flush must not trigger recap banking", banked)
	}
}

func TestHarvestToMemory_BankingErrorIsNonFatal(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	errs := 0
	opts := RunOptions{
		Provider: &steerProvider{},
		Model:    "x",
		Context:  &config.Context{Mode: cptr(config.ContextModeRecap), KeepLastN: cptr(2), Reasoning: cptr("drop"), HarvestToMemory: cptr(true)},
		BankCompactedSpan: func(_ context.Context, _ []providers.Message) (string, error) {
			return "", errors.New("agent has no user in memory_scopes")
		},
	}
	_, did := maybeRecap(context.Background(), opts, msgs, 0, func(ev providers.Event) {
		if ev.Type == providers.EventError {
			errs++
		}
	}, "auto")
	if !did {
		t.Fatal("recap must complete despite a banking failure — banking is secondary to keeping the run alive")
	}
	if errs == 0 {
		t.Fatal("a banking misconfiguration should surface as a non-fatal EventError, not be silently dropped")
	}
}

func TestRun_Stateful_HarvestsToMemoryWhenOptedIn(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"reasoning":"begin","patch":{"count":0},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":2},"done":true,"final":"count is 2"}`,
	}}
	echo := &echoTool{reply: "observed"}
	sc := statefulCtx(nil)
	sc.HarvestToMemory = cptr(true)
	banked := 0
	opts := RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    sc,
		OnEvent:    func(providers.Event) {},
		BankCompactedSpan: func(_ context.Context, _ []providers.Message) (string, error) {
			banked++
			return "mp", nil
		},
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("stateful run: %v", err)
	}
	// A stateful run harvests each step's (reasoning + observation) span; the
	// scripted run has multiple steps, so several banks fire.
	if banked < 2 {
		t.Fatalf("stateful harvest banked %d times, want >= 2 (one per step)", banked)
	}
}
