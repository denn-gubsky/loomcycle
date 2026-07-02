package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestRequestBody_ReplaysThinkingBlockOnToolContinuation is the regression for
// the Anthropic thinking+tools 400: with extended thinking enabled (effort) and
// a tool call, the prior assistant turn must be replayed STARTING with its
// thinking block (text + signature) or the continuation 400s with "a final
// assistant message must start with a thinking block". The loop now carries the
// block on Message.Reasoning + ReasoningSignature; buildRequestBody must emit it
// first in that assistant message's content.
func TestRequestBody_ReplaysThinkingBlockOnToolContinuation(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model:  "claude-sonnet-4-6",
		Effort: "high", // thinking stays enabled on the continuation request
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "do a thing"}}},
			{
				Role:               "assistant",
				Reasoning:          "let me reason about this",
				ReasoningSignature: "SIG_abc123",
				Content: []providers.ContentBlock{
					{Type: "tool_use", ToolUseID: "tu_1", ToolName: "Read", ToolInput: json.RawMessage(`{"path":"x"}`)},
				},
			},
			{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: "tu_1", Text: "file contents"}}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(parsed.Messages))
	}
	asst := parsed.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("messages[1] role = %q, want assistant", asst.Role)
	}
	// MUST start with the thinking block, seal included.
	if len(asst.Content) == 0 || asst.Content[0].Type != "thinking" {
		t.Fatalf("assistant turn must START with a thinking block; got %+v", asst.Content)
	}
	if asst.Content[0].Thinking != "let me reason about this" || asst.Content[0].Signature != "SIG_abc123" {
		t.Errorf("thinking block = %+v, want the text + signature echoed verbatim", asst.Content[0])
	}
	// The tool_use block still follows it.
	if len(asst.Content) < 2 || asst.Content[1].Type != "tool_use" {
		t.Errorf("tool_use must follow the thinking block; got %+v", asst.Content)
	}
}

// TestRequestBody_NoThinkingBlockWithoutSignature: a bare/unsigned thinking
// block 400s differently, so the driver only replays a block when BOTH the text
// and the signature are present.
func TestRequestBody_NoThinkingBlockWithoutSignature(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model:  "claude-sonnet-4-6",
		Effort: "high",
		Messages: []providers.Message{
			{Role: "assistant", Reasoning: "reasoned but no seal",
				Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	if strings.Contains(string(body), `"thinking":"reasoned but no seal"`) {
		t.Errorf("must NOT replay a thinking block without its signature:\n%s", body)
	}
}

// TestProcessFrame_CapturesThinkingAndSignature: the driver accumulates the
// thinking text (thinking_delta) and captures the seal (signature_delta) so
// EventDone can carry them for the next-turn replay.
func TestProcessFrame_CapturesThinkingAndSignature(t *testing.T) {
	var current pendingBlock
	var stop, model string
	var usage *providers.Usage
	var reasoning strings.Builder
	var sig string
	sink := func(providers.Event) bool { return true }

	feed := func(event, data string) {
		processFrame(sseFrame{event: event, data: []byte(data)},
			&current, &stop, &model, &usage, &reasoning, &sig, sink)
	}
	feed("content_block_delta", `{"delta":{"type":"thinking_delta","thinking":"step one "}}`)
	feed("content_block_delta", `{"delta":{"type":"thinking_delta","thinking":"step two"}}`)
	feed("content_block_delta", `{"delta":{"type":"signature_delta","signature":"SEAL_xyz"}}`)

	if got := reasoning.String(); got != "step one step two" {
		t.Errorf("accumulated thinking = %q, want %q", got, "step one step two")
	}
	if sig != "SEAL_xyz" {
		t.Errorf("captured signature = %q, want SEAL_xyz", sig)
	}
}
