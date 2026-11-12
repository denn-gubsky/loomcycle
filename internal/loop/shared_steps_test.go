package loop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// countingPauseGate counts how often the loop asks whether to pause.
type countingPauseGate struct{ asked atomic.Int32 }

func (g *countingPauseGate) PauseRequested() bool       { g.asked.Add(1); return false }
func (g *countingPauseGate) Park(context.Context) error { return nil }

// A cancelled run stops at the top of its iteration, as a stateful run stops
// at the top of its step: no heartbeat, no pause check and no model call
// after the cancel.
func TestRun_CancelledRunStopsBeforeHeartbeatPauseAndRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prov := &endTurnProvider{}
	gate := &countingPauseGate{}
	var beats atomic.Int32
	res, err := Run(ctx, RunOptions{
		Provider:    prov,
		Model:       "x",
		Dispatcher:  tools.NewDispatcher(nil),
		Segments:    userSeg("go"),
		PauseGate:   gate,
		OnHeartbeat: func() { beats.Add(1) },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.StopReason != "cancelled" {
		t.Errorf("stop reason = %q, want cancelled", res.StopReason)
	}
	if n := prov.calls(); n != 0 {
		t.Errorf("provider called %d time(s) after the cancel, want 0", n)
	}
	if n := beats.Load(); n != 0 {
		t.Errorf("heartbeat sent %d time(s) after the cancel, want 0", n)
	}
	if n := gate.asked.Load(); n != 0 {
		t.Errorf("pause asked %d time(s) after the cancel, want 0", n)
	}
}

// A stateful run records one loomcycle.iteration span per step, as the append
// loop does per iteration, ended however the step ends; the step's model call
// and its action's tool call are children of it.
func TestRunStateful_RecordsAnIterationSpanPerStep(t *testing.T) {
	exp, _ := traced(t)
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":1},"action":{"tool":"Echo","input":{}}}`,
		`{"patch":{"count":2},"done":true,"final":"count is 2"}`,
	}}
	echo := &echoTool{reply: "observed"}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Tools: []tools.Tool{echo},
		Dispatcher: tools.NewDispatcher([]tools.Tool{echo}),
		Segments:   statefulTaskSegs(), Context: statefulCtx(nil),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	iters := map[trace.SpanID]int64{}
	for _, sp := range exp.GetSpans() {
		if sp.Name != lcotel.SpanIteration {
			continue
		}
		for _, a := range sp.Attributes {
			if string(a.Key) == lcotel.AttrIteration {
				iters[sp.SpanContext.SpanID()] = a.Value.AsInt64()
			}
		}
	}
	if len(iters) != 2 {
		t.Fatalf("ended iteration spans = %d, want 2 (one per step)", len(iters))
	}
	for _, name := range []string{lcotel.SpanModelCall, lcotel.SpanToolCall} {
		var n int
		for _, sp := range exp.GetSpans() {
			if sp.Name != name {
				continue
			}
			n++
			if _, ok := iters[sp.Parent.SpanID()]; !ok {
				t.Errorf("%s span is not a child of an iteration span", name)
			}
		}
		if n == 0 {
			t.Errorf("no %s span recorded", name)
		}
	}
}
