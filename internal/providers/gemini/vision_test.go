package gemini

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const visTestB64 = "iVBORw0KGgo="

// TestVision_GeminiInlineData: an image content block serializes to a Gemini
// part carrying inlineData {mimeType, data}.
func TestVision_GeminiInlineData(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "gemini-2.5-flash",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "what is this"},
			{Type: "image", MediaType: "image/jpeg", Data: visTestB64},
		}}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w struct {
		Contents []struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(w.Contents) != 1 || len(w.Contents[0].Parts) != 2 {
		t.Fatalf("unexpected content shape: %s", body)
	}
	img := w.Contents[0].Parts[1]
	if img.InlineData == nil {
		t.Fatalf("second part missing inlineData: %s", body)
	}
	if img.InlineData.MimeType != "image/jpeg" || img.InlineData.Data != visTestB64 {
		t.Errorf("inlineData wrong: %+v", img.InlineData)
	}
}
