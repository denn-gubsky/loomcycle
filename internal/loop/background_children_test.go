package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// spawnTool stands in for Agent poll mode: each call files the children its
// input names in the run's background table and returns at once, leaving them
// running until the test ends them.
type spawnTool struct {
	mu   sync.Mutex
	bg   *tools.Background
	ctxs map[string]context.Context
}

func (t *spawnTool) Name() string                 { return "Spawn" }
func (t *spawnTool) Description() string          { return "" }
func (t *spawnTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *spawnTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in struct {
		IDs      []string `json:"ids"`
		Notify   bool     `json:"notify"`
		OnEndCxl bool     `json:"cancel_on_end"`
		FinishAs string   `json:"finish_as"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Result{}, err
	}
	bg := tools.BackgroundOf(ctx)
	if bg == nil {
		return tools.Result{Text: "no table", IsError: true}, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bg = bg
	if t.ctxs == nil {
		t.ctxs = map[string]context.Context{}
	}
	for _, id := range in.IDs {
		cctx, err := bg.Start(ctx, tools.ChildSpec{RunID: id, Agent: "worker", Index: -1, Notify: in.Notify, CancelOnParentEnd: in.OnEndCxl})
		if err != nil {
			return tools.Result{}, err
		}
		bg.SetState(id, tools.ChildRunning)
		t.ctxs[id] = cctx
		if in.FinishAs != "" {
			bg.Finish(id, in.FinishAs, tools.ChildResult{Output: "out-" + id})
		}
	}
	return tools.Result{Text: "started"}, nil
}

func (t *spawnTool) finish(id string) {
	t.mu.Lock()
	bg := t.bg
	t.mu.Unlock()
	bg.Finish(id, tools.ChildCompleted, tools.ChildResult{Output: "out-" + id})
}

func (t *spawnTool) childCtx(id string) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ctxs[id]
}

func spawnCall(id, input string) providers.Event {
	return providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "Spawn", Input: json.RawMessage(input)}}
}

func endTurn(text string) []providers.Event {
	return []providers.Event{{Type: providers.EventText, Text: text}, {Type: providers.EventDone, StopReason: "end_turn", Usage: usage(1, 1)}}
}

func toolTurn(calls ...providers.Event) []providers.Event {
	return append(calls, providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: usage(1, 1)})
}

// childEvents collects a run's events from any goroutine.
type childEvents struct {
	mu  sync.Mutex
	evs []providers.Event
}

func (s *childEvents) add(ev providers.Event) {
	s.mu.Lock()
	s.evs = append(s.evs, ev)
	s.mu.Unlock()
}

func (s *childEvents) of(typ providers.EventType) []providers.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []providers.Event
	for _, ev := range s.evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func runWithSpawn(ctx context.Context, prov *fakeProvider, tool *spawnTool, maxIter int, sink *childEvents) (RunResult, error) {
	fr := &fakeTool{}
	ts := []tools.Tool{tool, fr}
	return Run(ctx, RunOptions{
		Provider:      prov,
		Model:         "fake-model",
		Tools:         ts,
		Dispatcher:    tools.NewDispatcher(ts),
		MaxIterations: maxIter,
		Segments:      []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		OnEvent:       sink.add,
	})
}

func lastUserText(req providers.Request) string {
	m := req.Messages[len(req.Messages)-1]
	if m.Role != "user" || len(m.Content) == 0 || m.Content[0].Type != "text" {
		return ""
	}
	return m.Content[0].Text
}

func countNotes(reqs []providers.Request, sub string) int {
	n := 0
	for _, r := range reqs {
		for _, m := range r.Messages {
			for _, c := range m.Content {
				if c.Type == "text" && strings.Contains(c.Text, sub) {
					n++
				}
			}
		}
	}
	return n
}

// Children that ended since the last call are named in ONE note on the next
// call, after the tool results; a later call does not repeat it, and a child
// started with notify off is not named.
func TestRun_ChildrenNoteRidesTheNextCallOnceForSeveral(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1","r_2"],"notify":true,"finish_as":"completed"}`), spawnCall("s2", `{"ids":["r_3"],"finish_as":"completed"}`)),
		toolTurn(toolCall("t1")),
		endTurn("done"),
	}}
	sink := &childEvents{}
	if _, err := runWithSpawn(context.Background(), prov, &spawnTool{}, 0, sink); err != nil {
		t.Fatal(err)
	}
	note := "Background children finished: r_1 (worker): completed; r_2 (worker): completed. Use Agent poll to read their results."
	if got := lastUserText(prov.calls[1]); got != note {
		t.Fatalf("second call ends with %q, want the note %q", got, note)
	}
	prev := prov.calls[1].Messages[len(prov.calls[1].Messages)-2]
	if prev.Role != "user" || prev.Content[0].Type != "tool_result" {
		t.Errorf("the note does not follow the tool results: %+v", prev)
	}
	// The note stays in the conversation (once); no second note was added.
	if n := countNotes(prov.calls[2:], "Background child"); n != 1 {
		t.Errorf("the third call carries %d notes, want the one already in the history", n)
	}
	if n := countNotes(prov.calls, "r_3"); n != 0 {
		t.Errorf("a notify-off child was named %d times", n)
	}
	if n := len(sink.of(providers.EventChildrenNote)); n != 1 {
		t.Errorf("%d children_note events, want 1", n)
	}
}

// A run that ends its turn with background children outstanding does not
// complete: it parks — no model call — until EVERY one has ended, wakes once
// with a note naming how each ended, and completes after its next turn.
func TestRun_AwaitingChildrenParksUntilEveryChildEnds(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1","r_2"],"notify":true}`)),
		endTurn("started them"),
		endTurn("read them"),
	}}
	tool := &spawnTool{}
	sink := &childEvents{}
	type out struct {
		res RunResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := runWithSpawn(context.Background(), prov, tool, 0, sink)
		done <- out{res, err}
	}()
	waitFor(t, func() bool { return len(sink.of(providers.EventAwaitingChildren)) == 1 })
	parked := sink.of(providers.EventAwaitingChildren)[0].AwaitingChildren
	if parked == nil || strings.Join(parked.ChildRunIDs, ",") != "r_1,r_2" {
		t.Fatalf("awaiting_children = %+v, want r_1 and r_2", parked)
	}
	tool.finish("r_1")
	time.Sleep(50 * time.Millisecond)
	select {
	case o := <-done:
		t.Fatalf("the run ended with a child still running: %+v %v", o.res, o.err)
	default:
	}
	prov.mu.Lock()
	calls := len(prov.calls)
	prov.mu.Unlock()
	if calls != 2 {
		t.Fatalf("%d model calls while parked, want 2 (no call while waiting)", calls)
	}
	tool.finish("r_2")
	o := <-done
	if o.err != nil || o.res.StopReason != "end_turn" || o.res.FinalText != "read them" {
		t.Fatalf("run = %+v, %v; want it to complete on the turn after the wake", o.res, o.err)
	}
	wake := "Every background child you were waiting for has ended: r_1 (worker): completed; r_2 (worker): completed. Use Agent poll to read their results."
	if got := lastUserText(prov.calls[2]); got != wake {
		t.Errorf("wake call ends with %q, want %q", got, wake)
	}
	if n := len(sink.of(providers.EventAwaitingChildren)); n != 1 {
		t.Errorf("parked %d times, want once", n)
	}
	// The completion notes of r_1 and r_2 were folded into the wake note.
	if n := len(sink.of(providers.EventChildrenNote)); n != 1 {
		t.Errorf("%d notes, want only the wake note", n)
	}
}

// Children started with on_parent_end: cancel do not hold the run: it
// completes at once and they are cancelled.
func TestRun_OnParentEndCancelCompletesAtOnce(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1"],"cancel_on_end":true}`)),
		endTurn("done"),
	}}
	tool := &spawnTool{}
	sink := &childEvents{}
	res, err := runWithSpawn(context.Background(), prov, tool, 0, sink)
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("run = %+v, %v; want end_turn", res, err)
	}
	if n := len(sink.of(providers.EventAwaitingChildren)); n != 0 {
		t.Error("the run waited for a child started to be cancelled at its turn end")
	}
	if c := tool.childCtx("r_1"); c.Err() == nil || !strings.Contains(context.Cause(c).Error(), "on_parent_end") {
		t.Errorf("child cause = %v, want the turn-end cancel", context.Cause(c))
	}
}

// Cancelling a parent that waits for its children ends it cancelled and
// cancels them.
func TestRun_ParkedParentCancelCancelsItsChildren(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1"]}`)),
		endTurn("started"),
	}}
	tool := &spawnTool{}
	sink := &childEvents{}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runWithSpawn(ctx, prov, tool, 0, sink)
		done <- err
	}()
	waitFor(t, func() bool { return len(sink.of(providers.EventAwaitingChildren)) == 1 })
	why := errors.New("operator cancelled the parent")
	cancel(why)
	if err := <-done; err == nil {
		t.Fatal("a cancelled parked run returned no error")
	}
	if c := tool.childCtx("r_1"); !errors.Is(context.Cause(c), why) {
		t.Errorf("child cause = %v, want the parent's cancel", context.Cause(c))
	}
}

// A run that ends its LAST iteration with children outstanding cannot take
// the turn that would read them: they are cancelled, said so, and the run
// finishes rather than waiting for results nothing will read.
func TestRun_NoIterationLeftCancelsOutstandingChildren(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1"]}`)),
		endTurn("out of turns"),
	}}
	tool := &spawnTool{}
	sink := &childEvents{}
	res, err := runWithSpawn(context.Background(), prov, tool, 2, sink)
	if err != nil || res.FinalText != "out of turns" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if n := len(sink.of(providers.EventAwaitingChildren)); n != 0 {
		t.Error("the run waited with no iteration left")
	}
	if c := tool.childCtx("r_1"); c.Err() == nil || !strings.Contains(context.Cause(c).Error(), "no iteration left") {
		t.Errorf("child cause = %v, want cancelled for want of an iteration", context.Cause(c))
	}
	errs := sink.of(providers.EventError)
	if len(errs) != 1 || !strings.Contains(errs[0].Error, "r_1 (worker)") {
		t.Errorf("error events = %+v, want one naming r_1", errs)
	}
}

// Whatever a run leaves running when it ends is cancelled.
func TestRun_EndingRunCancelsWhatItLeftRunning(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1"]}`)),
		// no further script: the next call fails the run
	}}
	tool := &spawnTool{}
	if _, err := runWithSpawn(context.Background(), prov, tool, 0, &childEvents{}); err == nil {
		t.Fatal("the run did not fail")
	}
	if c := tool.childCtx("r_1"); c.Err() == nil || !strings.Contains(context.Cause(c).Error(), "parent run ended") {
		t.Errorf("child cause = %v, want cancelled as the run ended", context.Cause(c))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
	}
}

// A run waiting for its children is at a clean boundary: a runtime pause
// records it paused while it waits, and it goes on waiting after the resume.
func TestRun_AwaitingChildrenTakesPartInAPause(t *testing.T) {
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1"]}`)),
		endTurn("started"),
		endTurn("read"),
	}}
	tool := &spawnTool{}
	sink := &childEvents{}
	gate := newIdleGate()
	fr := &fakeTool{}
	ts := []tools.Tool{tool, fr}
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), RunOptions{
			Provider: prov, Model: "fake-model", Tools: ts, Dispatcher: tools.NewDispatcher(ts),
			PauseGate: gate,
			Segments:  []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
			OnEvent:   sink.add,
		})
		done <- err
	}()
	waitFor(t, func() bool { return len(sink.of(providers.EventAwaitingChildren)) == 1 })
	gate.declare()
	waitCount(t, "paused records", gate.paused.Load, 1)
	gate.lift()
	waitCount(t, "paused records", gate.paused.Load, 0)
	tool.finish("r_1")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A run waiting for its children keeps its heartbeat going, so the stale-run
// sweeper never takes a parked parent for a dead one.
func TestRun_AwaitingChildrenKeepsHeartbeating(t *testing.T) {
	orig := parkHeartbeatInterval
	parkHeartbeatInterval = 10 * time.Millisecond
	defer func() { parkHeartbeatInterval = orig }()
	prov := &fakeProvider{responses: [][]providers.Event{
		toolTurn(spawnCall("s1", `{"ids":["r_1"]}`)),
		endTurn("started"),
		endTurn("read"),
	}}
	tool := &spawnTool{}
	sink := &childEvents{}
	var beats atomic.Int32
	fr := &fakeTool{}
	ts := []tools.Tool{tool, fr}
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), RunOptions{
			Provider: prov, Model: "fake-model", Tools: ts, Dispatcher: tools.NewDispatcher(ts),
			OnHeartbeat: func() { beats.Add(1) },
			Segments:    []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
			OnEvent:     sink.add,
		})
		done <- err
	}()
	waitFor(t, func() bool { return len(sink.of(providers.EventAwaitingChildren)) == 1 })
	at := beats.Load()
	waitFor(t, func() bool { return beats.Load() >= at+3 })
	tool.finish("r_1")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
