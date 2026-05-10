package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// Three tests pin the reasoning_content roundtrip — the contract
// DeepSeek V4 Pro / deepseek-reasoner enforces with a 400:
//
//   "The `reasoning_content` in the thinking mode must be passed
//    back to the API."
//
// Capture: streaming reasoning_content deltas accumulate into
// EventDone.Reasoning.
// Replay: an assistant Message carrying Reasoning emits the
// `reasoning_content` field on the next request body.
// No-thinking: vanilla streams without reasoning_content stay clean
// (the field is omitted from EventDone and from the wire body).

// TestReasoning_EmitsLiveEventThinking pins the v0.7.x EventThinking
// contract for the OpenAI driver: every reasoning_content delta on
// the wire must surface as an EventThinking event in addition to
// being accumulated for EventDone.Reasoning. Pre-fix, the driver
// only buffered — adapters could see the final consolidated trace
// but had no way to render it as it streamed.
func TestReasoning_EmitsLiveEventThinking(t *testing.T) {
	frames := []string{
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"reasoning_content":"Let me think... "}}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"reasoning_content":"the answer is 42."}}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"content":"42"}}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()
	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "deepseek-v4-pro",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})

	var thinking strings.Builder
	thinkingEvents := 0
	var done providers.Event
	for ev := range ch {
		if ev.Type == providers.EventThinking {
			thinkingEvents++
			thinking.WriteString(ev.Text)
		}
		if ev.Type == providers.EventDone {
			done = ev
		}
	}
	// Two reasoning deltas → two EventThinking events (no coalescing
	// for thinking; the per-delta cadence IS the contract).
	if thinkingEvents != 2 {
		t.Errorf("EventThinking count = %d, want 2", thinkingEvents)
	}
	want := "Let me think... the answer is 42."
	if thinking.String() != want {
		t.Errorf("EventThinking concat = %q, want %q", thinking.String(), want)
	}
	// EventDone.Reasoning still carries the consolidated trace —
	// EventThinking is additive, not a replacement, because the
	// loop's next-turn echo (DeepSeek roundtrip) reads from
	// EventDone.Reasoning.
	if done.Reasoning != want {
		t.Errorf("EventDone.Reasoning = %q, want %q (live emit must NOT break the buffered roundtrip)", done.Reasoning, want)
	}
}

func TestReasoning_CaptureAccumulatesAcrossDeltas(t *testing.T) {
	// Mimics DeepSeek V4 Pro: reasoning_content streams in chunks
	// alongside content. The driver should accumulate both into
	// the per-iteration buffers — content into EventText events,
	// reasoning into the internal accumulator surfaced on
	// EventDone.Reasoning.
	frames := []string{
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"reasoning_content":"Let me think... "}}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"reasoning_content":"the answer is 42."}}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"content":"42"}}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"model":"deepseek-v4-pro","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "deepseek-v4-pro",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var done providers.Event
	for ev := range ch {
		if ev.Type == providers.EventDone {
			done = ev
		}
	}
	want := "Let me think... the answer is 42."
	if done.Reasoning != want {
		t.Errorf("EventDone.Reasoning = %q, want %q", done.Reasoning, want)
	}
}

func TestReasoning_ReplayedToWireOnAssistantMessage(t *testing.T) {
	// An assistant Message with Reasoning set should serialise the
	// `reasoning_content` field in its wire form. Catches the
	// regression where the buildRequestBody helper drops the field
	// — exactly the bug that 400'd against DeepSeek in production.
	body, err := buildRequestBody(providers.Request{
		Model: "deepseek-v4-pro",
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			{
				Role:      "assistant",
				Content:   []providers.ContentBlock{{Type: "text", Text: "42"}},
				Reasoning: "Let me think... the answer is 42.",
			},
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "explain"}}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// messages[0] = user, messages[1] = assistant, messages[2] = user
	asst := w.Messages[1]
	if asst["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", asst["role"])
	}
	got, ok := asst["reasoning_content"].(string)
	if !ok {
		t.Fatalf("messages[1].reasoning_content missing or wrong type, body: %s", body)
	}
	if got != "Let me think... the answer is 42." {
		t.Errorf("reasoning_content = %q, want %q", got, "Let me think... the answer is 42.")
	}
}

func TestReasoning_OmittedWhenAssistantHasNoReasoning(t *testing.T) {
	// Non-thinking models never set Reasoning on their assistant
	// turns. The wire body must omit reasoning_content entirely
	// (omitempty) — a present empty-string field would still be
	// echoed, and some strict OpenAI-compatible endpoints might
	// reject the unknown empty field.
	body, err := buildRequestBody(providers.Request{
		Model: "gpt-5.4",
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "again"}}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(body, &w)
	asst := w.Messages[1]
	if _, has := asst["reasoning_content"]; has {
		t.Errorf("assistant message without Reasoning should omit reasoning_content from wire, got %v", asst["reasoning_content"])
	}
}
