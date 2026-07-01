package loop

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// thinkingProvider streams a thinking chunk, then text, then done — mirroring a
// reasoning model (Ollama message.thinking / Anthropic thinking_delta /
// DeepSeek reasoning_content) that surfaces its trace out-of-band.
type thinkingProvider struct{}

func (thinkingProvider) ID() string                                   { return "thinking-llm" }
func (thinkingProvider) Probe(context.Context) error                  { return nil }
func (thinkingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (thinkingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsThinking: true}
}
func (thinkingProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 4)
	ch <- providers.Event{Type: providers.EventThinking, Text: "Let me work through this: 40 / 0.8 = 50."}
	ch <- providers.Event{Type: providers.EventText, Text: "The original price was $50."}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Reasoning: "Let me work through this: 40 / 0.8 = 50.", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// TestRun_ForwardsEventThinking is the regression for the loop dropping the
// reasoning trace: the loop's event switch had no EventThinking case (and no
// default), so a driver's streamed thinking never reached the caller's OnEvent
// — no client (SSE/gRPC/adapter) could render it, for any provider. Fail-before:
// the switch ignores EventThinking, so `events` contains text/done but no
// thinking, and this assertion fails.
func TestRun_ForwardsEventThinking(t *testing.T) {
	var events []providers.Event
	_, err := Run(context.Background(), RunOptions{
		Provider:   thinkingProvider{},
		Model:      "reasoner",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "shirt costs $40 after 20% off; original?"}}}},
		OnEvent:    func(ev providers.Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var thinking, text int
	var firstThinking string
	for _, ev := range events {
		switch ev.Type {
		case providers.EventThinking:
			thinking++
			if firstThinking == "" {
				firstThinking = ev.Text
			}
		case providers.EventText:
			text++
		}
	}
	if thinking == 0 {
		t.Fatalf("EventThinking was not forwarded to OnEvent — the loop dropped the reasoning trace; got events %+v", events)
	}
	if firstThinking == "" {
		t.Errorf("forwarded EventThinking carried no text")
	}
	if text == 0 {
		t.Errorf("expected the user-facing text to still be forwarded alongside thinking")
	}
}
