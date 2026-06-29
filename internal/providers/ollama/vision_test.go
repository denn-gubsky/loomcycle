package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

const visTestB64 = "iVBORw0KGgo="

// TestVision_OllamaImagesField: an image content block on a user message
// serializes to Ollama's message.images: [base64] alongside the text content.
func TestVision_OllamaImagesField(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"llava","message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model: "llava",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "describe this"},
			{Type: "image", MediaType: "image/png", Data: visTestB64},
		}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}

	var w struct {
		Messages []struct {
			Role    string   `json:"role"`
			Content string   `json:"content"`
			Images  []string `json:"images"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured, &w); err != nil {
		t.Fatalf("decode body: %v (%s)", err, captured)
	}
	var user *struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Images  []string `json:"images"`
	}
	for i := range w.Messages {
		if w.Messages[i].Role == "user" {
			user = &w.Messages[i]
		}
	}
	if user == nil {
		t.Fatalf("no user message in body: %s", captured)
	}
	if user.Content != "describe this" {
		t.Errorf("content = %q, want %q", user.Content, "describe this")
	}
	if len(user.Images) != 1 || user.Images[0] != visTestB64 {
		t.Errorf("images = %v, want [%q]", user.Images, visTestB64)
	}
}
