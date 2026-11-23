package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func toolImageRequest(model string) providers.Request {
	return providers.Request{
		Model: model,
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "draw it"}}},
			{Role: "assistant", Content: []providers.ContentBlock{
				{Type: "tool_use", ToolUseID: "call_a", ToolName: "Workspace", ToolInput: json.RawMessage(`{}`)},
				{Type: "tool_use", ToolUseID: "call_b", ToolName: "Read", ToolInput: json.RawMessage(`{}`)},
			}},
			{Role: "user", Content: []providers.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_a", ToolName: "Workspace", Text: "a chart",
					Images: []providers.ImagePart{{MediaType: "image/png", Data: visTestB64}}},
				{Type: "tool_result", ToolUseID: "call_b", ToolName: "Read", Text: "file text"},
			}},
		},
	}
}

// TestToolImage_OpenAIFollowsToolMessagesWithUserImage: a tool message cannot
// carry an image, so the picture comes in one user message AFTER every tool
// message of the turn (tool messages must follow the assistant turn directly),
// naming the call it belongs to.
func TestToolImage_OpenAIFollowsToolMessagesWithUserImage(t *testing.T) {
	body, err := buildRequestBody(toolImageRequest("gpt-4o"))
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	msgs := decodeWireMessages(t, body)
	var roles []string
	for _, m := range msgs {
		var r string
		_ = json.Unmarshal(m["role"], &r)
		roles = append(roles, r)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,tool,user" {
		t.Fatalf("roles = %s, want user,assistant,tool,tool,user", got)
	}
	var toolText string
	if err := json.Unmarshal(msgs[2]["content"], &toolText); err != nil || toolText != "a chart" {
		t.Errorf("tool message content = %s, want the text as a string", msgs[2]["content"])
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(msgs[4]["content"], &parts); err != nil {
		t.Fatalf("trailing user content is not an array: %s", msgs[4]["content"])
	}
	if len(parts) != 2 || parts[0].Type != "text" || !strings.Contains(parts[0].Text, "call_a") {
		t.Fatalf("want [text naming call_a, image_url], got %s", msgs[4]["content"])
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,"+visTestB64 {
		t.Errorf("image_url wrong: %s", msgs[4]["content"])
	}
}

// TestToolImage_OpenAIRefusesTextOnlyModel: the per-model refusal counts a
// picture inside a tool result, and the driver answers the loop's question.
func TestToolImage_OpenAIRefusesTextOnlyModel(t *testing.T) {
	d := &Driver{}
	if d.SupportsVisionFor("gpt-3.5-turbo") || !d.SupportsVisionFor("gpt-4o") {
		t.Error("SupportsVisionFor does not follow openaiSupportsVision")
	}
}
