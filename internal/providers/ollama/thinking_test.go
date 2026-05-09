package ollama

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestStreamThinking_EmitsLiveEventThinking pins the v0.7.x
// EventThinking contract for the Ollama driver. Reasoning models
// (qwen3, deepseek-r1, hermes3) surface their reasoning trace in
// `message.thinking` separate from `message.content`. Pre-fix the
// driver only consumed `content`, silently dropping the thinking
// stream — operators paid for the tokens (visible in
// prompt_eval_count / eval_count) without visibility into what was
// thought.
func TestStreamThinking_EmitsLiveEventThinking(t *testing.T) {
	// Mimics qwen3:14b's wire shape: thinking and content interleave
	// across frames; the final frame closes both.
	frames := []string{
		`{"model":"qwen3:14b","message":{"role":"assistant","thinking":"The user asks for the answer. ","content":""},"done":false}` + "\n",
		`{"model":"qwen3:14b","message":{"role":"assistant","thinking":"Computing: 6×7=42.","content":""},"done":false}` + "\n",
		`{"model":"qwen3:14b","message":{"role":"assistant","thinking":"","content":"42"},"done":false}` + "\n",
		`{"model":"qwen3:14b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":50,"eval_count":20}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "qwen3:14b",
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
		}
	}

	if thinkingEvents != 2 {
		t.Errorf("EventThinking count = %d, want 2 (one per non-empty thinking frame)", thinkingEvents)
	}
	wantThinking := "The user asks for the answer. Computing: 6×7=42."
	if thinking.String() != wantThinking {
		t.Errorf("EventThinking concat = %q, want %q", thinking.String(), wantThinking)
	}
	if textEvents != 1 || text.String() != "42" {
		t.Errorf("EventText: count=%d text=%q, want 1×%q (thinking must NOT bleed into the user-visible answer)",
			textEvents, text.String(), "42")
	}
}

// TestStreamThinking_NotEmittedOnNonThinkingModel guards against a
// regression where a frame without a `thinking` field still fires
// EventThinking (e.g. via a default-zero-value bug). Plain
// content-only Ollama streams must stay clean.
func TestStreamThinking_NotEmittedOnNonThinkingModel(t *testing.T) {
	frames := []string{
		`{"model":"llama3.1","message":{"role":"assistant","content":"hello "},"done":false}` + "\n",
		`{"model":"llama3.1","message":{"role":"assistant","content":"world"},"done":false}` + "\n",
		`{"model":"llama3.1","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":2}` + "\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New(srv.URL, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "llama3.1",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})

	for ev := range ch {
		if ev.Type == providers.EventThinking {
			t.Errorf("EventThinking emitted on non-thinking-model stream: text=%q", ev.Text)
		}
	}
}
