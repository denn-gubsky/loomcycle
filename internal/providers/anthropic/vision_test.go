package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const visTestB64 = "iVBORw0KGgo="

func imageReq(model string) providers.Request {
	return providers.Request{
		Model: model,
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "what is this"},
			{Type: "image", MediaType: "image/png", Data: visTestB64},
		}}},
	}
}

// TestVision_AnthropicImageBlockWireShape: an image content block serializes to
// Anthropic's {"type":"image","source":{"type":"base64","media_type","data"}}.
func TestVision_AnthropicImageBlockWireShape(t *testing.T) {
	body, err := buildRequestBody(imageReq("claude-sonnet-4-6"))
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w struct {
		Messages []struct {
			Content []struct {
				Type   string `json:"type"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(w.Messages) != 1 || len(w.Messages[0].Content) != 2 {
		t.Fatalf("unexpected message shape: %s", body)
	}
	img := w.Messages[0].Content[1]
	if img.Type != "image" || img.Source == nil {
		t.Fatalf("second block not an image source: %s", body)
	}
	if img.Source.Type != "base64" || img.Source.MediaType != "image/png" || img.Source.Data != visTestB64 {
		t.Errorf("image source wrong: %+v", img.Source)
	}
}

// TestVision_AnthropicRejectsImageOnTextOnlyModel: a legacy text-only family
// (claude-2 / claude-instant) is refused before the call with a clear error
// rather than an opaque provider 400.
func TestVision_AnthropicRejectsImageOnTextOnlyModel(t *testing.T) {
	for _, model := range []string{"claude-2.1", "claude-instant-1.2"} {
		if _, err := buildRequestBody(imageReq(model)); err == nil {
			t.Errorf("model %q: want error refusing image, got nil", model)
		}
	}
}

func TestAnthropicSupportsVision(t *testing.T) {
	vision := []string{"claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5", "claude-3-5-sonnet", "claude-3-opus"}
	textOnly := []string{"claude-2.1", "claude-2.0", "claude-instant-1.2"}
	for _, m := range vision {
		if !anthropicSupportsVision(m) {
			t.Errorf("anthropicSupportsVision(%q) = false, want true", m)
		}
	}
	for _, m := range textOnly {
		if anthropicSupportsVision(m) {
			t.Errorf("anthropicSupportsVision(%q) = true, want false", m)
		}
	}
}
