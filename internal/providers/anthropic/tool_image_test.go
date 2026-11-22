package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// toolImageReq is a turn in which the model called a tool and the tool
// answered with text and a picture (RFC GW §5.8).
func toolImageReq(model string, text string) providers.Request {
	return providers.Request{
		Model: model,
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "draw it"}}},
			{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: "tu1", ToolName: "Workspace", ToolInput: json.RawMessage(`{"op":"view"}`)}}},
			{Role: "user", Content: []providers.ContentBlock{{
				Type: "tool_result", ToolUseID: "tu1", ToolName: "Workspace", Text: text,
				Images: []providers.ImagePart{{MediaType: "image/png", Data: visTestB64}},
			}}},
		},
	}
}

type wireToolResult struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

func lastToolResult(t *testing.T, body []byte) wireToolResult {
	t.Helper()
	var w struct {
		Messages []struct {
			Content []wireToolResult `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	last := w.Messages[len(w.Messages)-1].Content[0]
	if last.Type != "tool_result" {
		t.Fatalf("last block is %q, want tool_result: %s", last.Type, body)
	}
	return last
}

// TestToolImage_AnthropicNestsImageBlocksInToolResult: a picture in a tool
// result is sent inside the tool_result's content array, after its text.
func TestToolImage_AnthropicNestsImageBlocksInToolResult(t *testing.T) {
	body, err := buildRequestBody(toolImageReq("claude-sonnet-4-6", "a chart"))
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	tr := lastToolResult(t, body)
	var parts []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(tr.Content, &parts); err != nil {
		t.Fatalf("tool_result content is not an array: %s (%v)", tr.Content, err)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "a chart" {
		t.Fatalf("want [text, image], got %s", tr.Content)
	}
	if parts[1].Type != "image" || parts[1].Source == nil || parts[1].Source.Type != "base64" ||
		parts[1].Source.MediaType != "image/png" || parts[1].Source.Data != visTestB64 {
		t.Errorf("image block wrong: %s", tr.Content)
	}
}

// TestToolImage_AnthropicEmptyTextSendsOnlyImageBlocks: Anthropic refuses an
// empty text block, so a picture-only result has no text block.
func TestToolImage_AnthropicEmptyTextSendsOnlyImageBlocks(t *testing.T) {
	body, err := buildRequestBody(toolImageReq("claude-sonnet-4-6", ""))
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(lastToolResult(t, body).Content, &parts); err != nil {
		t.Fatalf("content not an array: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "image" {
		t.Errorf("want one image block, got %+v", parts)
	}
}

// TestToolImage_AnthropicTextOnlyResultKeepsString: a result without pictures
// keeps the string content form.
func TestToolImage_AnthropicTextOnlyResultKeepsString(t *testing.T) {
	req := toolImageReq("claude-sonnet-4-6", "plain")
	req.Messages[2].Content[0].Images = nil
	body, err := buildRequestBody(req)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	if got := string(lastToolResult(t, body).Content); got != `"plain"` {
		t.Errorf("content = %s, want \"plain\"", got)
	}
}

// TestToolImage_AnthropicRefusesTextOnlyModel: the per-model refusal counts a
// picture inside a tool result, and the driver answers the loop's question.
func TestToolImage_AnthropicRefusesTextOnlyModel(t *testing.T) {
	if _, err := buildRequestBody(toolImageReq("claude-2.1", "x")); err == nil {
		t.Error("want an error for a tool picture to claude-2.1")
	}
	d := &Driver{}
	if d.SupportsVisionFor("claude-2.1") || !d.SupportsVisionFor("claude-sonnet-4-6") {
		t.Error("SupportsVisionFor does not follow anthropicSupportsVision")
	}
}
