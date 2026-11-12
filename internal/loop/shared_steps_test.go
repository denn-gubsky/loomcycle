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

// spanAtParkGate requests one pause and records the span its Park ran under.
type spanAtParkGate struct {
	asked  atomic.Int32
	parked trace.SpanContext
}

func (g *spanAtParkGate) PauseRequested() bool { return g.asked.Add(1) == 1 }
func (g *spanAtParkGate) Park(ctx context.Context) error {
	g.parked = trace.SpanFromContext(ctx).SpanContext()
	return nil
}

// A stateful run parks for a runtime pause on its step's own ctx, inside the
// step's iteration span, as an append run parks on its iteration's ctx.
func TestRunStateful_ParksInsideTheIterationSpan(t *testing.T) {
	exp, _ := traced(t)
	prov := &statefulScriptProvider{scripts: []string{`{"patch":{"count":1},"done":true,"final":"count is 1"}`}}
	gate := &spanAtParkGate{}
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x", Dispatcher: tools.NewDispatcher(nil),
		Segments: statefulTaskSegs(), Context: statefulCtx(nil), PauseGate: gate,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !gate.parked.IsValid() {
		t.Fatalf("the park ran under no span; want the step's iteration span")
	}
	for _, sp := range exp.GetSpans() {
		if sp.Name == lcotel.SpanIteration && sp.SpanContext.SpanID() == gate.parked.SpanID() {
			return
		}
	}
	t.Errorf("the park ran under span %s, which is not an iteration span", gate.parked.SpanID())
}
