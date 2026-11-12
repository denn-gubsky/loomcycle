package loop

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The steps of a stateful run happen in the order statefulLoop.step lists:
// the gates before the model call, Σ merged and weighed before the step is
// recorded, the step recorded before the turn can end, and the action only
// when it does not. The events each step emits are interleaved with the step
// names, so the pin follows the work and not only the hook calls.
func TestRunStateful_StepsRunInTheirOrder(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":2},"done":true,"final":"count is 2"}`,
	}}
	echo := &echoTool{reply: "observed"}
	var mu sync.Mutex
	var got []string
	note := func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() }
	opts := RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
		OnEvent: func(ev providers.Event) {
			switch ev.Type {
			case providers.EventPromptSnapshot, providers.EventUsage, providers.EventContextState,
				providers.EventText, providers.EventToolCall, providers.EventDone:
				note("event:" + string(ev.Type))
			}
		},
	}
	opts.statefulStepHook = note
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", res.StopReason)
	}

	want := []string{
		// iteration 0: an action step
		"gates", "steer", "queue_move", "call",
		"event:" + string(providers.EventPromptSnapshot), "event:" + string(providers.EventUsage),
		"merge", "evict", "record", "event:" + string(providers.EventContextState),
		"end_turn", "action", "event:" + string(providers.EventToolCall),
		// iteration 1: the turn ends
		"gates", "steer", "queue_move", "call", "event:" + string(providers.EventUsage),
		"merge", "evict", "record", "event:" + string(providers.EventContextState),
		"end_turn", "event:" + string(providers.EventText), "event:" + string(providers.EventDone),
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("step order:\n got  %v\n want %v", got, want)
	}
}

// Merge and evict emit no event of their own, so the order pin above cannot
// see their CALLS swapped under unchanged labels. This pins their effect: the
// step whose patch fills the window evicts that very key in the same step,
// which holds only when the patch is merged before eviction weighs Σ.
func TestRunStateful_MergeRunsBeforeEvict(t *testing.T) {
	m := config.ContextModeStateful
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"spine": map[string]any{"type": "string", retentionKeyword: RetentionCore},
		"note":  map[string]any{"type": "string", retentionKeyword: RetentionScratch},
	}}
	var mu sync.Mutex
	var states []providers.ContextStateEventInfo
	if _, err := Run(context.Background(), RunOptions{
		Provider: &oneBigWriteProvider{}, Model: "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		Context:    &config.Context{Mode: &m, StateSchema: schema},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextState && ev.ContextState != nil {
				mu.Lock()
				states = append(states, *ev.ContextState)
				mu.Unlock()
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 2 {
		t.Fatalf("got %d context_state events, want 2", len(states))
	}
	first := states[0]
	if len(first.Evicted) != 1 || first.Evicted[0] != "note" {
		t.Errorf("step 0 evicted %v, want [note]: eviction did not weigh the patch merged in the same step", first.Evicted)
	}
	if _, ok := first.State["note"]; ok {
		t.Error("step 0 state still holds the evicted note")
	}
	if _, ok := first.State["spine"]; !ok {
		t.Error("step 0 state lost its core key: the patch was not merged before the step was recorded")
	}
}
