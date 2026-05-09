package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestStreamThinking_EmitsLiveEventThinking pins the v0.7.x
// EventThinking contract for the Anthropic driver. Extended-thinking
// blocks (claude-opus-4-7 / claude-sonnet-4-6 with `thinking:
// {type:enabled}`) stream their reasoning as content_block_delta
// frames with delta.type="thinking_delta" carrying a `thinking`
// payload. Pre-fix the driver only handled "text_delta" and
// "input_json_delta" — thinking deltas fell through the switch and
// were silently dropped.
func TestStreamThinking_EmitsLiveEventThinking(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-opus-4-7\"}}\n\n",
		// First content block: extended-thinking. Anthropic streams
		// the reasoning as a sequence of thinking_delta frames bracketed
		// by content_block_start (type=thinking) / content_block_stop.
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Let me reason. \"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"6×7 = 42.\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		// Second content block: visible answer text.
		"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"42\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":1}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "claude-opus-4-7",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "what is 6 times 7"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var thinking, text strings.Builder
	thinkingEvents, textEvents := 0, 0
	for ev := range ch {
		switch ev.Type {
		case providers.EventThinking:
			thinkingEvents++
			thinking.WriteString(ev.Text)
		case providers.EventText:
			textEvents++
			text.WriteString(ev.Text)
		case providers.EventError:
			t.Fatalf("unexpected error: %s", ev.Error)
		}
	}

	if thinkingEvents != 2 {
		t.Errorf("EventThinking count = %d, want 2 (one per thinking_delta)", thinkingEvents)
	}
	wantThinking := "Let me reason. 6×7 = 42."
	if thinking.String() != wantThinking {
		t.Errorf("EventThinking concat = %q, want %q", thinking.String(), wantThinking)
	}
	if textEvents != 1 || text.String() != "42" {
		t.Errorf("EventText: count=%d text=%q, want 1×%q (thinking must NOT bleed into the user-visible answer)",
			textEvents, text.String(), "42")
	}
}

// TestStreamThinking_NotEmittedOnPlainTextStream guards against a
// regression where thinking_delta parsing accidentally fires on
// text_delta frames (e.g. a switch-case fall-through). A vanilla
// text-only stream must produce zero EventThinking events.
func TestStreamThinking_NotEmittedOnPlainTextStream(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-haiku-4-5\"}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "claude-haiku-4-5",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})

	for ev := range ch {
		if ev.Type == providers.EventThinking {
			t.Errorf("EventThinking emitted on plain-text stream: text=%q", ev.Text)
		}
	}
}
