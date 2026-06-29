package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const visTestB64 = "iVBORw0KGgo="

// TestVision_OpenAIImageUsesContentArray: a user message with an image
// serializes to the content-array form — a text part plus an image_url part
// whose url is the inline data-URI the driver builds from media_type + base64.
func TestVision_OpenAIImageUsesContentArray(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "describe"},
			{Type: "image", MediaType: "image/png", Data: visTestB64},
		}}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	user := findByRole(t, decodeWireMessages(t, body), "user")
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(user["content"], &parts); err != nil {
		t.Fatalf("user content is not an array: %s (%v)", user["content"], err)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 content parts, got %d: %s", len(parts), user["content"])
	}
	if parts[0].Type != "text" || parts[0].Text != "describe" {
		t.Errorf("part 0 not the text part: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("part 1 not an image_url part: %+v", parts[1])
	}
	if want := "data:image/png;base64," + visTestB64; parts[1].ImageURL.URL != want {
		t.Errorf("image_url.url = %q, want %q", parts[1].ImageURL.URL, want)
	}
}

// TestVision_TextOnlyUserKeepsFlatString: a text-only user message keeps the
// flat string content form — the pre-vision path is unchanged (no regression
// to the content-array form for non-image turns).
func TestVision_TextOnlyUserKeepsFlatString(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "hello"},
		}}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	user := findByRole(t, decodeWireMessages(t, body), "user")
	if string(user["content"]) != `"hello"` {
		t.Errorf("text-only content = %s, want flat string \"hello\"", user["content"])
	}
}

// TestVision_OpenAIRejectsImageOnTextOnlyModel: a known text-only model is
// refused before the call with a clear error.
func TestVision_OpenAIRejectsImageOnTextOnlyModel(t *testing.T) {
	for _, model := range []string{"gpt-3.5-turbo", "gpt-4", "gpt-4-32k"} {
		_, err := buildRequestBody(providers.Request{
			Model: model,
			Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{
				{Type: "image", MediaType: "image/png", Data: visTestB64},
			}}},
		})
		if err == nil || !strings.Contains(err.Error(), "does not support image input") {
			t.Errorf("model %q: want image-refusal error, got %v", model, err)
		}
	}
}

func TestOpenAISupportsVision(t *testing.T) {
	vision := []string{"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-4.1", "gpt-5", "gpt-5.4-mini", "o3", "gpt-4-0125-preview"}
	textOnly := []string{"gpt-3.5-turbo", "gpt-3.5-turbo-0613", "gpt-4", "gpt-4-0314", "gpt-4-0613", "gpt-4-32k"}
	for _, m := range vision {
		if !openaiSupportsVision(m) {
			t.Errorf("openaiSupportsVision(%q) = false, want true", m)
		}
	}
	for _, m := range textOnly {
		if openaiSupportsVision(m) {
			t.Errorf("openaiSupportsVision(%q) = true, want false", m)
		}
	}
}
