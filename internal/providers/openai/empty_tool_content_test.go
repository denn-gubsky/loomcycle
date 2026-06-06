package openai

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// RFC Q / finding F10: the openai-compat driver must always serialize
// `content` on a role:"tool" message, even when the tool produced empty
// output. OpenAI tolerates an omitted content; DeepSeek's stricter
// deserializer 400s with "missing field content", which broke every
// tool-using DeepSeek agent the moment any tool returned empty stdout
// (a silent `mkdir`, a script that only writes files).

func decodeWireMessages(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var w struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal request body: %v (body: %s)", err, body)
	}
	return w.Messages
}

func findByRole(t *testing.T, msgs []map[string]json.RawMessage, role string) map[string]json.RawMessage {
	t.Helper()
	for _, m := range msgs {
		var r string
		_ = json.Unmarshal(m["role"], &r)
		if r == role {
			return m
		}
	}
	t.Fatalf("no message with role=%q in %v", role, msgs)
	return nil
}

func TestToolMessage_EmptyResultStillSerializesContent(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "deepseek-v4-pro",
		Messages: []providers.Message{{
			Role: "user",
			Content: []providers.ContentBlock{
				// Empty tool output — the F10 trigger (e.g. `mkdir -p`).
				{Type: "tool_result", ToolUseID: "call_1", Text: ""},
			},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	tool := findByRole(t, decodeWireMessages(t, body), "tool")
	raw, ok := tool["content"]
	if !ok {
		t.Fatalf("role:tool omitted `content` on empty output — DeepSeek 400s. body: %s", body)
	}
	if string(raw) != `""` {
		t.Errorf("content = %s, want \"\"", raw)
	}
}

func TestToolMessage_NonEmptyResultRoundTrips(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "deepseek-v4-pro",
		Messages: []providers.Message{{
			Role: "user",
			Content: []providers.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Text: "done"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	tool := findByRole(t, decodeWireMessages(t, body), "tool")
	if string(tool["content"]) != `"done"` {
		t.Errorf("content = %s, want \"done\"", tool["content"])
	}
}

// The fix must NOT change the assistant path: an assistant message that
// carries only tool_calls (no text) still omits `content`, which both
// OpenAI and DeepSeek accept. Guards against a "drop omitempty globally"
// regression.
func TestAssistantToolCallsOnly_StillOmitsContent(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "deepseek-v4-pro",
		Messages: []providers.Message{{
			Role: "assistant",
			Content: []providers.ContentBlock{
				{Type: "tool_use", ToolUseID: "call_1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)},
			},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	asst := findByRole(t, decodeWireMessages(t, body), "assistant")
	if _, has := asst["content"]; has {
		t.Errorf("assistant-with-only-tool_calls should omit content, got: %s", asst["content"])
	}
}
