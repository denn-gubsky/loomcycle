package http

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A walk that pauses for a person is a run waiting on an Interruption, and is
// reported as one.
//
// A model's ask is seen waiting because the loop records its tool_call: the
// run-state stream announces the hold from the live event (holdAfter), and
// every run read derives awaited_state from the persisted row (awaited.ForRun).
// A walk asks without a model — a breakpoint, or an iteration cap escalated to
// a person, calls the Interruption tool directly — so neither ever saw it, and
// a paused walk read as a walk that was simply slow.
//
// The walk's ask is therefore recorded the way a model's is: a tool_call of
// Interruption `ask` on the walk's transcript when it starts and its tool_result
// when it ends, with a stream frame at each. Reads derive the wait from those
// rows with no special case, on any replica, and the stream says what the reads
// say.

// walkAskInput is the recorded call's input: the op and kind the awaited
// derivation reads. The question stays on the interrupt row — a breakpoint's
// carries every pending prompt, and the transcript needs none of it to say what
// the walk is waiting on.
var walkAskInput = json.RawMessage(`{"op":"ask","kind":"question"}`)

// walkAwareAsk wraps the TeamDef tool's human ask: an ask made under a walk's
// run is recorded and announced as that run's wait for as long as it blocks.
// Any other caller is passed straight through.
func (s *Server) walkAwareAsk(ask func(context.Context, string) (string, error)) func(context.Context, string) (string, error) {
	return func(ctx context.Context, question string) (answer string, err error) {
		// The ask is filed under the ctx's run id, so that is the run waiting.
		w, ok := s.walks.get(tools.RunID(ctx))
		if !ok {
			return ask(ctx, question)
		}
		end := s.holdWalk(ctx, w.meta)
		// Deferred: every way out of the ask — answered, timed out, declined,
		// cancelled with the walk — ends the wait.
		defer func() { end(answer, err) }()
		return ask(ctx, question)
	}
}

// holdWalk records and announces the walk's wait on an Interruption, and
// returns what ends it.
func (s *Server) holdWalk(ctx context.Context, meta runStateMeta) (end func(answer string, err error)) {
	// Written on a ctx that survives the walk's cancel: a cancelled walk's ask
	// returns because the ctx ended, and its end must still be recorded, or the
	// row would say the run was waiting on it after all.
	bg := context.WithoutCancel(ctx)
	// Unique per ask: the derivation pairs a result with its call by id.
	id := "walk_ask_" + strings.TrimPrefix(store.MintInterruptID(time.Now()), "intr_")
	s.recordWalkEvent(bg, meta.RunID, providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: id, Name: "Interruption", Input: walkAskInput}})
	state, on := awaited.FromToolUse("Interruption", walkAskInput)
	s.publishHold(meta, runHold{state: state, on: on})
	return func(answer string, err error) {
		res := providers.Event{Type: providers.EventToolResult,
			ToolUse: &providers.ToolUse{ID: id, Name: "Interruption"}, Text: answer}
		if err != nil {
			res.Text, res.IsError = err.Error(), true
		}
		res.Text = s.redactor.String(res.Text)
		s.recordWalkEvent(bg, meta.RunID, res)
		s.publishHold(meta, runHold{})
	}
}

// recordWalkEvent appends one event to the walk's transcript. A failed write
// is logged, not returned: the pause itself does not depend on it.
func (s *Server) recordWalkEvent(ctx context.Context, runID string, ev providers.Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := s.store.AppendEvent(ctx, runID, string(ev.Type), payload); err != nil {
		log.Printf("teamdef: record %s on walk run %s: %v", ev.Type, runID, err)
	}
}
