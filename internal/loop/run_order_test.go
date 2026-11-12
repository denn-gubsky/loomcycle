package loop

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runLoop's steps run in the order its driver lists: the image gate before the
// run is reported started, the hooks after it, the modes and the heartbeat
// above the stateful branch, and the append loop's own state below it. The
// events the run emits are interleaved with the step names, so the pin
// follows the work and not only the hook calls.
func TestRunLoop_StepsRunInTheirOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	note := func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() }
	onEvent := func(ev providers.Event) {
		switch ev.Type {
		case providers.EventStarted, providers.EventText, providers.EventDone:
			note("event:" + string(ev.Type))
		}
	}
	setup := []string{"defaults", "observe", "iteration_cap", "run_meta", "effort", "conversation", "images",
		"event:" + string(providers.EventStarted), "start_hooks", "modes", "heartbeat", "holders"}

	t.Run("append", func(t *testing.T) {
		got = nil
		prov := &fakeProvider{responses: [][]providers.Event{{
			{Type: providers.EventText, Text: "done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 1}},
		}}}
		opts := RunOptions{Provider: prov, Model: "fake-model", Segments: statefulTaskSegs(), OnEvent: onEvent}
		opts.runStepHook = note
		if _, err := Run(context.Background(), opts); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := append(append([]string{}, setup...), "background", "run_state", "resume_parked", "iterations",
			"event:"+string(providers.EventText), "finish", "event:"+string(providers.EventDone))
		mu.Lock()
		defer mu.Unlock()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("step order:\n got  %v\n want %v", got, want)
		}
	})

	// A stateful run leaves at the branch: none of the append loop's steps
	// run, and its first step follows the holders.
	t.Run("stateful", func(t *testing.T) {
		got = nil
		prov := &statefulScriptProvider{scripts: []string{`{"patch":{"count":1},"done":true,"final":"count is 1"}`}}
		echo := &echoTool{reply: "observed"}
		opts := RunOptions{Provider: prov, Model: "x", Tools: []tools.Tool{echo},
			Dispatcher: tools.NewDispatcher([]tools.Tool{echo}), Segments: statefulTaskSegs(),
			Context: statefulCtx(nil), OnEvent: onEvent}
		opts.runStepHook = note
		opts.statefulStepHook = func(s string) {
			if s == "gates" {
				note("stateful:" + s)
			}
		}
		if _, err := Run(context.Background(), opts); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := append(append([]string{}, setup...), "stateful:gates",
			"event:"+string(providers.EventText), "event:"+string(providers.EventDone))
		mu.Lock()
		defer mu.Unlock()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("step order:\n got  %v\n want %v", got, want)
		}
	})
}
