package gemini

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// The schema goes to responseJsonSchema (plain JSON Schema) with a JSON mime
// type — not to the deprecated OpenAPI-subset responseSchema.
func TestOutputFormat_SendsResponseJSONSchemaWithAJSONMimeType(t *testing.T) {
	b, err := buildRequestBody(providers.Request{
		Model:        "gemini-3-pro",
		Messages:     []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		OutputFormat: &providers.OutputFormat{Name: "a", Schema: json.RawMessage(`{"type":"object","properties":{"k":{"type":"string"}}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		GenerationConfig map[string]any `json:"generationConfig"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	gc := m.GenerationConfig
	if gc["responseMimeType"] != "application/json" || gc["responseJsonSchema"] == nil {
		t.Errorf("generationConfig = %v", gc)
	}
	if _, ok := gc["responseSchema"]; ok {
		t.Error("the deprecated responseSchema was sent")
	}
}

// Only the Gemini 3 line combines a schema with function calling.
func TestEnforcesStructuredOutput_WithToolsOnlyOnGemini3(t *testing.T) {
	d := New("k", "", streamhttp.Options{}, nil)
	for _, tc := range []struct {
		model    string
		hasTools bool
		want     bool
	}{
		{"gemini-2.5-flash", false, true},
		{"gemini-2.5-flash", true, false},
		{"gemini-3-pro-preview", true, true},
	} {
		if got := d.EnforcesStructuredOutput(tc.model, tc.hasTools); got != tc.want {
			t.Errorf("%s tools=%v: %v, want %v", tc.model, tc.hasTools, got, tc.want)
		}
	}
}
