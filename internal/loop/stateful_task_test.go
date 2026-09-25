package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The user's request reaches EVERY step, not only the first. It used to arrive
// only as the first observation, and the next tool result replaced it: measured
// live, a model that had not copied it into the state itself lost its task
// after one tool call. The first step shows it once (as the observation); a
// later step shows it beside the state.
func TestRun_Stateful_EveryStepCarriesTheTask(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"step":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"step":2},"action":{"tool":"Echo","input":{}}}`,
		`{"done":true,"final":"counted"}`,
	}}
	echo := &echoTool{reply: "observed"}
	if _, err := Run(context.Background(), RunOptions{
		Provider:   prov,
		Model:      "x",
		Tools:      []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(),
		Context:    statefulCtx(nil),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.requests) < 3 {
		t.Fatalf("got %d model calls, want 3", len(prov.requests))
	}
	fed := func(i int) string {
		var b strings.Builder
		for _, m := range prov.requests[i] {
			for _, c := range m.Content {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	if first := fed(0); strings.Count(first, "count to 2") != 1 || strings.Contains(first, "Your task") {
		t.Errorf("step 1 should show the task once, as the observation:\n%s", first)
	}
	for i := 1; i < 3; i++ {
		got := fed(i)
		if !strings.Contains(got, "Your task (keep working on it until it is done):\nTask: count to 2") {
			t.Errorf("step %d lost the task:\n%s", i+1, got)
		}
		if !strings.Contains(got, "Latest observation:\nobserved") {
			t.Errorf("step %d lost its observation:\n%s", i+1, got)
		}
	}
}
