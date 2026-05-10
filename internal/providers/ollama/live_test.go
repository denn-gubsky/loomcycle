// Live integration test against a real Ollama server.
//
// Skipped unless OLLAMA_TEST_BASE_URL is set in the test environment,
// so `go test ./...` stays clean on machines without an Ollama
// reachable. The variable is intentionally distinct from
// OLLAMA_BASE_URL (which the loomcycle binary picks up at runtime)
// so an operator running tests on a machine that ALSO runs an
// Ollama service for production doesn't accidentally beat on it
// from the test suite.
//
// Run explicitly:
//
//   OLLAMA_TEST_BASE_URL=http://denn-desktop.local:11434 \
//   OLLAMA_TEST_MODEL=qwen3:14b \
//   go test -run TestLive_Ollama -v ./internal/providers/ollama/
//
// OLLAMA_TEST_MODEL defaults to "qwen3:14b" (the model recommended
// for an RTX 5080 / 16 GB VRAM tier — comfortable headroom at
// Q4_K_M, native tool calling, OpenAI-tools-compatible). Override
// for a different size or family. The model must be pulled on the
// remote (`ollama pull qwen3:14b`) before the test can run.

package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// liveBaseURL returns the test Ollama address from the env, or
// skips the test if not set. Centralised so the chat + tool-call
// tests share one skip path.
func liveBaseURL(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("OLLAMA_TEST_BASE_URL")
	if addr == "" {
		t.Skip("OLLAMA_TEST_BASE_URL not set; skipping live test")
	}
	return addr
}

// liveModel returns the model to test against. Defaulted to
// qwen3:14b — change via OLLAMA_TEST_MODEL.
func liveModel() string {
	if m := os.Getenv("OLLAMA_TEST_MODEL"); m != "" {
		return m
	}
	return "qwen3:14b"
}

// requireModelLoaded probes /api/tags and t.Skip's with a useful
// message if the requested model isn't pulled on the remote. The
// test would otherwise fail at /api/chat with a confusing
// "model not found" — the early skip is friendlier when running
// against a fresh Ollama install.
func requireModelLoaded(t *testing.T, baseURL, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Ollama unreachable at %s: %v", baseURL, err)
	}
	defer resp.Body.Close()
	var doc struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Skipf("could not decode /api/tags response: %v", err)
	}
	for _, m := range doc.Models {
		if m.Name == model {
			return
		}
	}
	names := make([]string, 0, len(doc.Models))
	for _, m := range doc.Models {
		names = append(names, m.Name)
	}
	t.Skipf("model %q not pulled on %s; available: %v\n"+
		"To pull it: ollama pull %s", model, baseURL, names, model)
}

// TestLive_OllamaChatCompletion exercises the basic streaming
// chat round-trip end-to-end. Verifies:
//   - the NDJSON wire is parsed correctly
//   - text comes back
//   - the final "done":true frame's prompt_eval_count /
//     eval_count populate Usage.InputTokens / OutputTokens
//   - Usage.Model is set from the wire (not echoed from req.Model
//     — the Ollama driver captures the wire-resolved alias too,
//     same fix class as the v0.4 anthropic + v0.6 openai/deepseek
//     work)
func TestLive_OllamaChatCompletion(t *testing.T) {
	baseURL := liveBaseURL(t)
	model := liveModel()
	requireModelLoaded(t, baseURL, model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d := New(baseURL, streamhttp.Options{}, nil)

	// MaxTokens=512 gives reasoning models (qwen3, deepseek-r1, etc.)
	// budget for the <think>...</think> block AND the actual answer.
	// At MaxTokens=24, qwen3:14b consumes the entire budget inside an
	// unclosed reasoning block — Ollama puts that in message.thinking
	// (a separate wire field the driver doesn't currently surface),
	// so message.content stays empty and the test sees text="".
	// Non-reasoning models (llama3.x, mistral) also pass under 512;
	// the higher cap costs nothing on a local model.
	ch, err := d.Call(ctx, providers.Request{
		Model: model,
		Messages: []providers.Message{{
			Role: "user",
			Content: []providers.ContentBlock{
				{Type: "text", Text: "Reply with exactly one word: ping"},
			},
		}},
		MaxTokens: 512,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var text strings.Builder
	var sawDone bool
	var modelOnUsage string
	var inputTokens, outputTokens int
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventDone:
			sawDone = true
			if ev.Usage != nil {
				modelOnUsage = ev.Usage.Model
				inputTokens = ev.Usage.InputTokens
				outputTokens = ev.Usage.OutputTokens
			}
		case providers.EventError:
			t.Fatalf("provider error: %s", ev.Error)
		}
	}

	if !sawDone {
		t.Fatal("no EventDone received; stream ended unexpectedly")
	}
	if modelOnUsage == "" {
		t.Errorf("Usage.Model empty; runs.model column would not populate downstream")
	}
	if inputTokens == 0 || outputTokens == 0 {
		t.Errorf("token counters zero: input=%d output=%d (the final 'done' frame may not have carried prompt_eval_count / eval_count)",
			inputTokens, outputTokens)
	}
	if text.Len() == 0 {
		t.Errorf("no text response received from %s", model)
	}

	t.Logf("OK — model=%q input_tokens=%d output_tokens=%d text=%q",
		modelOnUsage, inputTokens, outputTokens, text.String())
}

// TestLive_OllamaToolCall exercises the tool-call round-trip.
// Tool-tuned models (qwen2.5+, llama3.1+, mistral-large, ...)
// emit structured tool_calls envelopes that the driver translates
// into EventToolCall.
//
// If this test fails on a model you expect to support tools, the
// most likely cause is that the model is NOT tool-tuned — the
// Ollama driver doesn't paper over this with prompt-engineering;
// it trusts the native API. See the package docstring at the top
// of driver.go for the reasoning.
func TestLive_OllamaToolCall(t *testing.T) {
	baseURL := liveBaseURL(t)
	model := liveModel()
	requireModelLoaded(t, baseURL, model)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	d := New(baseURL, streamhttp.Options{}, nil)

	ch, err := d.Call(ctx, providers.Request{
		Model: model,
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
	var toolInput string
	for ev := range ch {
		switch ev.Type {
		case providers.EventToolCall:
			sawToolCall = true
			if ev.ToolUse != nil {
				toolName = ev.ToolUse.Name
				toolInput = string(ev.ToolUse.Input)
			}
		case providers.EventError:
			t.Fatalf("provider error: %s", ev.Error)
		}
	}

	if !sawToolCall {
		t.Errorf("no EventToolCall received from %s; the model may not be tool-tuned. "+
			"Ollama silently drops the `tools` field for non-tuned models — see "+
			"internal/providers/ollama/driver.go package docstring", model)
	}
	if toolName != "get_weather" {
		t.Errorf("tool name = %q, want %q (model invented a different tool)", toolName, "get_weather")
	}
	if !strings.Contains(strings.ToLower(toolInput), "paris") {
		t.Errorf("tool input doesn't mention Paris: %s", toolInput)
	}

	t.Logf("OK — model=%q called tool %q with input %s", model, toolName, toolInput)
}

// TestLive_OllamaProbe is a one-shot health probe that lists what
// the remote server has. Useful as a separate quick sanity check
// before running the heavier chat / tool tests when iterating on a
// new setup. Skipped under the same gate as the other live tests.
func TestLive_OllamaProbe(t *testing.T) {
	baseURL := liveBaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s/api/tags: %v", baseURL, err)
	}
	defer resp.Body.Close()
	var doc struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			ModifiedAt string `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /api/tags: %v", err)
	}
	if len(doc.Models) == 0 {
		t.Fatalf("no models pulled on %s; run `ollama pull <model>` first", baseURL)
	}
	for _, m := range doc.Models {
		gb := float64(m.Size) / (1 << 30)
		fmt.Printf("  %-30s  %5.1f GiB  %s\n", m.Name, gb, m.ModifiedAt)
	}
	t.Logf("OK — %s has %d model(s) loaded", baseURL, len(doc.Models))
}
