package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestToolImage_GeminiPartsFollowFunctionResponses: a tool's picture goes in
// the same turn as the function responses, after all of them, led by a text
// part naming the call.
func TestToolImage_GeminiPartsFollowFunctionResponses(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "gemini-2.5-flash",
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "draw it"}}},
			{Role: "assistant", Content: []providers.ContentBlock{
				{Type: "tool_use", ToolUseID: "c1", ToolName: "Workspace", ToolInput: json.RawMessage(`{}`)},
				{Type: "tool_use", ToolUseID: "c2", ToolName: "Read", ToolInput: json.RawMessage(`{}`)},
			}},
			{Role: "user", Content: []providers.ContentBlock{
				{Type: "tool_result", ToolUseID: "c1", ToolName: "Workspace", Text: "a chart",
					Images: []providers.ImagePart{{MediaType: "image/webp", Data: visTestB64}}},
				{Type: "tool_result", ToolUseID: "c2", ToolName: "Read", Text: "file text"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text             string          `json:"text"`
				FunctionResponse json.RawMessage `json:"functionResponse"`
				InlineData       *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(w.Contents) != 3 {
		t.Fatalf("want 3 contents, got %d: %s", len(w.Contents), body)
	}
	parts := w.Contents[2].Parts
	if len(parts) != 4 || parts[0].FunctionResponse == nil || parts[1].FunctionResponse == nil {
		t.Fatalf("want [functionResponse, functionResponse, text, inlineData], got %s", body)
	}
	if !strings.Contains(parts[2].Text, "c1") {
		t.Errorf("lead text %q does not name the call", parts[2].Text)
	}
	if parts[3].InlineData == nil || parts[3].InlineData.MimeType != "image/webp" || parts[3].InlineData.Data != visTestB64 {
		t.Errorf("inlineData wrong: %s", body)
	}
}
