package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// stubChannelIO is a two-message inbox and a recording sink — enough to drive a
// starter state end-to-end through op=run without a channel store.
type stubChannelIO struct {
	mu        sync.Mutex
	published []json.RawMessage
}

func (s *stubChannelIO) Read(context.Context, string, int, int, int) ([]teamrun.ChannelMessage, string, error) {
	return []teamrun.ChannelMessage{
		{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)},
		{ID: "m2", Payload: json.RawMessage(`{"pr":2}`)},
	}, "cur", nil
}

func (s *stubChannelIO) Ack(context.Context, string, string) error { return nil }

func (s *stubChannelIO) Publish(_ context.Context, _ string, payload json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = append(s.published, payload)
	return nil
}

func (s *stubChannelIO) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.published)
}

// breakFixture wires a runnable starter team: a spawner, a channel executor and
// the team itself.
func breakFixture(t *testing.T) (*TeamDef, context.Context, *stubChannelIO, *int, func()) {
	t.Helper()
	tool, ctx, done := teamDefFixture(t)
	io := &stubChannelIO{}
	spawned := 0
	var mu sync.Mutex
	tool.Spawn = func(_ context.Context, _ string, _ teamrun.Prompt, _ string) (string, error) {
		mu.Lock()
		spawned++
		mu.Unlock()
		return "reviewed", nil
	}
	tool.Channels = func(context.Context, teamgraph.Definition) teamrun.ChannelIO { return io }
	actx := authoringCtx([]string{"verdicts"}, []string{"pr-events"})
	createTeam(t, tool, actx, "triage",
		strings.TrimSuffix(starterGraph, "}")+`,"channels":{"publish":["verdicts"],"subscribe":["pr-events"]}}`)
	_ = ctx
	return tool, actx, io, &spawned, done
}

// TestTeamDefTool_Run_BreakpointAsksAtBothPhasesAndReleases: the end-to-end
// debug path — a breakpoint on a starter pauses twice (before dispatch, after
// collection), the human's answer releases each, and the run reports what
// happened.
func TestTeamDefTool_Run_BreakpointAsksAtBothPhasesAndReleases(t *testing.T) {
	tool, ctx, io, spawned, done := breakFixture(t)
	defer done()

	var questions []string
	var spawnedAtAsk, publishedAtAsk []int
	tool.AskHuman = func(_ context.Context, q string) (string, error) {
		questions = append(questions, q)
		spawnedAtAsk = append(spawnedAtAsk, *spawned)
		publishedAtAsk = append(publishedAtAsk, io.count())
		return "continue", nil
	}

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"run","name":"triage","input":"x","breakpoints":["wave"]}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["status"] != "completed" {
		t.Fatalf("status = %v, want completed", out["status"])
	}
	if len(questions) != 2 {
		t.Fatalf("asked %d times, want 2 (before_dispatch + after_collection):\n%s",
			len(questions), strings.Join(questions, "\n---\n"))
	}
	// Pause 1 happens with nothing spawned; pause 2 with everything spawned and
	// nothing published. Those two numbers ARE the feature.
	if spawnedAtAsk[0] != 0 {
		t.Errorf("%d runs already spawned at the before_dispatch pause, want 0", spawnedAtAsk[0])
	}
	if spawnedAtAsk[1] != 2 || publishedAtAsk[1] != 0 {
		t.Errorf("at the after_collection pause: spawned=%d published=%d, want 2 and 0",
			spawnedAtAsk[1], publishedAtAsk[1])
	}
	if !strings.Contains(questions[0], "BEFORE dispatching") || !strings.Contains(questions[0], `state "wave"`) {
		t.Errorf("before_dispatch question does not say what it is:\n%s", questions[0])
	}
	if !strings.Contains(questions[0], `{"pr":1}`) {
		t.Errorf("before_dispatch question omits the composed prompt — the one thing no channel inspection shows:\n%s", questions[0])
	}
	if !strings.Contains(questions[1], "AFTER collecting") || !strings.Contains(questions[1], "reviewed") {
		t.Errorf("after_collection question omits the results:\n%s", questions[1])
	}
	if io.count() != 2 {
		t.Errorf("published %d sink messages, want 2", io.count())
	}
	if out["breakpoints_hit"].(float64) != 2 {
		t.Errorf("breakpoints_hit = %v, want 2", out["breakpoints_hit"])
	}
	if out["break_decision"] != "continue" {
		t.Errorf("break_decision = %v, want continue", out["break_decision"])
	}
}

// TestTeamDefTool_Run_BreakpointAbortWithholdsTheSink: an unanswerable or
// refused ask aborts, and nothing reaches the sink.
func TestTeamDefTool_Run_BreakpointAbortWithholdsTheSink(t *testing.T) {
	tool, ctx, io, spawned, done := breakFixture(t)
	defer done()
	tool.AskHuman = func(context.Context, string) (string, error) { return "abort", nil }

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"run","name":"triage","input":"x","breakpoints":["wave:after_collection"]}`))
	if !res.IsError {
		t.Fatalf("an aborted breakpoint must fail the run, got: %s", res.Text)
	}
	if *spawned != 2 {
		t.Errorf("spawned %d, want 2 (after_collection aborts AFTER the runs)", *spawned)
	}
	if io.count() != 0 {
		t.Errorf("published %d sink messages after an abort, want 0 — withholding the result is the point", io.count())
	}
}

// TestTeamDefTool_Run_BreakpointRefusals pins every way a breakpoint argument
// can be wrong. Each is refused rather than armed as nothing — an operator who
// typed a breakpoint and got a walk that never paused would reasonably conclude
// the feature is broken.
func TestTeamDefTool_Run_BreakpointRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, bps, want string
	}{
		{"unknown state", `["nope"]`, "no state"},
		{"not a starter", `["done"]`, "only a starter"},
		{"unknown phase", `["wave:whenever"]`, "breakpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ctx, io, spawned, done := breakFixture(t)
			defer done()
			tool.AskHuman = func(context.Context, string) (string, error) { return "continue", nil }

			res, _ := tool.Execute(ctx, json.RawMessage(
				`{"op":"run","name":"triage","input":"x","breakpoints":`+tc.bps+`}`))
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("want a refusal containing %q, got IsError=%v: %s", tc.want, res.IsError, res.Text)
			}
			if *spawned != 0 || io.count() != 0 {
				t.Errorf("a refused run must not walk: spawned=%d published=%d", *spawned, io.count())
			}
		})
	}
}

// TestTeamDefTool_Run_BreakpointsRequireTheInterruptionMachinery: with no way
// to ask, the run is REFUSED rather than silently running at full speed.
// interrupt_on_cap may degrade to aborting because that fallback is still safe;
// a breakpoint's whole job is to hold work back.
func TestTeamDefTool_Run_BreakpointsRequireTheInterruptionMachinery(t *testing.T) {
	tool, ctx, io, spawned, done := breakFixture(t)
	defer done()
	tool.AskHuman = nil

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"run","name":"triage","input":"x","breakpoints":["wave"]}`))
	if !res.IsError || !strings.Contains(res.Text, "Interruption") {
		t.Fatalf("want a refusal naming the missing machinery, got IsError=%v: %s", res.IsError, res.Text)
	}
	if *spawned != 0 || io.count() != 0 {
		t.Errorf("the wave ran anyway: spawned=%d published=%d", *spawned, io.count())
	}
}

// TestTeamDefTool_Run_NoBreakpointsNeverAsks pins the additive contract: with
// AskHuman wired but `breakpoints` absent, the run never pauses and the response
// carries no debug fields. (Fail-before: arm unconditionally and this breaks.)
func TestTeamDefTool_Run_NoBreakpointsNeverAsks(t *testing.T) {
	tool, ctx, io, _, done := breakFixture(t)
	defer done()
	asked := 0
	tool.AskHuman = func(context.Context, string) (string, error) { asked++; return "continue", nil }

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if asked != 0 {
		t.Errorf("AskHuman called %d times without breakpoints, want 0", asked)
	}
	out := decodeResult(t, res.Text)
	if _, present := out["breakpoints_hit"]; present {
		t.Errorf("breakpoints_hit present on a run that set none: %v", out)
	}
	if io.count() != 2 {
		t.Errorf("published %d, want 2", io.count())
	}
}

func TestParseBreakAnswer(t *testing.T) {
	for _, tc := range []struct {
		in     string
		action teamrun.BreakAction
		n      int
	}{
		{"continue", teamrun.BreakContinue, 0},
		{"  CONTINUE ", teamrun.BreakContinue, 0},
		{"all", teamrun.BreakContinue, 0},
		{"release:3", teamrun.BreakRelease, 3},
		{"release 2", teamrun.BreakRelease, 2},
		{"release", teamrun.BreakRelease, 1},
		{"release:0", teamrun.BreakAbort, 0},
		{"release:-1", teamrun.BreakAbort, 0},
		{"release:many", teamrun.BreakAbort, 0},
		{"abort", teamrun.BreakAbort, 0},
		// Anything unrecognised STOPS. A debugger that defaulted to releasing
		// would be a debugger you cannot trust to hold.
		{"", teamrun.BreakAbort, 0},
		{"sure, go ahead", teamrun.BreakAbort, 0},
	} {
		got := parseBreakAnswer(tc.in)
		if got.Action != tc.action || (tc.action == teamrun.BreakRelease && got.N != tc.n) {
			t.Errorf("parseBreakAnswer(%q) = %+v, want action=%v n=%d", tc.in, got, tc.action, tc.n)
		}
	}
}

// TestFormatBreakpoint_TruncatesAnOversizedPreview: an agent's output can be a
// whole document; the question a human answers must stay readable and say that
// it was cut rather than hand over a prefix they would read as the whole thing.
func TestFormatBreakpoint_TruncatesAnOversizedPreview(t *testing.T) {
	q := formatBreakpoint("triage", teamrun.Breakpoint{
		State: "wave", Phase: teamrun.AfterCollection, Wave: "wav_x",
		WaveSize: 1, Pending: 1,
		Results: []teamrun.BreakpointResult{{Index: 0, Agent: "reviewer", Ok: true,
			Output: strings.Repeat("x", maxBreakPreview*3)}},
	})
	if len(q) > maxBreakPreview*2 {
		t.Errorf("question is %d bytes — an oversized output was not bounded", len(q))
	}
	if !strings.Contains(q, "bytes)") {
		t.Errorf("a truncated preview must say so:\n%s", q)
	}
}
