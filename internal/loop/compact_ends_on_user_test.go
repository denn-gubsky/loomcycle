package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// singleTurnToolLoop is one long turn: the task, then rounds of tool calls and
// their results, ending on the results the next model call is about to read.
// Every assistant turn carries reasoning, as a thinking model's does. It has no
// fresh user turn after the task, so a compaction has nowhere to cut and keeps
// no tail.
func singleTurnToolLoop(rounds int) []providers.Message {
	msgs := []providers.Message{{Role: "user",
		Content: []providers.ContentBlock{{Type: "text", Text: "the task " + strings.Repeat("x", 400)}}}}
	for i := 0; i < rounds; i++ {
		msgs = append(msgs,
			providers.Message{Role: "assistant", Reasoning: "thinking",
				Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: "t", ToolName: "Noop", ToolInput: json.RawMessage(`{}`)}}},
			providers.Message{Role: "user",
				Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: "t", ToolName: "Noop", Text: strings.Repeat("result ", 200)}}})
	}
	return msgs
}

// A run that had been calling tools for one long turn failed on its first call
// after an automatic compaction: "The `reasoning_content` in the thinking mode
// must be passed back to the API". The compaction kept no tail, so the history
// became [summary, acknowledgement] and ended on an assistant turn the runtime
// wrote. Measured against deepseek-v4-pro with tools: that history is refused,
// and the same history followed by a user turn is accepted. The history a
// model call is about to read must end on a user turn.
func TestMaybeAutoCompact_MidTurnWithNoKeptTailEndsOnAUserTurn(t *testing.T) {
	msgs := singleTurnToolLoop(5)
	opts := RunOptions{
		Provider:   &steerProvider{}, // Call returns "ok" → the summary
		Model:      "x",
		Compaction: &config.Compaction{KeepLastN: cptr(4), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
	}
	out, did := maybeAutoCompact(context.Background(), opts, msgs, 0, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("expected the compaction to happen")
	}
	last := out[len(out)-1]
	if last.Role != "user" {
		t.Fatalf("compacted history ends on a %s turn; the next call would ask the model to continue the runtime's own message: %+v", last.Role, out)
	}
	if len(out) != 3 || out[1].Role != "assistant" || out[1].Content[0].Text != compactionAckText {
		t.Errorf("want [summary, acknowledgement, continue], got %d messages: %+v", len(out), out)
	}
	if last.Content[0].Text != compactionContinueText {
		t.Errorf("last turn = %q, want the continue instruction", last.Content[0].Text)
	}
}

func TestEndOnUserTurn(t *testing.T) {
	text := func(role, s string) providers.Message {
		return providers.Message{Role: role, Content: []providers.ContentBlock{{Type: "text", Text: s}}}
	}
	midTurn := singleTurnToolLoop(2)                                         // ends on tool results
	parked := []providers.Message{text("user", "q"), text("assistant", "a")} // ends on the answer
	noTail := CompactionMessages("task", "summary", nil)                     // ends on the acknowledgement
	withTail := CompactionMessages("task", "summary", []providers.Message{text("user", "q3")})
	recapNoTail := RecapMessages("task", "recap", nil) // ends on the recap note

	cases := []struct {
		name      string
		compacted []providers.Message
		before    []providers.Message
		wantAdded bool
	}{
		{"mid-turn compaction that kept no tail", noTail, midTurn, true},
		{"mid-turn recap that kept no tail", recapNoTail, midTurn, true},
		{"mid-turn compaction whose tail ends on a user turn", withTail, midTurn, false},
		{"a parked run's compaction: the operator's message is the next user turn", noTail, parked, false},
		{"nothing to replace", noTail, nil, false},
	}
	for _, tc := range cases {
		got := EndOnUserTurn(append([]providers.Message(nil), tc.compacted...), tc.before)
		added := len(got) == len(tc.compacted)+1
		if added != tc.wantAdded {
			t.Errorf("%s: user turn appended = %v, want %v", tc.name, added, tc.wantAdded)
			continue
		}
		if added && (got[len(got)-1].Role != "user" || got[len(got)-1].Content[0].Text != compactionContinueText) {
			t.Errorf("%s: appended turn = %+v", tc.name, got[len(got)-1])
		}
	}
}
