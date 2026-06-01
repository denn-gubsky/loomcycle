package codejs

import (
	"encoding/json"

	"github.com/dop251/goja"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// This file is the RFC J Appendix B replay engine — the stateless successor to
// the Mechanism-1 parked-goroutine continuation. There is no continuation, no
// registry, no parked goroutine: each Provider.Call builds a fresh runtime,
// REPLAYS the run's recorded tool results (already in the transcript), and
// stops at the first un-recorded call (the "frontier"), which the loop then
// dispatches. The transcript IS the durable memoization log, so a run is
// resumable across restart/replica for free.

// toolRecord is one already-dispatched tool call recovered from the
// transcript: the call the JS made (name+input) and the result the loop
// returned (text+isError). Replayed in order to fast-forward run() to its
// frontier.
type toolRecord struct {
	name    string
	input   json.RawMessage
	text    string
	isError bool
}

// frontierStop is the sentinel Interrupt value for "run() reached its next
// un-recorded tool call." The provider emits an EventToolCall for it.
type frontierStop struct {
	idx   int
	name  string
	input json.RawMessage
}

// replayDivergence is the sentinel for "the replayed call sequence no longer
// matches the recorded one" — a control-flow change across re-execution that
// must never feed a mismatched result into the JS.
type replayDivergence struct {
	idx      int
	expected string
	got      string
}

// replayState drives one Call's execution. It runs entirely on the runtime
// goroutine (the bound tool funcs call emit synchronously); nothing crosses
// goroutines, so there is no goja-Value-escape concern.
type replayState struct {
	rt       *goja.Runtime
	recorded []toolRecord
	k        int // index of the next tool call run() makes this execution

	frontier *frontierStop     // set when run() reaches an un-recorded call
	diverged *replayDivergence // set when the replayed sequence mismatches
}

// emit is the toolEmitter the bindings call for every JS tool invocation.
// It either fast-forwards a recorded result, or stops at the frontier — both
// without dispatching anything (the provider/loop dispatches the frontier).
func (s *replayState) emit(name string, input json.RawMessage) (string, bool, error) {
	idx := s.k
	s.k++
	if idx < len(s.recorded) {
		rec := s.recorded[idx]
		// Divergence guard: ambient determinism (sandbox.go) keeps the
		// replayed call sequence identical to the recorded one. A name
		// mismatch means control flow changed (an unhooked non-determinism
		// source, or allowed_tools changed mid-run) — abort rather than feed
		// the wrong result into the JS. (Input-level divergence detection is a
		// possible hardening; name match is the high-value check and avoids
		// JSON-canonicalization false positives.)
		if rec.name != name {
			s.diverged = &replayDivergence{idx: idx, expected: rec.name, got: name}
			s.rt.Interrupt(s.diverged)
			return "", false, nil
		}
		return rec.text, rec.isError, nil // fast-forward: replay recorded result
	}
	// Frontier: the first un-recorded call. Abort this execution; the provider
	// emits the tool_use for the loop to dispatch. Copy the input — the
	// runtime that produced it is discarded after this Call.
	s.frontier = &frontierStop{idx: idx, name: name, input: append(json.RawMessage(nil), input...)}
	s.rt.Interrupt(s.frontier)
	return "", false, nil
}

// extractRecorded recovers, in order, the tool calls already dispatched in
// this run from the transcript: each assistant tool_use block paired with its
// user tool_result block by tool_use id. A tool_use without a matching result
// (shouldn't happen on a well-formed resume) stops the scan — we never replay
// past a result we don't have.
func extractRecorded(req providers.Request) []toolRecord {
	type res struct {
		text    string
		isError bool
	}
	results := make(map[string]res)
	type use struct {
		id    string
		name  string
		input json.RawMessage
	}
	var uses []use
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				uses = append(uses, use{id: b.ToolUseID, name: b.ToolName, input: b.ToolInput})
			case "tool_result":
				results[b.ToolUseID] = res{text: b.Text, isError: b.IsError}
			}
		}
	}
	recs := make([]toolRecord, 0, len(uses))
	for _, u := range uses {
		r, ok := results[u.id]
		if !ok {
			break
		}
		recs = append(recs, toolRecord{name: u.name, input: u.input, text: r.text, isError: r.isError})
	}
	return recs
}

// extractFinalText pulls the final_text out of run()'s return value. run() may
// return {final_text: "...", metadata?}, a bare string, or nothing — all map
// to a string. Must be called on the runtime goroutine.
func extractFinalText(rt *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	if obj, ok := v.(*goja.Object); ok {
		if ft := obj.Get("final_text"); ft != nil && !goja.IsUndefined(ft) && !goja.IsNull(ft) {
			return ft.String()
		}
		if b, err := v.ToObject(rt).MarshalJSON(); err == nil {
			return string(b)
		}
	}
	return v.String()
}
