package loop

import (
	"context"
	"strings"
	"sync"
	"testing"

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
