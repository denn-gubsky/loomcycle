package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

const docTestB64 = "JVBERi0xLjcK"

// The PDF the transcription path cuts out of a kept file reaches the model as
// a `document` block, but only through Request.Documents, which nothing but
// that path sets: a document in a conversation message — what a user turn or
// any other content path would produce — is refused before the call.
func TestAnthropic_DocumentBlockOutOfBandOnly(t *testing.T) {
	if !New("k", "", streamhttp.Options{}, nil).Capabilities().SupportsDocumentInput {
		t.Fatal("the driver does not report SupportsDocumentInput")
	}

	// Out of band: the document comes first in the user message, then the
	// instruction.
	body, err := buildRequestBody(providers.Request{
		Model:     "claude-sonnet-4-6",
		Documents: []providers.DocumentInput{{MediaType: "application/pdf", Data: docTestB64}},
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "transcribe"}}}},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
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
		t.Fatalf("message shape: %s", body)
	}
	doc := w.Messages[0].Content[0]
	if doc.Type != "document" || doc.Source == nil || doc.Source.Type != "base64" ||
		doc.Source.MediaType != "application/pdf" || doc.Source.Data != docTestB64 {
		t.Fatalf("first block is not the PDF document: %s", body)
	}
	if c := w.Messages[0].Content[1]; c.Type != "text" || c.Text != "transcribe" {
		t.Fatalf("second block is not the instruction: %s", body)
	}

	// In band: a document block inside a message is refused, wherever it is.
	for _, role := range []string{"user", "assistant"} {
		_, err := buildRequestBody(providers.Request{
			Model: "claude-sonnet-4-6",
			Messages: []providers.Message{{Role: role, Content: []providers.ContentBlock{
				{Type: "text", Text: "read this"},
				{Type: "document", MediaType: "application/pdf", Data: docTestB64},
			}}},
		})
		if err == nil || !strings.Contains(err.Error(), "document") {
			t.Errorf("a %s message carrying a document block: err = %v, want a refusal", role, err)
		}
	}

	// Only a PDF, and only with a user message to carry it.
	if _, err := buildRequestBody(providers.Request{
		Model:     "claude-sonnet-4-6",
		Documents: []providers.DocumentInput{{MediaType: "text/html", Data: docTestB64}},
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
	}); err == nil {
		t.Error("a non-PDF document was accepted")
	}
	if _, err := buildRequestBody(providers.Request{
		Model:     "claude-sonnet-4-6",
		Documents: []providers.DocumentInput{{MediaType: "application/pdf", Data: docTestB64}},
	}); err == nil {
		t.Error("a document with no user message to carry it was accepted")
	}
}
