// Live integration test against api.deepseek.com.
//
// Skipped unless DEEPSEEK_API_KEY is set in the test environment, so
// `go test ./...` stays clean on machines without a key. Run
// explicitly:
//
//   DEEPSEEK_API_KEY=sk-... go test -run TestLive_DeepSeek -v ./internal/providers/deepseek/
//
// The test uses MaxTokens=24 and a single short user message so the
// cost per run is well under a cent (DeepSeek-V3 chat is ~$0.27 /
// 1M output tokens at the time of writing).

package deepseek

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func TestLive_DeepSeekChatCompletion(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := New(key, "", nil)

	ch, err := d.Call(ctx, providers.Request{
		Model: "deepseek-chat",
		Messages: []providers.Message{{
			Role: "user",
			Content: []providers.ContentBlock{
				{Type: "text", Text: "Reply with exactly one word: ping"},
			},
		}},
		MaxTokens: 24,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var text strings.Builder
	var sawDone bool
	var sawUsage bool
	var modelOnUsage string
	var inputTokens, outputTokens int64
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventDone:
			sawDone = true
		case providers.EventUsage:
			sawUsage = true
			if ev.Usage != nil {
				modelOnUsage = ev.Usage.Model
				inputTokens = int64(ev.Usage.InputTokens)
				outputTokens = int64(ev.Usage.OutputTokens)
			}
		case providers.EventError:
			t.Fatalf("provider error: %s", ev.Error)
		}
	}

	if !sawDone {
		t.Errorf("no EventDone received; stream ended unexpectedly")
	}
	if !sawUsage {
		t.Errorf("no EventUsage received; runs.model column would not populate")
	}
	if modelOnUsage == "" {
		t.Errorf("Usage.Model empty; downstream cost-attribution would fail. usage seen=%v", sawUsage)
	}
	if inputTokens == 0 || outputTokens == 0 {
		t.Errorf("token counters zero: input=%d output=%d", inputTokens, outputTokens)
	}
	if text.Len() == 0 {
		t.Errorf("no text response received")
	}

	t.Logf("OK — model=%q input_tokens=%d output_tokens=%d text=%q",
		modelOnUsage, inputTokens, outputTokens, text.String())
}

// TestLive_DeepSeekToolCall — DeepSeek-V3 supports parallel tool
// calls via the OpenAI Chat Completions tool_calls envelope. This
// test issues a request with a single tool spec and verifies the
// model emits a tool_use event referencing it. Same gating as
// the chat-completion test.
func TestLive_DeepSeekToolCall(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := New(key, "", nil)

	ch, err := d.Call(ctx, providers.Request{
		Model: "deepseek-chat",
		Messages: []providers.Message{{
			Role: "user",
			Content: []providers.ContentBlock{
				{Type: "text", Text: "Use the get_weather tool to find the weather in Paris."},
			},
		}},
		Tools: []providers.ToolSpec{{
			Name:        "get_weather",
			Description: "Get current weather for a city.",
			InputSchema: []byte(`{
				"type": "object",
				"properties": {
					"city": {"type": "string", "description": "City name"}
				},
				"required": ["city"]
			}`),
		}},
		MaxTokens: 256,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var sawToolCall bool
	var toolName string
	for ev := range ch {
		switch ev.Type {
		case providers.EventToolCall:
			sawToolCall = true
			if ev.ToolUse != nil {
				toolName = ev.ToolUse.Name
			}
		case providers.EventError:
			t.Fatalf("provider error: %s", ev.Error)
		}
	}

	if !sawToolCall {
		t.Errorf("no EventToolCall received; DeepSeek did not emit a tool_calls envelope")
	}
	if toolName != "get_weather" {
		t.Errorf("tool name = %q, want %q", toolName, "get_weather")
	}

	t.Logf("OK — model called tool %q", toolName)
}
